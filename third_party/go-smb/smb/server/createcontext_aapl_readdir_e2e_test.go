package server_test

// 端到端验证：AAPL READ_DIR_ATTR 协商后的目录枚举必须带 Apple 的
// readdir 属性块（Samba smb2_trans2.c 口径）。
//
// 没有协商时不带；协商后每个 FileIdBothDirectoryInformation 条目的
// ShortName 区改写为：
//
//	ShortNameLength(u16) = 24
//	rfork_size (u64)     = 0
//	finder_info(16 字节) = 0
//
// 且 EaSize = MaximalAccess。ShortNameLength 留 0 会让 iOS 的 FileProvider
// 把每个条目解析成损坏 —— 图标全灰、只剩删除按钮，进不去子目录。

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/jfjallid/go-smb/ntlmssp"
	"github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"
	"github.com/jfjallid/go-smb/smb/server/filevfs"
)

func startAaplDirServer(t *testing.T) (port int, shutdown func(), user, pass, share string) {
	t.Helper()
	user, pass, share = "alice", "Hunter2!", "files"
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs, err := filevfs.New(filevfs.Options{Root: dir})
	if err != nil {
		t.Fatalf("filevfs.New: %v", err)
	}
	srv := &server.Server{
		Config: &server.ServerConfig{
			Authenticator: &server.MapAuthenticator{
				Domain:   "WORKGROUP",
				Accounts: map[string]*server.Account{user: {NTHash: ntlmssp.Ntowfv1(pass)}},
			},
		},
	}
	sh := server.Share{Type: smb.ShareTypeDisk, VFS: fs}
	sh.MaximalAccess = 0x001f01ff
	srv.RegisterShare(share, sh)

	addr, stop := startTestServer(t, srv)
	return addr.Port, stop, user, pass, share
}

// buildQueryDirectoryPDU 手工拼一条 QUERY_DIRECTORY（MS-SMB2 §2.2.33）。
// fileID 为已打开目录句柄的 16 字节 FileId，名字留空表示继续该句柄的枚举。
func buildQueryDirectoryPDU(sessionID uint64, treeID uint32, fileID []byte, class byte) []byte {
	const bodyLen = 32
	pdu := make([]byte, 64+bodyLen)
	copy(pdu[0:4], []byte(smb.ProtocolSmb2))
	binary.LittleEndian.PutUint16(pdu[4:6], 64)
	binary.LittleEndian.PutUint16(pdu[6:8], 1)
	binary.LittleEndian.PutUint16(pdu[12:14], smb.CommandQueryDirectory)
	binary.LittleEndian.PutUint16(pdu[14:16], 1)
	binary.LittleEndian.PutUint32(pdu[36:40], treeID)
	binary.LittleEndian.PutUint64(pdu[40:48], sessionID)

	b := pdu[64:]
	binary.LittleEndian.PutUint16(b[0:2], 33)
	// 注意：本库（与实测各客户端一致）的口径是 class@2、flags@3。
	b[2] = class
	b[3] = 0
	copy(b[8:24], fileID)
	binary.LittleEndian.PutUint32(b[28:32], 64*1024) // OutputBufferLength
	return pdu
}

// openRootWithAapl 打开共享根（带 AAPL 协商），返回句柄的 16 字节 FileId。
func openRootWithAapl(t *testing.T, c *smb.Connection, share string) []byte {
	t.Helper()
	pdu := buildRootCreatePDU(c.SessionID(), c.TreeID(share), aaplContextBlob(aaplServerQueryData))
	resp, err := c.SendRawPDU(pdu)
	if err != nil {
		t.Fatalf("SendRawPDU CREATE: %v", err)
	}
	if status := binary.LittleEndian.Uint32(resp[8:12]); status != smb.StatusOk {
		t.Fatalf("CREATE status = 0x%08x", status)
	}
	// CREATE 响应体 88 字节，FileId 在 body 的 64..80（绝对 128..144）。
	fid := make([]byte, 16)
	copy(fid, resp[128:144])
	return fid
}

// queryDirRaw 发一条 QUERY_DIRECTORY 并返回 OutputBuffer 的原始字节。
func queryDirRaw(t *testing.T, c *smb.Connection, share string, fid []byte, class byte) []byte {
	t.Helper()
	resp, err := c.SendRawPDU(buildQueryDirectoryPDU(c.SessionID(), c.TreeID(share), fid, class))
	if err != nil {
		t.Fatalf("SendRawPDU QUERY_DIRECTORY: %v", err)
	}
	if status := binary.LittleEndian.Uint32(resp[8:12]); status != smb.StatusOk {
		t.Fatalf("QUERY_DIRECTORY status = 0x%08x", status)
	}
	body := resp[64:]
	off := int(binary.LittleEndian.Uint16(body[2:4]))
	length := int(binary.LittleEndian.Uint32(body[4:8]))
	return resp[off : off+length]
}

// 逐条目检查 Apple readdir 属性块。
func verifyAaplReaddirBlob(t *testing.T, reg []byte, wantSNL int, wantEa uint32) {
	t.Helper()
	pos, n := 0, 0
	for pos+104 <= len(reg) {
		next := int(binary.LittleEndian.Uint32(reg[pos:]))
		snl := int(binary.LittleEndian.Uint16(reg[pos+68:]))
		if snl != wantSNL {
			t.Errorf("条目 %d: ShortNameLength = %d，应为 %d", n, snl, wantSNL)
			return
		}
		if wantEa != 0 {
			if ea := binary.LittleEndian.Uint32(reg[pos+64:]); ea != wantEa {
				t.Errorf("条目 %d: EaSize = 0x%08x，应为 0x%08x", n, ea, wantEa)
			}
			// rfork_size + finder_info 必须全 0
			for i, b := range reg[pos+70 : pos+94] {
				if b != 0 {
					t.Errorf("条目 %d: 属性块字节 %d 非零", n, i)
					break
				}
			}
		}
		n++
		if next == 0 {
			break
		}
		pos += next
	}
	if n == 0 {
		t.Fatal("响应里没有解析出任何条目")
	}
}

// 核心用例：协商 AAPL 后，每条都必须带属性块，EaSize 变成 MaximalAccess。
func TestAaplReaddirAttrBlockPresent(t *testing.T) {
	port, stop, user, pass, share := startAaplDirServer(t)
	defer stop()
	c := dialMxAc(t, port, user, pass, share)

	fid := openRootWithAapl(t, c, share)
	reg := queryDirRaw(t, c, share, fid, smb.FileIdBothDirectoryInformation)
	verifyAaplReaddirBlob(t, reg, 24, 0x001f01ff)
}

// 反向用例：没协商时 ShortNameLength 必须是 0、EaSize 是 0，普通客户端不受影响。
func TestAaplReaddirAttrAbsentWithoutNegotiation(t *testing.T) {
	port, stop, user, pass, share := startAaplDirServer(t)
	defer stop()
	c := dialMxAc(t, port, user, pass, share)

	pdu := buildRootCreatePDU(c.SessionID(), c.TreeID(share), nil)
	resp, err := c.SendRawPDU(pdu)
	if err != nil {
		t.Fatalf("SendRawPDU: %v", err)
	}
	if status := binary.LittleEndian.Uint32(resp[8:12]); status != smb.StatusOk {
		t.Fatalf("CREATE status = 0x%08x", status)
	}
	fid := make([]byte, 16)
	copy(fid, resp[128:144])

	reg := queryDirRaw(t, c, share, fid, smb.FileIdBothDirectoryInformation)
	verifyAaplReaddirBlob(t, reg, 0, 0)
}
