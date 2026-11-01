// MIT License
//
// Copyright (c) 2026 Jimmy Fjällid

package server_test

import (
	"encoding/binary"
	"testing"

	"github.com/jfjallid/go-smb/smb/server"
)

// TestDefaultSecurityDescriptorGrantsWrite parses the fallback security
// descriptor the way a client would and asserts the two properties Apple's
// SMB client depends on:
//
//  1. SE_DACL_PRESENT is set — a *null* DACL is legal Windows ("allow all")
//     but Apple's client reads it as "no ACEs, therefore no rights", which
//     turns the whole mount read-only.
//  2. The DACL holds an ACCESS_ALLOWED ACE for Everyone with FILE_ALL_ACCESS.
func TestDefaultSecurityDescriptorGrantsWrite(t *testing.T) {
	sd := server.DefaultSecurityDescriptorForTest()
	if len(sd) < 20 {
		t.Fatalf("security descriptor too short: %d bytes", len(sd))
	}
	if sd[0] != 0x01 {
		t.Errorf("Revision=%d, want 1", sd[0])
	}
	control := binary.LittleEndian.Uint16(sd[2:4])
	const (
		seDaclPresent  = 0x0004
		seSelfRelative = 0x8000
	)
	if control&seSelfRelative == 0 {
		t.Errorf("Control=0x%04x lacks SE_SELF_RELATIVE", control)
	}
	if control&seDaclPresent == 0 {
		t.Fatalf("Control=0x%04x lacks SE_DACL_PRESENT; "+
			"Apple clients read a null DACL as zero rights and mount read-only", control)
	}

	daclOff := int(binary.LittleEndian.Uint32(sd[16:20]))
	if daclOff <= 0 || daclOff+8 > len(sd) {
		t.Fatalf("Dacl offset %d out of range (len %d)", daclOff, len(sd))
	}
	aclRev := sd[daclOff]
	aclSize := int(binary.LittleEndian.Uint16(sd[daclOff+2 : daclOff+4]))
	aceCount := binary.LittleEndian.Uint16(sd[daclOff+4 : daclOff+6])
	if aclRev != 0x02 {
		t.Errorf("AclRevision=%d, want 2", aclRev)
	}
	if aclSize != len(sd)-daclOff {
		t.Errorf("AclSize=%d but only %d bytes follow", aclSize, len(sd)-daclOff)
	}
	if aceCount != 1 {
		t.Fatalf("AceCount=%d, want 1", aceCount)
	}

	aceOff := daclOff + 8
	aceType := sd[aceOff]
	aceSize := int(binary.LittleEndian.Uint16(sd[aceOff+2 : aceOff+4]))
	mask := binary.LittleEndian.Uint32(sd[aceOff+4 : aceOff+8])
	if aceType != 0x00 {
		t.Errorf("AceType=0x%02x, want ACCESS_ALLOWED (0x00)", aceType)
	}
	if aceSize != 20 {
		t.Errorf("AceSize=%d, want 20 (Everyone SID is 12 bytes)", aceSize)
	}
	if mask != 0x001F01FF {
		t.Errorf("AceMask=0x%08x, want FILE_ALL_ACCESS (0x001f01ff)", mask)
	}
	// SID must be S-1-1-0: rev 1, one sub-authority, authority 1, subauth 0.
	sid := sd[aceOff+8:]
	if sid[0] != 1 || sid[1] != 1 {
		t.Errorf("SID header = % x, want rev=1 count=1", sid[:2])
	}
	authority := uint64(0)
	for i := 2; i < 8; i++ {
		authority = authority<<8 | uint64(sid[i])
	}
	if authority != 1 {
		t.Errorf("SID authority = %d, want 1 (SECURITY_WORLD_SID_AUTHORITY)", authority)
	}
	if binary.LittleEndian.Uint32(sid[8:12]) != 0 {
		t.Errorf("SID sub-authority = %d, want 0 (Everyone)", binary.LittleEndian.Uint32(sid[8:12]))
	}
}
