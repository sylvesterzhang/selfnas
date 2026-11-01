// MIT License
//
// Copyright (c) 2026 Jimmy Fjällid

package server_test

import (
	"encoding/binary"
	"testing"

	"github.com/jfjallid/go-smb/smb/server"
)

// buildAAPLContext lays out a single SMB2 create context the way Apple's client
// sends it in TREE_CONNECT:
//
//	Next(4)=0 NameOffset(2)=16 NameLength(2)=4 Reserved(2)=6
//	DataOffset(2)=24 DataLength(4)=8 "AAPL" <pad> kind(4) caps(4)
func buildAAPLContext(kind, caps uint32) []byte {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint16(b[4:], 16)  // NameOffset
	binary.LittleEndian.PutUint16(b[6:], 4)   // NameLength
	binary.LittleEndian.PutUint16(b[8:], 6)   // Reserved
	binary.LittleEndian.PutUint16(b[10:], 24) // DataOffset
	binary.LittleEndian.PutUint32(b[12:], 8)  // DataLength
	copy(b[16:], "AAPL")
	binary.LittleEndian.PutUint32(b[24:], kind)
	binary.LittleEndian.PutUint32(b[28:], caps)
	return b
}

// TestAppleTreeConnectReply covers the handshake that decides whether iOS
// mounts the share read-write. Apple's client asks (kAAPL_SERVER_QUERY); if the
// server stays silent the client concludes it cannot store Apple metadata and
// mounts read-only, with every SMB command still returning STATUS_SUCCESS.
func TestAppleTreeConnectReply(t *testing.T) {
	req := buildAAPLContext(1 /* kAAPL_SERVER_QUERY */, 0x00000001)

	out := server.AppleTreeConnectReplyForTest(req)
	if len(out) != len(req) {
		t.Fatalf("reply length = %d, want %d (same shape as the request context)", len(out), len(req))
	}
	if got := string(out[16:20]); got != "AAPL" {
		t.Errorf("context name = %q, want \"AAPL\"", got)
	}
	if next := binary.LittleEndian.Uint32(out[0:4]); next != 0 {
		t.Errorf("Next = %d, want 0 (single context)", next)
	}
	if kind := binary.LittleEndian.Uint32(out[24:28]); kind != 2 /* kAAPL_SERVER_CAPS */ {
		t.Errorf("reply kind = %d, want kAAPL_SERVER_CAPS (2)", kind)
	}
	// We advertise no optional extensions: READ_DIR_ATTR and OSX_COPYFILE are
	// not implemented, and claiming them would break listings and copies.
	if caps := binary.LittleEndian.Uint32(out[28:32]); caps != 0 {
		t.Errorf("capabilities = 0x%08x, want 0 (no optional AAPL extensions)", caps)
	}
}

// TestAppleTreeConnectReplyIgnoresNonAAPL makes sure a non-AAPL (or malformed)
// context list produces no reply rather than a corrupt one — sending a bogus
// context back would be worse than staying silent.
func TestAppleTreeConnectReplyIgnoresNonAAPL(t *testing.T) {
	if got := server.AppleTreeConnectReplyForTest(nil); got != nil {
		t.Errorf("nil context produced %d bytes, want nil", len(got))
	}
	if got := server.AppleTreeConnectReplyForTest([]byte{1, 2, 3}); got != nil {
		t.Errorf("short context produced %d bytes, want nil", len(got))
	}
	req := buildAAPLContext(1, 0)
	copy(req[16:20], "MxAc") // a different (Windows DFS-style) context name
	if got := server.AppleTreeConnectReplyForTest(req); got != nil {
		t.Errorf("non-AAPL context produced %d bytes, want nil", len(got))
	}
}
