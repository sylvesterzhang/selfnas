package server

import (
	"encoding/binary"
	"testing"
)

// MxAc 的应答体就是 8 字节：QueryStatus=0 + MaximalAccess。
// 这个测试盯的是「字节形状」而不是逻辑，因为客户端解析的是偏移。
func TestMaximalAccessReply(t *testing.T) {
	req := []createContext{
		{Name: createTagMxAc}, // 空 data，实测 iOS 就是发 0 字节
	}
	got := maximalAccessReply(req, 0x001f01ff)
	if got == nil {
		t.Fatal("带 MxAc 上下文时应答不能为 nil")
	}
	if got.Name != "MxAc" {
		t.Fatalf("应答上下文名 = %q，应为 MxAc", got.Name)
	}
	if len(got.Data) != 8 {
		t.Fatalf("应答体长度 = %d，应为 8", len(got.Data))
	}
	if qs := binary.LittleEndian.Uint32(got.Data[0:]); qs != 0 {
		t.Errorf("QueryStatus = 0x%08x，应为 0", qs)
	}
	if ma := binary.LittleEndian.Uint32(got.Data[4:]); ma != 0x001f01ff {
		t.Errorf("MaximalAccess = 0x%08x，应为 0x001f01ff", ma)
	}
}

func TestMaximalAccessReplyAbsent(t *testing.T) {
	if got := maximalAccessReply(nil, 0x001f01ff); got != nil {
		t.Errorf("没有 MxAc 上下文时不应凭空造一个，得到 %+v", got)
	}
	if got := maximalAccessReply([]createContext{{Name: "AAPL", Data: make([]byte, 24)}}, 0x1f01ff); got != nil {
		t.Errorf("只有 AAPL 时不应回 MxAc，得到 %+v", got)
	}
}

// 应答上下文要能被服务端自己的解析器读回来，且 8 字节数据落到对齐的位置上。
func TestMaximalAccessReplyRoundTrip(t *testing.T) {
	mx := maximalAccessReply([]createContext{{Name: createTagMxAc}}, 0x001f01ff)
	blob := marshalCreateContexts([]createContext{*mx})
	back, err := parseCreateContexts(blob)
	if err != nil {
		t.Fatalf("parseCreateContexts: %v", err)
	}
	if len(back) != 1 {
		t.Fatalf("解析出 %d 个上下文，应为 1", len(back))
	}
	if back[0].Name != "MxAc" {
		t.Errorf("名字 = %q，应为 MxAc", back[0].Name)
	}
	if len(back[0].Data) != 8 {
		t.Fatalf("数据长度 = %d，应为 8", len(back[0].Data))
	}
	if ma := binary.LittleEndian.Uint32(back[0].Data[4:]); ma != 0x001f01ff {
		t.Errorf("回读 MaximalAccess = 0x%08x，应为 0x001f01ff", ma)
	}
}

func TestShareMaximalAccessDefault(t *testing.T) {
	if got := shareMaximalAccess(Share{}); got != defaultMaximalAccess {
		t.Errorf("Share.MaximalAccess 为 0 时应回落到 0x%08x，得到 0x%08x", defaultMaximalAccess, got)
	}
	if got := shareMaximalAccess(Share{MaximalAccess: 0x00120089}); got != 0x00120089 {
		t.Errorf("显式设置时应原样返回，得到 0x%08x", got)
	}
}
