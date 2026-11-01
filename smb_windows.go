//go:build windows

package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// 接管 / 还回去 都要求管理员权限，所以都走 UAC 提权；
// 提权进程的 stdout 拿不到（Start-Process 起的另一个控制台），
// 结果统一写进临时文件再读回来。

// smbListenHint 把「445 绑不上」翻译成 Windows 用户能照着做的提示。
// Windows 上基本只有一个原因：系统自带的文件共享占着，点「接管」即可。
func smbListenHint(port int, err error) error {
	return fmt.Errorf("端口 %d 被系统 SMB 服务占着（这是 Windows 自带文件共享），先在管理台点「接管 %d」", port, port)
}

// smbTakeoverWindows 把 Windows 自带的 SMB 服务停掉，腾出 445 给 selfnas。
//
// 关键实测（2026-10-29）：只停 LanmanServer / srv2 都不释放端口，
// 必须停 srvnet 驱动 —— 停完 445 立刻空出来，而且**不需要重启**。
func smbTakeoverWindows() (string, error) {
	resFile := filepath.Join(os.TempDir(), "selfnas-smb-takeover.txt")

	// 检查 445 是否还被「别人」占着时，要排掉我们自己的监听 ——
	// 否则「SMB 已经在跑、只想把系统服务设成禁用」这个场景会误报 BUSY，
	// 用户点一下按钮反而收到一句「445 仍被占用」，看着像按钮坏了。
	inner := fmt.Sprintf(`$ErrorActionPreference='SilentlyContinue'
foreach ($s in @('srvnet','srv2','LanmanServer')) {
  Stop-Service -Name $s -Force
  # 顺便设成「禁用」：否则下次开机 Windows 的 SMB 又会抢回 445，
  # 用户会莫名其妙地发现手机连不上了。
  Set-Service -Name $s -StartupType Disabled
}
Start-Sleep -Milliseconds 900
$own = @(Get-NetTCPConnection -State Listen -LocalPort 445 | Where-Object { $_.OwningProcess -eq %d })
$all = @(Get-NetTCPConnection -State Listen -LocalPort 445)
# 注意：PowerShell 里 if 语句不能直接接管道（会报「不允许使用空管道元素」），
# 必须先落到变量再 Out-File —— 这个坑实测踩过
$r = 'FREE'
if ($all.Count -gt $own.Count) { $r = 'BUSY' }
$r | Out-File -FilePath '%s' -Encoding utf8`, os.Getpid(), strings.ReplaceAll(resFile, "'", "''"))

	out, err := psElevated(inner, resFile)
	if err != nil {
		return "", err
	}
	switch {
	case strings.Contains(out, "FREE"):
		return "445 已接管：Windows 自带文件共享已停止，并设为开机不再启动", nil
	case strings.Contains(out, "BUSY"):
		return "", fmt.Errorf("命令执行了，但 445 仍被占用（可能是别的程序）")
	}
	return "", fmt.Errorf("结果无法识别：%s", strings.TrimSpace(out))
}

// smbRestoreWindows 把 445 还给 Windows 自带的文件共享。
//
// 关掉 selfnas 的 SMB 时必须调这个——不然用户会看到「SMB 关了，
// 结果 Windows 自己的网络邻居也用不了」，等于我们把人家的功能带走了。
// LanmanServer 恢复成「自动」并启动（srv2 / srvnet 是它的依赖驱动，改回「手动」即可，
// 由 LanmanServer 按需拉起）。
func smbRestoreWindows() (string, error) {
	resFile := filepath.Join(os.TempDir(), "selfnas-smb-restore.txt")

	inner := fmt.Sprintf(`$ErrorActionPreference='SilentlyContinue'
Set-Service -Name LanmanServer -StartupType Automatic
Set-Service -Name srv2 -StartupType Manual
Set-Service -Name srvnet -StartupType Manual
Start-Service -Name srvnet
Start-Service -Name LanmanServer
# 系统共享起监听要一点时间，最多等 6 秒
for ($i = 0; $i -lt 20; $i++) {
  Start-Sleep -Milliseconds 300
  if (Get-NetTCPConnection -State Listen -LocalPort 445) { break }
}
$up = @(Get-NetTCPConnection -State Listen -LocalPort 445 | Where-Object { $_.OwningProcess -ne %d })
$r = 'DOWN'
if ($up.Count -gt 0) { $r = 'UP' }
$r | Out-File -FilePath '%s' -Encoding utf8`, os.Getpid(), strings.ReplaceAll(resFile, "'", "''"))

	out, err := psElevated(inner, resFile)
	if err != nil {
		return "", err
	}
	switch {
	case strings.Contains(out, "UP"):
		return "已把 445 还给 Windows 自带文件共享（服务已恢复自动启动）", nil
	case strings.Contains(out, "DOWN"):
		return "", fmt.Errorf("服务已改回自动，但 445 还没起来（可以等几秒再试，或在「服务」里手动启动 LanmanServer）")
	}
	return "", fmt.Errorf("结果无法识别：%s", strings.TrimSpace(out))
}

// psElevated 用管理员权限跑一段 PowerShell，读回它写在 resFile 里的最后结果。
func psElevated(inner, resFile string) (string, error) {
	_ = os.Remove(resFile)
	outer := fmt.Sprintf(`$p = Start-Process -FilePath powershell -Verb RunAs -PassThru -Wait `+
		`-ArgumentList '-NoProfile','-NonInteractive','-EncodedCommand','%s'`, psEncode(inner))
	if _, err := psRun(outer); err != nil {
		return "", fmt.Errorf("提权执行失败（可能点了「否」）：%v", err)
	}
	b, err := os.ReadFile(resFile)
	if err != nil {
		return "", fmt.Errorf("没拿到执行结果（可能被取消了）：%v", err)
	}
	_ = os.Remove(resFile)
	return string(b), nil
}

// psEncode 转成 -EncodedCommand 需要的 UTF-16LE + base64
func psEncode(script string) string {
	u := utf16.Encode([]rune(script))
	buf := make([]byte, len(u)*2)
	for i, r := range u {
		buf[i*2] = byte(r)
		buf[i*2+1] = byte(r >> 8)
	}
	return base64.StdEncoding.EncodeToString(buf)
}
