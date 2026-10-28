//go:build windows

package main

// 开机自启动：管理台里的开关，负责「开机后自动在后台把 selfnas 拉起来」。
//
// 首选计划任务（S4U）：开机就启动、不等登录、后台运行没有任何窗口 —— 这正是
// 用户在意的形态（黑窗口被吐槽过）。S4U 建不起来（权限不足等）才退回注册表
// Run 键，后者每个用户都能写，但启动时会带一个控制台窗口。
//
// 任务名固定为 selfnas，和本程序的手工部署方式一致：开关就是这件事的唯一事实来源，
// 点「开启」永远用当前 exe 重建任务，exe 搬家之后点一下就能纠正指向。
//
// ⚠️ 所有 PowerShell 调用都走 -EncodedCommand（UTF-16LE + base64），不靠命令行传参：
// powershell.exe 按控制台代码页解析参数，中文路径（F:\项目\...）会被写成一串乱码，
// 表现为「开关显示已开启，开机却什么也没起」。输出侧也要强制 UTF-8，
// 否则 Go 这边收到的是 GBK 字节，回给前端就是乱码。

import (
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

const asTaskName = "selfnas"
const asRunKey = `HKCU:\Software\Microsoft\Windows\CurrentVersion\Run`

type autostartInfo struct {
	Supported  bool   `json:"supported"`
	Enabled    bool   `json:"enabled"`
	Mode       string `json:"mode"`       // task=计划任务（无窗口） run=注册表启动项（有窗口） ""=未开启
	Exe        string `json:"exe"`        // 当前配置指向的 exe（未开启为空）
	CurrentExe string `json:"currentExe"` // 正在运行的进程自己的 exe，前端对比两者是否一致
	Detail     string `json:"detail"`
}

// psRun 跑一段 PowerShell（UTF-16LE base64 传脚本），返回 UTF-8 stdout。
//
// 末尾补 `exit 0`：`Get-ScheduledTask -ErrorAction SilentlyContinue` 查不到任务时
// PowerShell 会把 $? 置假并以退出码 1 结束，被当成「查询出错」——实际上「没有任务」
// 是正常结果。真正的异常仍然会因为 $ErrorActionPreference='Stop' 终止并返回非 0。
func psRun(script string) (string, error) {
	full := "$ProgressPreference='SilentlyContinue'; $ErrorActionPreference='Stop'; " +
		"[Console]::OutputEncoding=[Text.Encoding]::UTF8; " + script + "; exit 0"
	u := utf16.Encode([]rune(full))
	buf := make([]byte, len(u)*2)
	for i, r := range u {
		buf[i*2] = byte(r)
		buf[i*2+1] = byte(r >> 8)
	}
	enc := base64.StdEncoding.EncodeToString(buf)
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive",
		"-EncodedCommand", enc).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, firstLine(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// autostartGet 带缓存的查询入口。
// 为什么必须缓存：管理台概览页每 2 秒拉一次 /api/state，而 state 里要带自启状态；
// 每次现查 = 每 2 秒起一个 powershell.exe，对一台常年开着的 NAS 来说太浪费。
// 5 秒的陈旧度对「开关状态」完全无感，切换时会主动失效缓存。
var (
	asCacheMu sync.Mutex
	asCache   autostartInfo
	asCacheAt time.Time
)

const asCacheTTL = 5 * time.Second

func autostartGet() autostartInfo {
	asCacheMu.Lock()
	defer asCacheMu.Unlock()
	if !asCacheAt.IsZero() && time.Since(asCacheAt) < asCacheTTL {
		return asCache
	}
	asCache = autostartGetRaw()
	asCacheAt = time.Now()
	return asCache
}

func autostartInvalidate() {
	asCacheMu.Lock()
	asCacheAt = time.Time{}
	asCacheMu.Unlock()
}

// autostartGetRaw 真去问系统：一次 PowerShell 拿「任务是否存在 + 指向哪个 exe」。
func autostartGetRaw() autostartInfo {
	info := autostartInfo{Supported: true}
	if exe, err := os.Executable(); err == nil {
		if abs, err := filepath.Abs(exe); err == nil {
			info.CurrentExe = abs
		}
	}
	out, err := psRun(fmt.Sprintf(
		`$t = Get-ScheduledTask -TaskName '%s' -ErrorAction SilentlyContinue; `+
			`if ($t) { ($t.Actions | ForEach-Object { $_.Execute.Trim('"') }) -join ';' }`,
		asTaskName))
	if err == nil && out != "" {
		info.Enabled, info.Mode = true, "task"
		info.Exe = out
		info.Detail = "计划任务 " + asTaskName + "：开机自动后台启动，无窗口"
		return info
	}
	if err != nil {
		log.Printf("[自启] 查询计划任务失败: %v", err)
	}
	// 计划任务没有 → 看注册表 Run 兜底方式
	if v := asRunKeyGet(); v != "" {
		info.Enabled, info.Mode = true, "run"
		info.Exe = v
		info.Detail = "注册表启动项：登录后启动，会闪一个控制台窗口"
	}
	return info
}

// autostartSet 开关。开启时优先计划任务，失败退注册表；关闭时两种形式都清。
func autostartSet(enable bool) (autostartInfo, error) {
	defer autostartInvalidate() // 切换后立刻作废旧状态
	// 先把两种形式都清干净，避免「任务和注册表同时存在」的怪状态；
	// 开启流程最后会重新创建想要的那个。
	asRunKeyDelete()
	rmErr := asTaskRemove()
	if !enable {
		// 关闭必须真的关掉：删不掉就报错，不能返回「成功」但状态还是开着
		if rmErr != nil {
			return autostartGet(), fmt.Errorf("删除计划任务失败: %w", rmErr)
		}
		return autostartGet(), nil
	}
	if rmErr != nil {
		// 开着的时候删旧任务失败不致命：下面 Register 带 -Force 会直接覆盖
		log.Printf("[自启] 删除旧任务失败（继续用 -Force 覆盖）: %v", rmErr)
	}

	exe, err := os.Executable()
	if err != nil {
		return autostartGet(), err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return autostartGet(), err
	}
	if err = asTaskCreate(exe); err != nil {
		log.Printf("[自启] 计划任务创建失败，退回注册表启动项: %v", err)
		if err2 := asRunKeySet(exe); err2 != nil {
			return autostartGet(), fmt.Errorf("计划任务: %v；注册表启动项: %w", err, err2)
		}
	}
	return autostartGet(), nil
}

// asTaskCreate 用 S4U 注册开机任务：启动触发 + 20s 延迟（等磁盘服务就绪）、
// 不限执行时长（长跑服务不能被 72h 默认时限杀掉）、重复实例忽略、电池供电也启动。
// 工作目录设成 exe 所在目录，config.json 的默认位置才找得到。
func asTaskCreate(exe string) error {
	q := func(s string) string { return strings.ReplaceAll(s, "'", "''") }
	ps := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$action = New-ScheduledTaskAction -Execute '%s' -WorkingDirectory '%s'
$trigger = New-ScheduledTaskTrigger -AtStartup
$trigger.Delay = 'PT20S'
$me = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
$principal = New-ScheduledTaskPrincipal -UserId $me -LogonType S4U
$set = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName '%s' -Action $action -Trigger $trigger -Principal $principal -Settings $set -Force | Out-Null
`, q(exe), q(filepath.Dir(exe)), asTaskName)
	_, err := psRun(ps)
	return err
}

// asTaskRemove 删任务，任务不存在不算错。
func asTaskRemove() error {
	_, err := psRun(fmt.Sprintf(
		`$t = Get-ScheduledTask -TaskName '%s' -ErrorAction SilentlyContinue; `+
			`if ($t) { Unregister-ScheduledTask -TaskName '%s' -Confirm:$false }`,
		asTaskName, asTaskName))
	return err
}

func asRunKeyGet() string {
	out, err := psRun(fmt.Sprintf(
		`$v = (Get-ItemProperty -Path '%s' -Name '%s' -ErrorAction SilentlyContinue).'%s'; if ($v) { $v }`,
		asRunKey, asTaskName, asTaskName))
	if err != nil {
		return ""
	}
	return out
}

func asRunKeySet(exe string) error {
	_, err := psRun(fmt.Sprintf(
		`New-ItemProperty -Path '%s' -Name '%s' -Value '"%s"' -PropertyType String -Force | Out-Null`,
		asRunKey, asTaskName, strings.ReplaceAll(exe, "'", "''")))
	return err
}

func asRunKeyDelete() {
	// 删不掉就删不掉（多半是不存在），不报错
	_, _ = psRun(fmt.Sprintf(
		`Remove-ItemProperty -Path '%s' -Name '%s' -ErrorAction SilentlyContinue`,
		asRunKey, asTaskName))
}
