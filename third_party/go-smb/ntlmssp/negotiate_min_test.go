package ntlmssp

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildRawNegotiate32 手工构造一条 32 字节的裸 NTLMSSP NEGOTIATE：
// Signature(8) + MessageType(4) + Flags(4) + DomainNameDesc(8) + WorkstationDesc(8)。
// 这是 MS-NLMP 2.2.1.1 允许的最小形态（不带可选的 Version 字段），
// 某些第三方 SMB 客户端就是这么发的。
func buildRawNegotiate32(flags uint32) []byte {
	buf := make([]byte, 32)
	copy(buf[0:8], Signature)
	binary.LittleEndian.PutUint32(buf[8:12], TypeNtLmNegotiate)
	binary.LittleEndian.PutUint32(buf[12:16], flags)
	// DomainName / Workstation 两个字段描述全部为 0（不带负载）
	return buf
}

func TestNegotiateUnmarshal32Bytes(t *testing.T) {
	const flags uint32 = 0xE0088207 // 56|128|KEY_EXCH|TARGET_INFO|EXT_SEC|ALWAYS_SIGN|NTLM|REQUEST_TARGET
	buf := buildRawNegotiate32(flags)

	var neg Negotiate
	if err := neg.UnmarshalBinary(buf); err != nil {
		t.Fatalf("32 字节 NEGOTIATE 应能解码: %v", err)
	}
	if neg.NegotiateFlags != flags {
		t.Errorf("flags = 0x%08x, 期望 0x%08x", neg.NegotiateFlags, flags)
	}
	if neg.Version != 0 {
		t.Errorf("不带 Version 字段时 Version 应为 0，实际 %d", neg.Version)
	}
	if len(neg.DomainName) != 0 || len(neg.Workstation) != 0 {
		t.Errorf("无负载时 DomainName/Workstation 应为空")
	}
}

func TestNegotiateUnmarshal40Bytes(t *testing.T) {
	// 带完整 Version 的 40 字节形态（本库 MarshalBinary 的输出）
	neg40 := &Negotiate{
		Header:         Header{Signature: []byte(Signature), MessageType: TypeNtLmNegotiate},
		NegotiateFlags: 0x02000000 | 0x00000200,
		Version:        0x00000F00,
	}
	buf, err := neg40.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if len(buf) < negotiateFixedSize {
		t.Fatalf("marshal 输出 %d 字节，应 >= %d", len(buf), negotiateFixedSize)
	}
	var back Negotiate
	if err := back.UnmarshalBinary(buf); err != nil {
		t.Fatalf("40 字节 NEGOTIATE 应能解码: %v", err)
	}
	if back.Version != 0x00000F00 {
		t.Errorf("Version = 0x%x, 期望 0xf00", back.Version)
	}
}

func TestNegotiateUnmarshalTooShort(t *testing.T) {
	buf := buildRawNegotiate32(1)[:31]
	var neg Negotiate
	err := neg.UnmarshalBinary(buf)
	if err == nil {
		t.Fatal("31 字节应报错")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("requires 32 bytes")) {
		t.Errorf("错误信息应写明最低 32 字节，实际: %v", err)
	}
}
