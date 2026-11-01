//go:build linux

package main

// Linux 的「磁盘发现」。Windows 首装扫盘符（C 盘之外全开、卷标做名字），
// 这里保持同一口径：
//   1) 家目录 —— Linux 没有盘符，家目录是最接近「用户自己的空间」的存在；
//   2) /media、/mnt 下已挂载的外部存储（U 盘、移动硬盘、NFS/CIFS），
//      挂载点路径当名字，系统挂载点与虚拟文件系统一律不扫。
// 用户之后可以在管理台增删、改名、收窄。

import (
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

func listDrives() []DriveInfo {
	var out []DriveInfo
	if hd, ok := homeDrive(); ok {
		out = append(out, hd)
	}
	if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		mounts := linuxExternalMounts(parseMountInfoLinux(string(b)))
		sort.Slice(mounts, func(i, j int) bool { return mounts[i].Path < mounts[j].Path })
		for _, m := range mounts {
			if st, err := os.Stat(m.Path); err != nil || !st.IsDir() {
				continue
			}
			out = append(out, DriveInfo{
				Path:  m.Path,
				Label: uniqueDriveName(filepath.Base(m.Path), out),
				Kind:  driveKindOf(m.FSType),
				Ready: true,
			})
		}
	}
	return out
}

// volumeSpaceOS 磁盘容量。Statfs 在 Linux/macOS 上字段含义一致：
// Blocks*frsize 是总容量，Bavail（非 root 保留后可用）*frsize 是剩余可用。
func volumeSpaceOS(path string) (uint64, uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bsize := uint64(st.Bsize)
	return uint64(st.Blocks) * bsize, uint64(st.Bavail) * bsize
}
