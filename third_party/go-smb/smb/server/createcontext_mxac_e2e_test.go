package server_test

// 端到端验证 "MxAc" create context 的应答（MS-SMB2 §2.2.13.2.2 / §2.2.13.2.3）。
//
// 为什么值得一条 E2E：这个上下文之前被服务端整个忽略，而它恰好是 Apple
// 客户端判断「这个共享能不能写」的方式 —— 应答里少了那 4 字节 MaximalAccess，
// iOS 的「文件」App 就会把共享标成「只读」，「新建文件夹」直接变灰。

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/jfjallid/go-smb/ntlmssp"
	"github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"
	"github.com/jfjallid/go-smb/smb/server/memvfs"
	"github.com/jfjallid/go-smb/spnego"
)

// mxAcContextBlob 拼一个只含 "MxAc"（空 data）的 create context 列表。
// 布局按 MS-SMB2 §2.2.13.2：头 16 字节 + 名字 + 8 字节对齐的 data。
func mxAcContextBlob() []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint32(b[0:], 0)  // Next context = 0
	binary.LittleEndian.PutUint16(b[4:], 16) // NameOffset
	binary.LittleEndian.PutUint16(b[6:], 4)  // NameLength
	binary.LittleEndian.PutUint16(b[10:], 24) // DataOffset（对齐到 8）
	binary.LittleEndian.PutUint32(b[12:], 0)  // DataLength
	copy(b[16:], "MxAc")
	return b
}

// buildRootCreatePDU 手工拼一条「打开共享根」的 CREATE（MS-SMB2 §2.2.13）。
// ctxs 为 nil 时不带任何 create context。
func buildRootCreatePDU(sessionID uint64, treeID uint32, ctxs []byte) []byte {
	const bodyLen = 56
	total := 64 + bodyLen + len(ctxs)
	// 解码器要求至少 1 字节 Buffer（MS-SMB2 的 Create 请求体最短 57 字节）；
	// 无上下文时补 1 字节对齐真实客户端的行为。
	if len(ctxs) == 0 {
		total++
	}
	pdu := make([]byte, total)
	copy(pdu[0:4], []byte(smb.ProtocolSmb2))
	binary.LittleEndian.PutUint16(pdu[4:6], 64)          // header StructureSize
	binary.LittleEndian.PutUint16(pdu[6:8], 1)           // CreditCharge
	binary.LittleEndian.PutUint16(pdu[12:14], smb.CommandCreate)
	binary.LittleEndian.PutUint16(pdu[14:16], 1)  // Credits
	binary.LittleEndian.PutUint32(pdu[36:40], treeID)
	binary.LittleEndian.PutUint64(pdu[40:48], sessionID)

	b := pdu[64:]
	binary.LittleEndian.PutUint16(b[0:2], 57)          // body StructureSize
	binary.LittleEndian.PutUint32(b[4:8], 2)           // ImpersonationLevel = Impersonation
	binary.LittleEndian.PutUint32(b[24:28], 0x00000080) // DesiredAccess = FILE_READ_ATTRIBUTES
	binary.LittleEndian.PutUint32(b[32:36], 0x00000007) // ShareAccess = R|W|D
	binary.LittleEndian.PutUint32(b[36:40], 1)          // CreateDisposition = FILE_OPEN
	binary.LittleEndian.PutUint32(b[40:44], 0x00000001) // CreateOptions = FILE_DIRECTORY_FILE
	// NameOffset/NameLength 留 0：打开共享根本身没有名字
	if len(ctxs) > 0 {
		binary.LittleEndian.PutUint32(b[48:52], uint32(64+bodyLen))
		binary.LittleEndian.PutUint32(b[52:56], uint32(len(ctxs)))
		copy(pdu[64+bodyLen:], ctxs)
	}
	return pdu
}

// parseCreateContextsAt 从 CREATE 响应里取出 create context 列表。
// 返回 name -> data 的映射（够用，不追求通用）。
func parseCreateContextsAt(t *testing.T, resp []byte) map[string][]byte {
	t.Helper()
	if len(resp) < 64+88 {
		t.Fatalf("响应太短：%d 字节", len(resp))
	}
	body := resp[64:]
	off := int(binary.LittleEndian.Uint32(body[80:84]))
	length := int(binary.LittleEndian.Uint32(body[84:88]))
	out := map[string][]byte{}
	if off == 0 || length == 0 {
		return out
	}
	if off+length > len(resp) {
		t.Fatalf("create context 越界：off=%d len=%d 总长=%d", off, length, len(resp))
	}
	reg := resp[off : off+length]
	for pos := 0; pos+16 <= len(reg); {
		next := int(binary.LittleEndian.Uint32(reg[pos:]))
		nameOff := int(binary.LittleEndian.Uint16(reg[pos+4:]))
		nameLen := int(binary.LittleEndian.Uint16(reg[pos+6:]))
		dataOff := int(binary.LittleEndian.Uint16(reg[pos+10:]))
		dataLen := int(binary.LittleEndian.Uint32(reg[pos+12:]))
		name := string(reg[pos+nameOff : pos+nameOff+nameLen])
		var data []byte
		if dataLen > 0 {
			data = reg[pos+dataOff : pos+dataOff+dataLen]
		}
		out[name] = data
		if next == 0 {
			break
		}
		pos += next
	}
	return out
}

