//go:build linux

package main

// Linux 的开机自启：**我们只生成配置文件，安装交给用户自己敲命令**。
//
// 为什么不替他装（和 Windows 的处理方式故意不一样）：
//   - 要装进 /etc/systemd/system 必须有 root，而我们的进程通常不是 root。
//     自己 sudo 自己这套在 Linux 上做不干净（没 TTY、密码要 stdin）。
//   - 这一步动的是系统目录，用户应该自己看一眼内容再执行 —— 给他一条
//     复制就能跑的命令，比偷偷往 /etc 里写文件更符合 Linux 用户的习惯。
//
// 关键约束：**80 和 445 都是 1024 以下的特权端口**，普通用户绑不了。
// 所以 unit 里用 AmbientCapabilities=CAP_NET_BIND_SERVICE 单独授权，
// 而不是把整个服务跑成 root —— 少一分权限就少一分风险。

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
)

const (
	asUnitName        = "selfnas.service"
	asUnitInstallPath = "/etc/systemd/system/" + asUnitName
)

type autostartInfo struct {
	Supported  bool   `json:"supported"`
	Enabled    bool   `json:"enabled"`
	Mode       string `json:"mode"` // systemd
	Exe        string `json:"exe"`
	CurrentExe string `json:"currentExe"`
	Detail     string `json:"detail"`
	// 下面三个只对 Linux/macOS 有意义：配置文件是我们生成的，
	// 但「装进系统」这一步要用户自己在终端执行（见文件头注释）。
	ConfigPath  string `json:"configPath,omitempty"`  // 生成好的 unit 文件路径
	Command     string `json:"command,omitempty"`     // 安装并启用开机自启（带 --now，即装即启）
	CommandStart string `json:"commandStart,omitempty"` // 仅现在启动（不开机自启）
	CommandOff  string `json:"commandOff,omitempty"`  // 卸载命令
}

func autostartGet() autostartInfo {
	info := autostartInfo{Supported: true, Mode: "systemd"}
	exe, dir := asSelfPaths()
	info.CurrentExe = exe

	if _, err := os.Stat(asUnitInstallPath); err == nil {
		info.Enabled = true
		info.Exe = asUnitExecStart(asUnitInstallPath)
		info.Detail = "systemd 服务 " + asUnitName + "：开机自动启动"
	} else {
		info.Detail = "未安装——需要你在终端执行一条命令（见下方）"
	}

	// 配置文件随时备好（幂等），这样用户任何时候复制命令都能直接用
	if p, err := asWriteUnit(exe, dir); err == nil {
		info.ConfigPath = p
	}
	info.Command, info.CommandStart, info.CommandOff = asInstallCommands()
	if info.Command == "" {
		info.Detail = "客户端凭据准备好了，但生成配置文件失败（家目录不可写？）"
	}
	return info
}

// autostartSet 在 Linux 上不做任何系统改动：开启 = 重新生成配置文件并返回命令，
// 关闭 = 只返回卸载命令。真正的启停在用户粘完那条命令之后。
//
// 所以这里返回的 Enabled 始终是「系统里实际装没装」，不是「用户点了什么」。
func autostartSet(enable bool) (autostartInfo, error) {
	info := autostartGet()
	if !enable && !info.Enabled {
		return info, nil // 本来就没装，无事可做
	}
	return info, nil
}

// asSelfPaths 返回 exe 绝对路径和它所在目录（= 工作目录，config.json 在那儿）。
func asSelfPaths() (exe, dir string) {
	e, err := os.Executable()
	if err != nil {
		return "", ""
	}
	if abs, err := filepath.Abs(e); err == nil {
		e = abs
	}
	if real, err := filepath.EvalSymlinks(e); err == nil {
		e = real // /usr/local/bin 里常是软链，unit 要写真实路径
	}
	return e, filepath.Dir(e)
}

// asWriteUnit 把 unit 写到 ~/.config/selfnas/（内容没变就不重写）。
func asWriteUnit(exe, dir string) (string, error) {
	if exe == "" {
		return "", fmt.Errorf("拿不到自身路径")
	}
	base, err := os.UserHomeDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	confDir := filepath.Join(base, ".config", "selfnas")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(confDir, asUnitName)
	want := asUnitContent(exe, dir, asServiceUser())
	if old, err := os.ReadFile(p); err == nil && string(old) == want {
		return p, nil
	}
	if err := os.WriteFile(p, []byte(want), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// asServiceUser 返回当前进程的用户名，写进 unit 的 User=。
// 服务以「生成 unit 的这个用户」运行，数据目录/文件主才对得上；
// root 身份生成时回 "root"，模板里会走「不写 User=、不剥能力」的分支。
func asServiceUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// asInstallCommands 给「复制即用」的安装 / 仅启动 / 卸载命令。
func asInstallCommands() (install, start, uninstall string) {
	exe, dir := asSelfPaths()
	src, err := asWriteUnit(exe, dir)
	if err != nil {
		return "", "", ""
	}
	install = fmt.Sprintf(
		"sudo cp '%s' %s && sudo systemctl daemon-reload && sudo systemctl enable --now %s",
		src, asUnitInstallPath, "selfnas")
	// 仅现在启动：把 unit 放到位但不 enable，下次开机不会自动拉起
	start = fmt.Sprintf(
		"sudo cp '%s' %s && sudo systemctl daemon-reload && sudo systemctl start %s",
		src, asUnitInstallPath, "selfnas")
	uninstall = fmt.Sprintf(
		"sudo systemctl disable --now %s && sudo rm -f %s && sudo systemctl daemon-reload",
		"selfnas", asUnitInstallPath)
	return install, start, uninstall
}
