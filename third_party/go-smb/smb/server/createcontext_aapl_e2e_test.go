package server_test

// 端到端验证 "AAPL" create context 的应答（MS-AAPL / vfs_fruit.c check_aapl）。
//
// 为什么值得一条 E2E：Apple 客户端在挂载共享根时的第一次 CREATE 会带这个
// 上下文问「你支持哪些 Apple 扩展」。服务端不回，iOS 的「文件」App 就会把
// 共享标成「只读」，「新建文件夹」变灰 —— 而日志里每个响应都是 STATUS_SUCCESS。
// 只有把请求真的发一遍、把响应字节摊开看，才能证明那一环补上了。

import (
	"encoding/binary"
	"testing"

	"github.com/jfjallid/go-smb/smb"
)

// aaplContextBlob 拼一个只含 "AAPL" 的 create context 列表。
// 布局按 MS-SMB2 §2.2.13：头 16 字节 + 名字 + 8 字节对齐的 data。
func aaplContextBlob(data []byte) []byte {
	const nameOff = 16
	const dataOff = 24 // 16 + "AAPL" 4 字节，对齐到 8
	b := make([]byte, dataOff+len(data))
	binary.LittleEndian.PutUint32(b[0:], 0)  // Next context = 0
	binary.LittleEndian.PutUint16(b[4:], nameOff)
	binary.LittleEndian.PutUint16(b[6:], 4)
	binary.LittleEndian.PutUint16(b[10:], dataOff)
	binary.LittleEndian.PutUint32(b[12:], uint32(len(data)))
	copy(b[nameOff:], "AAPL")
	copy(b[dataOff:], data)
	return b
}

// aaplServerQueryData 是 2026-10-29 23:25:08 从 iPhone（iOS 26）抓到的
// 真实 24 字节 AAPL 请求体。
var aaplServerQueryData = []byte{
	0x01, 0x00, 0x00, 0x00, // CommandCode = SERVER_QUERY
	0x00, 0x00, 0x00, 0x00, // 保留
	0x07, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // reqBitmap = ServerCaps|VolumeCaps|ModelInfo
	0x1f, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // clientCaps
}

// 核心用例：客户端带 AAPL 问，服务端必须回一段 AAPL，且 ServerCaps 里
// SUPPORTS_READ_DIR_ATTR 必须在 —— 这一位是 iOS 判定「可写」的关键。
func TestCreateAaplContextAnswered(t *testing.T) {
	port, stop, user, pass, share := startMxAcServer(t)
	defer stop()
	c := dialMxAc(t, port, user, pass, share)

	pdu := buildRootCreatePDU(c.SessionID(), c.TreeID(share), aaplContextBlob(aaplServerQueryData))
	resp, err := c.SendRawPDU(pdu)
	if err != nil {
		t.Fatalf("SendRawPDU: %v", err)
	}
	if status := binary.LittleEndian.Uint32(resp[8:12]); status != smb.StatusOk {
		t.Fatalf("CREATE status = 0x%08x，应为 STATUS_SUCCESS", status)
	}

	ctxs := parseCreateContextsAt(t, resp)
	data, ok := ctxs["AAPL"]
	if !ok {
		t.Fatalf("响应里没有 AAPL 上下文，实际有：%v", keysOf(ctxs))
	}

	if len(data) < 16 {
		t.Fatalf("AAPL 应答体 %d 字节，至少应有 16 字节头", len(data))
	}
	if cmd := binary.LittleEndian.Uint32(data[0:]); cmd != 1 {
		t.Errorf("CommandCode = %d，应为 1 (SERVER_QUERY)", cmd)
	}
	if bm := binary.LittleEndian.Uint64(data[8:]); bm != 7 {
		t.Errorf("回显的 reqBitmap = 0x%x，应为 0x7", bm)
	}

	// 16 头 + ServerCaps 8 + VolumeCaps 8 + ModelInfo 8 + model
	if len(data) < 40 {
		t.Fatalf("AAPL 应答体 %d 字节，reqBitmap=7 时至少应有 40 字节", len(data))
	}
	serverCaps := binary.LittleEndian.Uint64(data[16:])
	if serverCaps&0x4 == 0 {
		t.Errorf("serverCaps = 0x%x，缺 UNIX_BASED", serverCaps)
	}
	if serverCaps&0x1 == 0 {
		t.Errorf("serverCaps = 0x%x，缺 SUPPORTS_READ_DIR_ATTR —— iOS 会因此挂成只读", serverCaps)
	}
	if serverCaps&0x2 != 0 {
		t.Errorf("serverCaps = 0x%x，不该声明 OSX_COPYFILE（没实现 copychunk）", serverCaps)
	}

	modelLen := binary.LittleEndian.Uint32(data[36:])
	if len(data) != 40+int(modelLen) {
		t.Errorf("AAPL 应答体 %d 字节，meta 里 modelLen=%d，应为 %d", len(data), modelLen, 40+modelLen)
	}
	if modelLen > 0 && string(data[40:40+modelLen]) == "" {
		t.Error("ModelInfo 段为空")
	}
}

// 反向用例：没问就别答，避免给不认识的上下文乱造数据。
func TestCreateWithoutAaplHasNoContexts(t *testing.T) {
	port, stop, user, pass, share := startMxAcServer(t)
	defer stop()
	c := dialMxAc(t, port, user, pass, share)

	resp, err := c.SendRawPDU(buildRootCreatePDU(c.SessionID(), c.TreeID(share), nil))
	if err != nil {
		t.Fatalf("SendRawPDU: %v", err)
	}
	if ctxs := parseCreateContextsAt(t, resp); len(ctxs) != 0 {
		t.Errorf("没请求上下文却回了 %v", keysOf(ctxs))
	}
}

// 长度不对的 AAPL 上下文必须被安静跳过，不能把整个 CREATE 打回 ——
// 打回等于连浏览都做不了，比不答更糟。
func TestCreateAaplBadLengthIgnored(t *testing.T) {
	port, stop, user, pass, share := startMxAcServer(t)
	defer stop()
	c := dialMxAc(t, port, user, pass, share)

	resp, err := c.SendRawPDU(buildRootCreatePDU(c.SessionID(), c.TreeID(share), aaplContextBlob(make([]byte, 8))))
	if err != nil {
		t.Fatalf("SendRawPDU: %v", err)
	}
	if status := binary.LittleEndian.Uint32(resp[8:12]); status != smb.StatusOk {
		t.Fatalf("CREATE status = 0x%08x，应为 STATUS_SUCCESS", status)
	}
	if _, ok := parseCreateContextsAt(t, resp)["AAPL"]; ok {
		t.Error("长度为 8 的 AAPL 上下文不该得到应答")
	}
}
