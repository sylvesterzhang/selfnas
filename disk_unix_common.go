//go:build linux || darwin

package main

import "os"

// 家目录条目：Linux/macOS 没有盘符，家目录是最接近「用户自己的空间」的存在。
// Letter 留空（bootstrapConfig 里 Enabled: d.Letter != "C"，空串恒为 true —— 开箱即用）。
func homeDrive() (DriveInfo, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" {
		return DriveInfo{}, false
	}
	return DriveInfo{Path: home, Label: "家目录", Kind: "fixed", Ready: true}, true
}
