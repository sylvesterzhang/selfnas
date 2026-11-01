package main

import (
	"reflect"
	"strings"
	"testing"
)

// sample 取自真实 /proc/self/mountinfo 的典型片段（含转义、snap、嵌套挂载）。
const sampleMountInfo = `22 2 0:21 / / rw,relatime - ext4 /dev/sda2 rw
25 22 0:23 / /proc rw,nosuid,nodev - proc proc rw
27 22 0:25 / /sys rw,nosuid,nodev,noexec - sysfs sysfs rw
28 22 0:26 / /dev rw,nosuid - devtmpfs udev rw
33 28 0:29 / /run rw,nosuid - tmpfs tmpfs rw
45 22 259:2 / /media/zhanghuan/移动硬盘 rw,nosuid - ntfs3 /dev/sdb1 rw
46 22 8:17 / /mnt/data rw - ext4 /dev/sdb2 rw
48 45 8:33 / /media/zhanghuan/移动硬盘/part1 rw - ext4 /dev/sdc1 rw
52 22 0:61 / /media/zhanghuan/音乐仓库 ro - squashfs /dev/loop3 ro
55 22 0:65 / /mnt rw - ext4 /dev/sdd1 rw
60 45 0:70 / /media/zhanghuan/移动硬盘/.Trash rw - tmpfs tmpfs rw
63 22 0:75 /run/user/1000/doc /media/zhanghuan/移动硬盘/doc rw - fuse.gvfsd-fuse gvfsd-fuse rw
70 22 0:80 / /mnt/nas rw - nfs4 192.168.1.10:/export/data rw
72 22 0:85 /snap/core22/1000 /snap/core22/1000 ro - squashfs /dev/loop1 ro
75 22 0:90 /mnt/with\040space /mnt/with\040space rw - ext4 /dev/sde1 rw`

// TestParseMountInfoLinux 解析：列数、fstype、转义还原。
func TestParseMountInfoLinux(t *testing.T) {
	entries := parseMountInfoLinux(sampleMountInfo)
	if len(entries) != 15 {
		t.Fatalf("期望 15 条，得到 %d 条", len(entries))
	}
	byPath := map[string]string{}
	for _, e := range entries {
		byPath[e.Path] = e.FSType
	}
	if byPath["/"] != "ext4" {
		t.Errorf("/ 的 fstype 应为 ext4，得到 %q", byPath["/"])
	}
	if byPath["/media/zhanghuan/移动硬盘"] != "ntfs3" {
		t.Errorf("中文挂载点解析失败：%v", byPath)
	}
	if _, ok := byPath["/mnt/with space"]; !ok {
		t.Errorf("\\040 转义没有还原成空格：%v", byPath)
	}
	if _, ok := byPath["/mnt/with\\134space"]; ok {
		t.Errorf("转义序列不应原样保留")
	}
}

// TestLinuxExternalMounts 过滤口径：位置、虚拟文件系统、嵌套去重。
func TestLinuxExternalMounts(t *testing.T) {
	entries := parseMountInfoLinux(sampleMountInfo)
	got := linuxExternalMounts(entries)
	var paths []string
	for _, m := range got {
		paths = append(paths, m.Path)
	}
	want := []string{"/media/zhanghuan/移动硬盘", "/mnt", "/mnt/data", "/mnt/nas", "/mnt/with space"}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Errorf("过滤结果不符\n 得到: %v\n 期望: %v（顺序按路径）", paths, want)
	}
}

// TestLinuxExternalMountsRejectsVirtual 排除名单逐项确认。
func TestLinuxExternalMountsRejectsVirtual(t *testing.T) {
	entries := []mountEntry{
		{"/mnt/a", "squashfs"},        // snap
		{"/mnt/b", "tmpfs"},           // 内存盘
		{"/mnt/c", "overlay"},         // 容器
		{"/mnt/d", "fuse.gvfsd-fuse"}, // GVfs
		{"/mnt/e", "ext4"},            // 真盘
	}
	got := linuxExternalMounts(entries)
	if len(got) != 1 || got[0].Path != "/mnt/e" {
		t.Errorf("应只保留 /mnt/e，得到 %v", got)
	}
}

// TestDriveKindOf 网络文件系统归 network。
func TestDriveKindOf(t *testing.T) {
	for _, fs := range []string{"nfs4", "cifs", "9p", "fuse.sshfs"} {
		if driveKindOf(fs) != "network" {
			t.Errorf("%s 应为 network", fs)
		}
	}
	if driveKindOf("ext4") != "fixed" || driveKindOf("ntfs3") != "fixed" {
		t.Errorf("本地文件系统应为 fixed")
	}
}

// TestUniqueDriveName 重名要加后缀。
func TestUniqueDriveName(t *testing.T) {
	existing := []DriveInfo{{Label: "USB DISK"}}
	if got := uniqueDriveName("USB DISK", existing); got != "USB DISK 2" {
		t.Errorf("重名应变 %q，得到 %q", "USB DISK 2", got)
	}
	if got := uniqueDriveName("移动硬盘", existing); got != "移动硬盘" {
		t.Errorf("不重名应原样保留，得到 %q", got)
	}
	if !reflect.DeepEqual(uniqueDriveName("", nil), "") {
		t.Errorf("空名应原样返回")
	}
}
