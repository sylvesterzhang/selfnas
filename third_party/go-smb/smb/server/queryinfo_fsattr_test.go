// MIT License
//
// Copyright (c) 2026 Jimmy Fjällid

package server_test

import (
	"encoding/binary"
	"testing"

	"github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"
)

// TestFsAttributeInformationAdvertisesNamedStreams pins the one bit that
// decides whether Apple clients (iOS Files, macOS Finder) mount the share
// read-write:
//
// FILE_NAMED_STREAMS. Without it the iOS client assumes the server cannot
// store extended attributes, and defensively mounts the share read-only —
// with every request still returning STATUS_SUCCESS, so the server log shows
// a perfectly healthy session while the phone displays 只读.
//
// It also guards the opposite mistake: advertising FILE_READ_ONLY_VOLUME
// would make *every* client mount read-only.
func TestFsAttributeInformationAdvertisesNamedStreams(t *testing.T) {
	buf, status := server.DefaultFsInfoForTest(0x05, "资料")
	if status != smb.StatusOk {
		t.Fatalf("defaultFsInfo(FileFsAttributeInformation) status=0x%08x, want StatusOk", status)
	}
	if len(buf) < 12 {
		t.Fatalf("buffer too short: %d bytes", len(buf))
	}
	attrs := binary.LittleEndian.Uint32(buf[0:4])

	if attrs&server.FsAttrNamedStreams == 0 {
		t.Errorf("FileSystemAttributes=0x%08x lacks FILE_NAMED_STREAMS (0x%08x); "+
			"Apple clients will mount this share read-only",
			attrs, server.FsAttrNamedStreams)
	}
	if attrs&server.FsAttrPersistentACLs == 0 {
		t.Errorf("FileSystemAttributes=0x%08x lacks FILE_PERSISTENT_ACLS (0x%08x); "+
			"Apple clients then ignore the security descriptor and derive no rights",
			attrs, server.FsAttrPersistentACLs)
	}
	if attrs&server.FsAttrReadOnlyVolume != 0 {
		t.Errorf("FileSystemAttributes=0x%08x advertises FILE_READ_ONLY_VOLUME; "+
			"every client would mount this share read-only", attrs)
	}
	// Byte-for-byte parity with Samba's stock writable share.
	if attrs != 0x000400CF {
		t.Errorf("FileSystemAttributes=0x%08x, want 0x000400cf "+
			"(case-sensitive-search|case-preserved|unicode-on-disk|persistent-acls|"+
			"sparse-files|reparse-points|named-streams)", attrs)
	}

	nameLen := binary.LittleEndian.Uint32(buf[8:12])
	if int(nameLen) != len(buf)-12 {
		t.Errorf("FileSystemNameLength=%d but buffer holds %d name bytes", nameLen, len(buf)-12)
	}
}
