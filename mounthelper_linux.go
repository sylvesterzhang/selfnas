//go:build linux

package main

// Linux 的「外接盘热插拔自动挂载」助手（三个文件：脚本 + systemd 模板 + udev 规则）。
//
// 为什么需要它：Linux 不是插盘即挂。
//   - 程序自己挂不了：selfnas 以普通用户运行，systemd 里只给了 CAP_NET_BIND_SERVICE，
//     既不能 mkdir /mnt 也不能 mount（24 上实测过，日志里是一条 permission denied）。
//   - fstab 救不了热插拔：它只在开机那一刻由 local-fs.target 处理，运行中插进来的盘
//     不会重试，挂载点只剩一个空目录。
//   - udev 里也不能直接 mount：systemd-udevd 带 PrivateMounts=yes，它做的挂载留在
//     自己的私有命名空间里，外面的程序看不见。所以 udev 只负责点火，动作交给服务。
//
// 为什么不直接写进 /etc：和 autostart_linux.go 一个道理 —— 动系统目录要 root，
// 而本进程没有 TTY 也没有密码。所以这里只把文件准备好、把命令给出来，
// 装不装由用户执行一条 sudo 命令决定。

import (
	_ "embed"
	"os"
	"path/filepath"
)

// 三个文件编进二进制：用户不需要在旁边放这些散文件。
//
//go:embed scripts/selfnas-mount.sh
var mhScript []byte

//go:embed scripts/selfnas-mount@.service
var mhUnit []byte

//go:embed scripts/99-selfnas-mount.rules
var mhRule []byte

const (
	mhScriptInstall = "/usr/local/bin/selfnas-mount.sh"
	mhUnitInstall   = "/etc/systemd/system/selfnas-mount@.service"
	mhRuleInstall   = "/etc/udev/rules.d/99-selfnas-mount.rules"
)

// mhReload 装完 / 删完都要走这一脚，否则 systemd 和 udev 还拿着旧的。
const mhReload = "sudo systemctl daemon-reload && sudo udevadm control --reload-rules"

type mhFile struct {
	Name string
	Data []byte
	Mode os.FileMode
}

func mhFiles() []mhFile {
	return []mhFile{
		{"selfnas-mount.sh", mhScript, 0o755},
		{"selfnas-mount@.service", mhUnit, 0o644},
		{"99-selfnas-mount.rules", mhRule, 0o644},
	}
}

// mhWrite 把三个文件写到 ~/.config/selfnas/mount/（内容没变就不重写），返回目录。
func mhWrite() (string, error) {
	base, err := os.UserHomeDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, ".config", "selfnas", "mount")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, f := range mhFiles() {
		p := filepath.Join(dir, f.Name)
		if old, err := os.ReadFile(p); err == nil && string(old) == string(f.Data) {
			continue
		}
		if err := os.WriteFile(p, f.Data, f.Mode); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// mhInstalled 三个文件都在 = 装好了。少一个就当没装（半装状态重装一遍最省事）。
func mhInstalled() bool {
	for _, p := range []string{mhScriptInstall, mhUnitInstall, mhRuleInstall} {
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}

// mhInstallParts 返回「把文件装进系统」的那几步（不含 reload 和 rm）。
func mhInstallParts(dir string) []string {
	return []string{
		"sudo install -m 0755 '" + dir + "/selfnas-mount.sh' " + mhScriptInstall,
		"sudo install -m 0644 '" + dir + "/selfnas-mount@.service' '" + mhUnitInstall + "'",
		"sudo install -m 0644 '" + dir + "/99-selfnas-mount.rules' '" + mhRuleInstall + "'",
	}
}

// mhRemoveParts 卸载：连脚本一起删。
func mhRemoveParts() []string {
	return []string{
		"sudo rm -f " + mhScriptInstall + " '" + mhUnitInstall + "' '" + mhRuleInstall + "'",
		mhReload,
	}
}
