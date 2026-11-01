package server

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/jfjallid/go-smb/smb/unicode"
)

// aaplReplyOf 只取应答上下文，忽略宣告的能力位。
func aaplReplyOf(t *testing.T, c *Conn, req []createContext) *createContext {
	t.Helper()
	ctx, _ := c.aaplReply(req)
	return ctx
}

// newAaplTestConn 造一个能安全调用 c.logger() 的最小连接。
func newAaplTestConn() *Conn {
	// connLogger 会解引用 RemoteAddr，必须给一个真的 net.Addr。
	return &Conn{
		Server:     &Server{Config: &ServerConfig{}},
		RemoteAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 445},
	}
}

// unicodeModel 返回 ModelInfo 段里应当出现的 UTF-16LE 字节。
func unicodeModel(t *testing.T) []byte {
	t.Helper()
	return unicode.ToUnicode(aaplModel())
}

// aaplReqFromIOS 是 2026-10-29 23:25:08 从 iPhone（iOS 26）抓到的真实
// "AAPL" create context 数据，24 字节：
//
//	cmd=1 (SERVER_QUERY), 保留=0, reqBitmap=7 (ServerCaps|VolumeCaps|ModelInfo),
//	clientCaps=0x1f
//
// 拿真实字节当夹具，是为了防止有人「顺手把字段顺序调合理一点」。
var aaplReqFromIOS = []byte{
	0x01, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00,
	0x07, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x1f, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
}

func aaplData(t *testing.T, ctx *createContext) []byte {
	t.Helper()
	if ctx == nil {
		t.Fatal("aaplReply returned nil, want a reply")
	}
	if ctx.Name != createTagAAPL {
		t.Fatalf("reply context name = %q, want %q", ctx.Name, createTagAAPL)
	}
	return ctx.Data
}

func TestAaplReplyIOSRequest(t *testing.T) {
	c := newAaplTestConn()
	req := []createContext{{Name: createTagAAPL, Data: aaplReqFromIOS}}
	data := aaplData(t, aaplReplyOf(t, c, req))

	// 头 16 + ServerCaps 8 + VolumeCaps 8 + ModelInfo(8 + len(model))
	model := unicodeModel(t)
	wantLen := 16 + 8 + 8 + 8 + len(model)
	if len(data) != wantLen {
		t.Fatalf("reply length = %d, want %d", len(data), wantLen)
	}

	if cmd := binary.LittleEndian.Uint32(data[0:]); cmd != aaplCmdServerQuery {
		t.Errorf("CommandCode = %d, want %d", cmd, aaplCmdServerQuery)
	}
	if rsv := binary.LittleEndian.Uint32(data[4:]); rsv != 0 {
		t.Errorf("reserved = %d, want 0", rsv)
	}
	if bm := binary.LittleEndian.Uint64(data[8:]); bm != 7 {
		t.Errorf("echoed reqBitmap = 0x%x, want 0x7", bm)
	}

	serverCaps := binary.LittleEndian.Uint64(data[16:])
	// 客户端声明了 READ_DIR_ATTR 且我们的 fsAttrBits() 含 FILE_NAMED_STREAMS，
	// 所以必须回 READ_DIR_ATTR —— 这一位是 iOS 判定「可以写」的关键。
	if serverCaps&aaplCapUnixBased == 0 {
		t.Errorf("serverCaps = 0x%x, missing UNIX_BASED", serverCaps)
	}
	if serverCaps&aaplCapSupportsReadDirAttr == 0 {
		t.Errorf("serverCaps = 0x%x, missing SUPPORTS_READ_DIR_ATTR", serverCaps)
	}
	if serverCaps&aaplCapSupportsOSXCopyFile != 0 {
		t.Errorf("serverCaps = 0x%x, must not claim OSX_COPYFILE (no copychunk)", serverCaps)
	}
	if serverCaps&aaplCapSupportsNFSAce != 0 {
		t.Errorf("serverCaps = 0x%x, must not claim NFS_ACE", serverCaps)
	}

	if volCaps := binary.LittleEndian.Uint64(data[24:]); volCaps != 0 {
		t.Errorf("volumeCaps = 0x%x, want 0", volCaps)
	}

	if got := binary.LittleEndian.Uint32(data[32:]); got != 0 {
		t.Errorf("modelInfo reserved = %d, want 0", got)
	}
	modelLen := binary.LittleEndian.Uint32(data[36:])
	if int(modelLen) != len(model) {
		t.Fatalf("modelLen = %d, want %d", modelLen, len(model))
	}
	if string(data[40:40+modelLen]) != string(model) {
		t.Errorf("model = %q, want %q", data[40:40+modelLen], model)
	}
}

func TestAaplReplyHonoursRequestBitmap(t *testing.T) {
	c := newAaplTestConn()

	// 只要 ServerCaps：16 + 8。
	req := []createContext{{Name: createTagAAPL, Data: []byte{
		0x01, 0, 0, 0, 0, 0, 0, 0,
		0x01, 0, 0, 0, 0, 0, 0, 0,
		0x1f, 0, 0, 0, 0, 0, 0, 0,
	}}}
	if data := aaplData(t, aaplReplyOf(t, c, req)); len(data) != 24 {
		t.Errorf("bitmap=1 reply length = %d, want 24", len(data))
	}

	// 位图 0：只有 16 字节头。
	req[0].Data[8] = 0
	if data := aaplData(t, aaplReplyOf(t, c, req)); len(data) != 16 {
		t.Errorf("bitmap=0 reply length = %d, want 16", len(data))
	}
}

func TestAaplReplySkipsWhenClientLacksReadDirAttr(t *testing.T) {
	c := newAaplTestConn()
	// clientCaps = 0x2（只声明 OSX_COPYFILE），不得回 READ_DIR_ATTR。
	req := []createContext{{Name: createTagAAPL, Data: []byte{
		0x01, 0, 0, 0, 0, 0, 0, 0,
		0x01, 0, 0, 0, 0, 0, 0, 0,
		0x02, 0, 0, 0, 0, 0, 0, 0,
	}}}
	data := aaplData(t, aaplReplyOf(t, c, req))
	caps := binary.LittleEndian.Uint64(data[16:])
	if caps != aaplCapUnixBased {
		t.Errorf("serverCaps = 0x%x, want 0x%x (UNIX_BASED only)", caps, aaplCapUnixBased)
	}
}

func TestAaplReplyIgnoresBadInput(t *testing.T) {
	c := newAaplTestConn()
	cases := []struct {
		name string
		data []byte
	}{
		{"short", make([]byte, 16)},
		{"resolve-id", append([]byte{0x02, 0, 0, 0}, make([]byte, 20)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := c.aaplReply([]createContext{{Name: createTagAAPL, Data: tc.data}}); got != nil {
				t.Fatalf("got reply %x, want nil", got.Data)
			}
		})
	}
	// 无 AAPL 上下文时什么也不回。
	if got, _ := c.aaplReply([]createContext{{Name: createTagMxAc, Data: make([]byte, 8)}}); got != nil {
		t.Fatal("replied to a request without an AAPL context")
	}
	// 空列表不能 panic。
	if got, _ := c.aaplReply(nil); got != nil {
		t.Fatal("replied to an empty context list")
	}
}

func TestAaplServerCapsOverridable(t *testing.T) {
	t.Setenv("SELFNAS_SMB_AAPL_SERVER_CAPS", "0x4")
	if caps := aaplServerCaps(0x1f, fsAttrBits()); caps != 0x4 {
		t.Errorf("override caps = 0x%x, want 0x4", caps)
	}
}
