//go:build darwin

package main

// macOS 的「磁盘发现」：家目录 + /Volumes 下的真实卷。
// /Volumes/Macintosh HD 是指向 / 的软链（跳过），Time Machine 备份卷
// 不会被特别排除 —— 用户在管理台关掉即可，代码不做自作聪明的判断。

import (
	"os"
	"path/filepath"
	"syscall"
)

func listDrives() []DriveInfo {
	var out []DriveInfo
	if hd, ok := homeDrive(); ok {
		out = append(out, hd)
	}
	entries, err := os.ReadDir("/Volumes")
	if err == nil {
		for _, e := range entries {
			if e.Type()&os.ModeSymlink != 0 {
				continue // "Macintosh HD" 之类是指向 / 的软链
			}
			p := filepath.Join("/Volumes", e.Name())
			if st, err := os.Stat(p); err != nil || !st.IsDir() {
				continue
			}
			if real, err := filepath.EvalSymlinks(p); err == nil && real == "/" {
				continue
			}
			out = append(out, DriveInfo{
				Path:  p,
				Label: uniqueDriveName(e.Name(), out),
				Kind:  "fixed",
				Ready: true,
			})
		}
	}
	return out
}

func volumeSpaceOS(path string) (uint64, uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bsize := uint64(st.Bsize)
	return uint64(st.Blocks) * bsize, uint64(st.Bavail) * bsize
}
