//go:build !windows

package main

// 非 Windows 平台的自启动：暂不实现，管理台开关显示「此系统暂不支持」。
// Linux 上正路是 systemd user unit（要处理发行版差异），等真有需求再做。

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
