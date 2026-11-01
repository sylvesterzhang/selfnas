package server

import (
	"encoding/binary"
	"testing"
)

// fakeHandle 是最小 Handle 实现，只为了让 buildResumeKey 拿到 FileID。
type fakeHandle struct{ id uint64 }

func (f fakeHandle) Stat() (FileInfo, error) { return FileInfo{FileID: f.id}, nil }
func (f fakeHandle) Path() Path              { return "\\x" }
func (f fakeHandle) IsDir() bool             { return false }

// TestBuildResumeKey: FSCTL_SRV_REQUEST_RESUME_KEY 的应答必须是 24 字节
// （MS-SMB2 §2.2.32.3，与 Samba 的 file_id 三件套等长），且同一文件恒定。
func TestBuildResumeKey(t *testing.T) {
	got := buildResumeKey(fakeHandle{id: 0xdeadbeefcafef00d})
	if len(got) != 24 {
		t.Fatalf("resume key length = %d, want 24", len(got))
	}
	if v := binary.LittleEndian.Uint64(got[0:]); v != 0xdeadbeefcafef00d {
		t.Fatalf("resume key id = 0x%x, want 0xdeadbeefcafef00d", v)
	}
	again := buildResumeKey(fakeHandle{id: 0xdeadbeefcafef00d})
	if string(again) != string(got) {
		t.Fatal("resume key is not stable for the same file")
	}
	other := buildResumeKey(fakeHandle{id: 1})
	if string(other) == string(got) {
		t.Fatal("resume key must differ between files")
	}
	// 没有句柄时也要给出一个合法长度，不能 panic。
	if n := len(buildResumeKey(nil)); n != 24 {
		t.Fatalf("nil handle resume key length = %d, want 24", n)
	}
}

// TestFileIDBytesFor 锁住 SMB2 FileId 的字节布局：低 8 字节是持久 ID
// （必须与目录枚举的 FileId 一致），高 8 字节是 volatile 句柄号。
func TestFileIDBytesFor(t *testing.T) {
	b := fileIDBytesFor(0x1111111122222222, 0x3333333344444444)
	if len(b) != 16 {
		t.Fatalf("FileId length = %d, want 16", len(b))
	}
	if v := binary.LittleEndian.Uint64(b[0:]); v != 0x1111111122222222 {
		t.Fatalf("persistent half = 0x%x", v)
	}
	// 句柄查找只认 volatile 半边 —— 加了持久 ID 之后不能破坏这一点。
	if v := volatileFromFileID(b); v != 0x3333333344444444 {
		t.Fatalf("volatile half = 0x%x", v)
	}
}