func startMxAcServer(t *testing.T) (port int, shutdown func(), user, pass, share string) {
	t.Helper()
	user, pass, share = "alice", "Hunter2!", "files"
	srv := &server.Server{
		Config: &server.ServerConfig{
			Authenticator: &server.MapAuthenticator{
				Domain:   "WORKGROUP",
				Accounts: map[string]*server.Account{user: {NTHash: ntlmssp.Ntowfv1(pass)}},
			},
		},
	}
	sh := server.Share{Type: smb.ShareTypeDisk, VFS: memvfs.New(memvfs.Options{})}
	sh.MaximalAccess = 0x001f01ff
	srv.RegisterShare(share, sh)

	addr, stop := startTestServer(t, srv)
	return addr.Port, stop, user, pass, share
}

func dialMxAc(t *testing.T, port int, user, pass, share string) *smb.Connection {
	t.Helper()
	c, err := smb.NewConnection(smb.Options{
		Host:        "127.0.0.1",
		Port:        port,
		Initiator:   &spnego.NTLMInitiator{User: user, Password: pass, Domain: "WORKGROUP"},
		DialTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewConnection: %v", err)
	}
	t.Cleanup(c.Close)
	if err := c.TreeConnect(share); err != nil {
		t.Fatalf("TreeConnect: %v", err)
	}
	return c
}

// 核心用例：客户端问 MxAc，服务端必须回 8 字节，且 MaximalAccess 含写权限。
func TestCreateMxAcContextAnswered(t *testing.T) {
	port, stop, user, pass, share := startMxAcServer(t)
	defer stop()
	c := dialMxAc(t, port, user, pass, share)

	pdu := buildRootCreatePDU(c.SessionID(), c.TreeID(share), mxAcContextBlob())
	resp, err := c.SendRawPDU(pdu)
	if err != nil {
		t.Fatalf("SendRawPDU: %v", err)
	}
	if status := binary.LittleEndian.Uint32(resp[8:12]); status != smb.StatusOk {
		t.Fatalf("CREATE status = 0x%08x，应为 STATUS_SUCCESS", status)
	}

	ctxs := parseCreateContextsAt(t, resp)
	data, ok := ctxs["MxAc"]
	if !ok {
		t.Fatalf("响应里没有 MxAc 上下文，实际有：%v", keysOf(ctxs))
	}
	if len(data) != 8 {
		t.Fatalf("MxAc 应答体 %d 字节，应为 8", len(data))
	}
	if qs := binary.LittleEndian.Uint32(data[0:]); qs != 0 {
		t.Errorf("QueryStatus = 0x%08x，应为 0", qs)
	}
	ma := binary.LittleEndian.Uint32(data[4:])
	if ma != 0x001f01ff {
		t.Errorf("MaximalAccess = 0x%08x，应为 0x001f01ff", ma)
	}
	if ma&0x00000002 == 0 { // FILE_WRITE_DATA
		t.Errorf("MaximalAccess 0x%08x 不含 FILE_WRITE_DATA，客户端会认为只读", ma)
	}
}

// 反向用例：没问就别答，避免给不认识的上下文乱造数据。
func TestCreateWithoutMxAcHasNoContexts(t *testing.T) {
	port, stop, user, pass, share := startMxAcServer(t)
	defer stop()
	c := dialMxAc(t, port, user, pass, share)

	resp, err := c.SendRawPDU(buildRootCreatePDU(c.SessionID(), c.TreeID(share), nil))
	if err != nil {
		t.Fatalf("SendRawPDU: %v", err)
	}
	if status := binary.LittleEndian.Uint32(resp[8:12]); status != smb.StatusOk {
		t.Fatalf("CREATE status = 0x%08x，应为 STATUS_SUCCESS", status)
	}
	if ctxs := parseCreateContextsAt(t, resp); len(ctxs) != 0 {
		t.Errorf("没请求上下文却回了 %v", keysOf(ctxs))
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ = context.Background // 与同目录其他测试保持相同的导入形态
