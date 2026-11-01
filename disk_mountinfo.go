package main

// /proc/self/mountinfo 的解析与「外部存储」判定。
// 故意不带平台标签：解析和过滤是纯函数，在 Windows 开发机上就能跑单元测试；
// 真正读 /proc/self/mountinfo 的代码在 disk_linux.go。

import (
	"sort"
	"strconv"
	"strings"
)

type mountEntry struct {
	Path   string // 挂载点
	FSType string // 文件系统类型（ext4、ntfs、nfs4、squashfs…）
}

// parseMountInfoLinux 解析 /proc/self/mountinfo 的文本内容。
// 行格式：
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
//
// 「 - 」前是挂载描述（第 5 列是挂载点），后面依次是文件系统类型、设备源、超级块选项。
func parseMountInfoLinux(content string) []mountEntry {
	var out []mountEntry
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+3:])
		if len(left) < 5 || len(right) < 1 {
			continue
		}
		out = append(out, mountEntry{Path: unescapeMountPath(left[4]), FSType: right[0]})
	}
	return out
}

// unescapeMountPath 还原挂载点里的八进制转义：
// 空格 \040、制表 \011、换行 \012、反斜杠 \134（proc(5) 规定）。
func unescapeMountPath(p string) string {
	if !strings.Contains(p, `\`) {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' && i+3 < len(p) {
			switch p[i+1 : i+4] {
			case "040":
				b.WriteByte(' ')
				i += 3
				continue
			case "011":
				b.WriteByte('\t')
				i += 3
				continue
			case "012":
				b.WriteByte('\n')
				i += 3
				continue
			case "134":
				b.WriteByte('\\')
				i += 3
				continue
			}
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// virtualFSTypes 不该当共享目录扫出来的文件系统。
// squashfs 是 snap/iso；overlay 是容器；tmpfs 是内存盘；其余是内核伪文件系统。
var virtualFSTypes = map[string]bool{
	"squashfs": true, "tmpfs": true, "ramfs": true, "overlay": true,
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true,
	"mqueue": true, "hugetlbfs": true, "cgroup": true, "cgroup2": true,
	"autofs": true, "binfmt_misc": true, "configfs": true, "debugfs": true,
	"tracefs": true, "pstore": true, "bpf": true, "securityfs": true,
	"fusectl": true, "fuse.gvfsd-fuse": true, "nsfs": true,
	"rpc_pipefs": true, "efivarfs": true, "none": true,
}

// networkFSTypes 网络文件系统 —— 和 Windows 扫盘符口径一致（网络盘也算盘）。
var networkFSTypes = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smbfs": true,
	"sshfs": true, "fuse.sshfs": true, "9p": true, "ncpfs": true,
}

// linuxExternalMounts 从 mountinfo 里挑出该自动共享的外部挂载点：
//   - 位置必须在 /media、/mnt 之下（含 /mnt 本身被挂的情况）；
//   - 文件系统不能是虚拟的（snap 的 squashfs、容器的 overlay、内存盘 tmpfs 全挡掉）；
//   - 嵌套挂载只留最浅一层（仅限 /media：U 盘带 Windows 分区表时子分区会
//     自动挂进父挂载点；/mnt 下的挂载是用户自己配的，各是各的盘，不互相吞）。
//
// 返回按挂载点路径排序。
func linuxExternalMounts(entries []mountEntry) []mountEntry {
	var cand []mountEntry
	for _, e := range entries {
		if virtualFSTypes[e.FSType] {
			continue
		}
		p := strings.TrimSuffix(e.Path, "/")
		if p == "" || !strings.HasPrefix(p, "/media/") && p != "/mnt" && !strings.HasPrefix(p, "/mnt/") {
			continue
		}
		cand = append(cand, e)
	}
	// 浅的排前面，嵌套去重才能只留外层
	sort.Slice(cand, func(i, j int) bool {
		return strings.Count(cand[i].Path, "/") < strings.Count(cand[j].Path, "/")
	})
	var keep []mountEntry
	for _, e := range cand {
		// 嵌套去重只对 /media 做：U 盘带 Windows 分区表时子分区会自动挂进父挂载点
		//（/media/u/disk/part1），留外层即可；/mnt 下的挂载是用户自己配的
		//（/mnt/disk1、/mnt/disk2…），各是各的盘，不能互相吞。
		if !strings.HasPrefix(e.Path, "/media/") {
			keep = append(keep, e)
			continue
		}
		nested := false
		for _, k := range keep {
			if e.Path == k.Path || strings.HasPrefix(e.Path, k.Path+"/") {
				nested = true
				break
			}
		}
		if !nested {
			keep = append(keep, e)
		}
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].Path < keep[j].Path })
	return keep
}

// driveKindOf 按文件系统类型归类，前端只是展示用。
func driveKindOf(fsType string) string {
	if networkFSTypes[fsType] {
		return "network"
	}
	return "fixed"
}

// uniqueDriveName 避免重名（两个 U 盘可能都叫 "USB DISK"）。
func uniqueDriveName(want string, existing []DriveInfo) string {
	name := want
	for i := 2; ; i++ {
		taken := false
		for _, d := range existing {
			if d.Label == name {
				taken = true
				break
			}
		}
		if !taken {
			return name
		}
		name = want + " " + strconv.Itoa(i)
	}
}
