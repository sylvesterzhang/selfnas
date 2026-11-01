// MIT License
//
// Copyright (c) 2026 Jimmy Fjällid

package smb

import (
	"encoding/binary"
	"testing"

	"github.com/jfjallid/go-smb/smb/unicode"
)

func hdr(cmd uint16) []byte {
	h := Header{Command: cmd, StructureSize: headerSize}
	buf, _ := h.marshalInto(make([]byte, 0, headerSize))
	return buf
}

// TestTreeConnectReqLegacyForm pins the 9-byte (pre-3.1.1) TREE_CONNECT
// request: no contexts, Reserved present. Windows and older clients send this,
// and it must keep parsing exactly as before the extension was added.
func TestTreeConnectReqLegacyForm(t *testing.T) {
	path := unicode.ToUnicode("\\\\SRV\\文档")
	body := make([]byte, 8)
	binary.LittleEndian.PutUint16(body[0:], 9)            // StructureSize
	binary.LittleEndian.PutUint16(body[4:], uint16(64+8)) // PathOffset
	binary.LittleEndian.PutUint16(body[6:], uint16(len(path)))
	pdu := append(hdr(CommandTreeConnect), body...)
	pdu = append(pdu, path...)

	var req TreeConnectReq
	if err := req.UnmarshalBinary(pdu); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if req.StructureSize != 9 {
		t.Errorf("StructureSize = %d, want 9", req.StructureSize)
	}
	got, err := unicode.FromUnicodeString(req.Path)
	if err != nil {
		t.Fatalf("decode path: %v", err)
	}
	if got != "\\\\SRV\\文档" {
		t.Errorf("Path = %q", got)
	}
	if req.Context != nil {
		t.Errorf("Context = %d bytes, want none in the legacy form", len(req.Context))
	}
}

// TestTreeConnectReqWithAAPLContext covers the SMB 3.1.1 form: StructureSize
// 16, Flags at the old Reserved position, and ContextOffset/ContextLength
// after PathLength. Apple's client uses this to ask for Apple extensions.
func TestTreeConnectReqWithAAPLContext(t *testing.T) {
	path := unicode.ToUnicode("\\\\SRV\\资料")
	ctx := make([]byte, 32)
	binary.LittleEndian.PutUint16(ctx[4:], 16)  // NameOffset
	binary.LittleEndian.PutUint16(ctx[6:], 4)   // NameLength
	binary.LittleEndian.PutUint16(ctx[8:], 6)   // Reserved
	binary.LittleEndian.PutUint16(ctx[10:], 24) // DataOffset
	binary.LittleEndian.PutUint32(ctx[12:], 8)  // DataLength
	copy(ctx[16:], "AAPL")
	binary.LittleEndian.PutUint32(ctx[24:], 1) // kAAPL_SERVER_QUERY
	binary.LittleEndian.PutUint32(ctx[28:], 1)

	pathOff := 64 + 16
	ctxOff := pathOff + len(path)
	body := make([]byte, 16)
	binary.LittleEndian.PutUint16(body[0:], 16) // StructureSize
	binary.LittleEndian.PutUint16(body[2:], 1)  // Flags: EXTENSION_PRESENT
	binary.LittleEndian.PutUint16(body[4:], uint16(pathOff))
	binary.LittleEndian.PutUint16(body[6:], uint16(len(path)))
	binary.LittleEndian.PutUint32(body[8:], uint32(ctxOff))
	binary.LittleEndian.PutUint32(body[12:], uint32(len(ctx)))

	pdu := append(hdr(CommandTreeConnect), body...)
	pdu = append(pdu, path...)
	pdu = append(pdu, ctx...)

	var req TreeConnectReq
	if err := req.UnmarshalBinary(pdu); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if req.StructureSize != 16 {
		t.Errorf("StructureSize = %d, want 16", req.StructureSize)
	}
	if req.Flags != 1 {
		t.Errorf("Flags = %d, want 1 (EXTENSION_PRESENT)", req.Flags)
	}
	got, err := unicode.FromUnicodeString(req.Path)
	if err != nil {
		t.Fatalf("decode path: %v", err)
	}
	if got != "\\\\SRV\\资料" {
		t.Errorf("Path = %q", got)
	}
	if len(req.Context) != len(ctx) {
		t.Fatalf("Context = %d bytes, want %d", len(req.Context), len(ctx))
	}
	if string(req.Context[16:20]) != "AAPL" {
		t.Errorf("context name = %q, want AAPL", string(req.Context[16:20]))
	}
}

// TestTreeConnectResContextAppended checks that create contexts land right
// after the fixed 16-byte body — TREE_CONNECT Response has no offset field for
// them, so the position is implied.
func TestTreeConnectResContextAppended(t *testing.T) {
	res := TreeConnectRes{
		Header:        Header{Command: CommandTreeConnect},
		StructureSize: 16,
		ShareType:     ShareTypeDisk,
		MaximalAccess: 0x001f01ff,
		Context:       []byte("hello-ctx"),
	}
	buf, err := res.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if len(buf) != headerSize+16+len("hello-ctx") {
		t.Fatalf("wire length = %d, want %d", len(buf), headerSize+16+len("hello-ctx"))
	}
	if got := string(buf[headerSize+16:]); got != "hello-ctx" {
		t.Errorf("tail = %q", got)
	}
	if got := binary.LittleEndian.Uint32(buf[headerSize+12:]); got != 0x001f01ff {
		t.Errorf("MaximalAccess = 0x%08x", got)
	}
}
