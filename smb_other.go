//go:build !windows

package main

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
)

// 非 Windows 平台的 445 处理原则：**只告诉你怎么办，绝不替你去停别人的服务**。
//
// 和 Windows 的差别不是偷懒，是这几件事在 Linux / macOS 上没有可靠答案：
//
//   - 服务名不统一：Debian/Ubuntu 是 smbd.service，Fedora/RHEL 是 smb.service，
//     老系统还可能是 OpenRC / SysVinit，容器和 WSL 里连 systemctl 都没有。
//     猜错一条命令，用户照着敲就是「把自己的共享搞坏了」。
//   - 停完怎么还回去：Windows 上我们有 tookOver 记账 + 明确的还原命令（srvnet / srv2
//     / LanmanServer 三个服务名几十年没变过）；Linux 上还得记住原来是不是 enabled，
//     我们没记，就不该动。
//   - 那是用户自己装的：Samba 很可能是他有意开着给别人共享的，擅自停掉等于砸场子。
//   - 权限：停 Samba 要 root，我们的进程未必是 root，命令会失败还得解释半天。
//
// 所以这里只做两件事：**分清是「没权限」还是「被占」**（这两个混在一起讲，
// Linux 用户会以为是 Samba 占着，白折腾半天），然后告诉他该敲什么。

// smbTakeoverWindows 在非 Windows 上没有意义。
func smbTakeoverWindows() (string, error) {
	return "", fmt.Errorf("只有 Windows 需要「接管 445」：本机的 SMB 服务请你自己停，停完再回来打开这个开关")
}

// smbRestoreWindows 在其他平台同样无事可做：没有「系统自带 SMB 被我们顶掉」这回事。
func smbRestoreWindows() (string, error) {
	return "", nil
}

// smbListenHint 把「445 绑不上」翻译成能照着做的提示。
func smbListenHint(port int, err error) error {
	switch {
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		// 1024 以下是系统端口：Linux / macOS 上普通用户绑不了
		if runtime.GOOS == "darwin" {
			return fmt.Errorf("没有权限监听 %d 端口（1024 以下是系统端口）。\n"+
				"请用管理员权限启动：sudo ./selfnas", port)
		}
		return fmt.Errorf("没有权限监听 %d 端口（1024 以下是系统端口）。二选一：\n"+
			"  1) 用 root 启动：sudo ./selfnas\n"+
			"  2) 只给这一个程序授权（推荐，之后普通用户也能绑）：\n"+
			"     sudo setcap cap_net_bind_service=+ep ./selfnas\n"+
			"  注意：重新编译 / 换新版本后 setcap 会失效，要再执行一次", port)

	case errors.Is(err, syscall.EADDRINUSE):
		if runtime.GOOS == "darwin" {
			return fmt.Errorf("端口 %d 已被占用：本机的「文件共享」正开着。\n"+
				"请先关掉它，再回来打开这个开关：\n"+
				"  系统设置 → 通用 → 共享 → 关掉「文件共享」\n"+
				"  （或在终端执行：sudo launchctl bootout system/com.apple.smbd）", port)
		}
		return fmt.Errorf("端口 %d 已被占用：本机装了 SMB 服务（通常是 Samba）。\n"+
			"请先停掉它，再回来打开这个开关：\n"+
			"  Ubuntu / Debian：sudo systemctl stop smbd && sudo systemctl disable smbd\n"+
			"  Fedora / RHEL：  sudo systemctl stop smb  && sudo systemctl disable smb\n"+
			"（我们不会替你去停——那可能是你自己开给别人的共享）", port)
	}
	return fmt.Errorf("端口 %d 监听失败：%v", port, err)
}
