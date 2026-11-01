//go:build darwin

package main

// macOS 的开机自启：和 Linux 一样，**只生成 plist，安装交给用户敲命令**。
//
// 用 LaunchDaemon（/Library/LaunchDaemons）而不是 LaunchAgent（~/Library/LaunchAgents）：
// Agent 是登录后才起、且以登录用户身份跑，绑不了 80 / 445 这种特权端口。
// Daemon 是开机就起、以 root 跑，这才是「NAS 常驻」该有的形态。
//
// launchd 对 plist 的属主 / 权限很挑：必须是 root:wheel 且 644，
// 否则 launchctl bootstrap 会直接拒（报 "Invalid argument" 或权限错误）。
// 所以安装命令里把 chown/chmod 一起给了。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	asPlistLabel       = "cc.selfnas.server"
	asPlistName        = "cc.selfnas.server.plist"
	asPlistInstallPath = "/Library/LaunchDaemons/" + asPlistName
)

type autostartInfo struct {
	Supported  bool   `json:"supported"`
	Enabled    bool   `json:"enabled"`
	Mode       string `json:"mode"` // launchd
	Exe        string `json:"exe"`
	CurrentExe string `json:"currentExe"`
	Detail     string `json:"detail"`
	ConfigPath   string `json:"configPath,omitempty"`
	Command      string `json:"command,omitempty"`
	CommandStart string `json:"commandStart,omitempty"`
	CommandOff   string `json:"commandOff,omitempty"`
}

func autostartGet() autostartInfo {
	info := autostartInfo{Supported: true, Mode: "launchd"}
	exe, dir := asSelfPaths()
	info.CurrentExe = exe

	if _, err := os.Stat(asPlistInstallPath); err == nil {
		info.Enabled = true
		info.Exe = asPlistProgram(asPlistInstallPath)
		info.Detail = "LaunchDaemon " + asPlistLabel + "：开机自动启动"
	} else {
		info.Detail = "未安装——需要你在终端执行一条命令（见下方）"
	}

	if p, err := asWritePlist(exe, dir); err == nil {
		info.ConfigPath = p
	}
	info.Command, info.CommandOff = asInstallCommands()
	if info.Command == "" {
		info.Detail = "生成 plist 失败（家目录不可写？）"
	}
	return info
}

// autostartSet 在 macOS 上同样不做系统改动：只重新生成 plist 并返回命令。
func autostartSet(enable bool) (autostartInfo, error) {
	return autostartGet(), nil
}

func asSelfPaths() (exe, dir string) {
	e, err := os.Executable()
	if err != nil {
		return "", ""
	}
	if abs, err := filepath.Abs(e); err == nil {
		e = abs
	}
	if real, err := filepath.EvalSymlinks(e); err == nil {
		e = real
	}
	return e, filepath.Dir(e)
}

// xmlEsc 转义路径里的 XML 特殊字符（路径里出现 & 之类会毁掉整个 plist）。
func xmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func asPlistContent(exe, dir string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + asPlistLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEsc(exe) + `</string>
	</array>
	<!-- 工作目录必须是 exe 所在目录：config.json 默认就在它旁边。
	     不设的话 launchd 从 "/" 起，程序读不到配置，会当成全新安装重新生成一套。 -->
	<key>WorkingDirectory</key>
	<string>` + xmlEsc(dir) + `</string>
	<key>RunAtLoad</key>
	<true/>
	<!-- 只在异常退出时重启；正常停止（用户在管理台关掉）不会被拉起来 -->
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>` + xmlEsc(filepath.Join(dir, "logs", "launchd.out.log")) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEsc(filepath.Join(dir, "logs", "launchd.err.log")) + `</string>
	<key>ProcessType</key>
	<string>Background</string>
</dict>
</plist>
`
}

func asWritePlist(exe, dir string) (string, error) {
	if exe == "" {
		return "", fmt.Errorf("拿不到自身路径")
	}
	// launchd 打开 StandardOutPath 时目录必须已存在，否则服务起不来
	_ = os.MkdirAll(filepath.Join(dir, "logs"), 0o755)
	base, err := os.UserHomeDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	confDir := filepath.Join(base, ".config", "selfnas")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(confDir, asPlistName)
	want := asPlistContent(exe, dir)
	if old, err := os.ReadFile(p); err == nil && string(old) == want {
		return p, nil
	}
	if err := os.WriteFile(p, []byte(want), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

func asInstallCommands() (install, uninstall string) {
	exe, dir := asSelfPaths()
	src, err := asWritePlist(exe, dir)
	if err != nil {
		return "", ""
	}
	// chown/chmod 不能省：launchd 要求 Daemon 的 plist 是 root:wheel + 644
	install = fmt.Sprintf(
		"sudo cp '%s' %s && sudo chown root:wheel %s && sudo chmod 644 %s && sudo launchctl bootstrap system %s",
		src, asPlistInstallPath, asPlistInstallPath, asPlistInstallPath, asPlistInstallPath)
	uninstall = fmt.Sprintf(
		"sudo launchctl bootout system/%s 2>/dev/null; sudo rm -f %s",
		asPlistLabel, asPlistInstallPath)
	return install, uninstall
}

// asPlistProgram 从已安装的 plist 里抠出第一个 ProgramArguments，用于对比指向。
func asPlistProgram(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.Index(s, "<key>ProgramArguments</key>")
	if i < 0 {
		return ""
	}
	rest := s[i:]
	j := strings.Index(rest, "<string>")
	if j < 0 {
		return ""
	}
	rest = rest[j+len("<string>"):]
	k := strings.Index(rest, "</string>")
	if k < 0 {
		return ""
	}
	v := strings.TrimSpace(rest[:k])
	return strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">").Replace(v)
}
