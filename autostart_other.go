//go:build !windows && !linux && !darwin

package main

// 剩下的平台（FreeBSD 等）暂不支持自启，管理台开关显示「此系统暂不支持」。
// Windows / Linux / macOS 各有自己的实现（autostart.go / _linux.go / _darwin.go）。

type autostartInfo struct {
	Supported  bool   `json:"supported"`
	Enabled    bool   `json:"enabled"`
	Mode       string `json:"mode"`
	Exe        string `json:"exe"`
	CurrentExe string `json:"currentExe"`
	Detail     string `json:"detail"`
}

func autostartGet() autostartInfo {
	return autostartInfo{Supported: false, Detail: "此系统暂不支持开机自启（目前仅 Windows）"}
}

func autostartSet(enable bool) (autostartInfo, error) {
	return autostartGet(), nil
}
