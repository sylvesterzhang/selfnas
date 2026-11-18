package main

// 外网访问：Cloudflare Tunnel 一条线，cloudflared 官方二进制编进本程序（2026-11-15 用户拍板）。
//
// 跟 tunnel_ts.go（Tailscale）的分工，别混：
//   Tailscale  = 自己用。点对点直连、跑满上行、SMB/WebDAV/上传全都能走；代价是
//                对面那台手机/电脑也得装 Tailscale 并登录同一账号。
//   Cloudflare = 发给别人看。别人只要有浏览器，打开就完；代价是只有 HTTP 这一层，
//                速度受 CF 中转限制，且**免费模式每次重启地址都会变**。
//
// 两种模式：
//   quick（默认）—— `cloudflared tunnel --url http://127.0.0.1:<port>`，不注册、不要账号，
//                  CF 随机分一个 https://xxx.trycloudflare.com。**进程重启就换新地址**，
//                  这是 quick tunnel 的规则，不是我们能改的。
//   named（进阶）—— 用户自己在 CF 控制台建一条隧道拿到 token，再把自己的域名 CNAME 到
//                  <隧道ID>.cfargotunnel.com，把 token 和域名填进来。地址永久固定。
//
// 为什么不 import cloudflared 的 Go 包，见 cfbundle.go 顶部。

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	cfModeQuick = "quick"
	cfModeNamed = "named"

	// quick tunnel 从起进程到拿到地址一般 3~8 秒；超过这个数基本就是连不上 CF 了
	// （企业防火墙、DNS 污染、UDP/TCP 443 被拦）。给宽一点，但必须有个头。
	cfReadyTimeout = 45 * time.Second
)

// trycloudflare 分配的主机名：小写字母/数字/连字符。
var cfURLRe = regexp.MustCompile(`https://[a-z0-9][a-z0-9-]*\.trycloudflare\.com`)

// cloudflared 日志里代表「隧道真的建起来了」的几行（named 模式没有 URL 可抓，靠它判断）。
var cfUpMarkers = []string{
	"Registered tunnel connection",
	"Connection registered",
}

// cloudflared 日志里的典型失败原因，翻译成人话。
var cfFailMap = []struct {
	keys []string
	msg  string
}{
	{[]string{"failed to request quick Tunnel", "dial tcp", "i/o timeout", "no such host"},
		"连不上 Cloudflare（网络或防火墙拦了 443 / DNS 解析失败）"},
	{[]string{"Unauthorized", "Invalid tunnel secret", "failed to unmarshal token"},
		"Tunnel Token 不对或已失效，去控制台重新复制"},
	{[]string{"tunnel not found", "No tunnel found"},
		"这条隧道在 Cloudflare 那边不存在（是不是被删了）"},
	{[]string{"failed to serve tunnel connection", "Unable to reach the origin service"},
		"隧道通了，但回不到本机服务（本程序可能刚重启，稍等再试）"},
}

var (
	cfMu    sync.Mutex
	cfCmd   *exec.Cmd
	cfState = "off" // off / starting / running / error
	cfURL   string
	cfMode  string
	cfErr   string
	cfSrc   string // builtin / system，前端据此显示「内置」还是「系统的」
	// cfGen 同 tsGen：启停一次 +1，旧的读日志协程发现代不上就自杀。
	cfGen int
	// cfPrev 是「这次启动之前」分到的地址，用来判断重启后地址是否变了。
	// 不能直接读 cfg.CfLastURL —— cfPump 抓到新地址后会立刻把它覆盖掉。
	cfPrev string
	cfLogs = &tsRing{max: 80}
)

// stripANSI 去掉终端颜色码：cloudflared 在部分 Windows 终端下会带 ANSI，
// 不清掉会让日志和错误提示里混进 "[36m" 这类垃圾。
var cfANSIRe = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

func cfClean(s string) string {
	s = cfANSIRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// cfTarget 转发目标。必须是本机回环 + 实际监听端口（effPort），
// 不是 cfg.Port —— 用 `-p` 起的测试实例也能正确指到自己。
func cfTarget() string {
	return fmt.Sprintf("http://127.0.0.1:%d", effPort)
}

type cfStartReq struct {
	Mode  string `json:"mode"`
	Host  string `json:"host"`
	Token string `json:"token"`
	// Auto 控制「开机自动恢复」。空值时按 true —— 用户点「开启」本身就意味着想让它一直开着。
	Auto *bool `json:"auto,omitempty"`
}

// cfStart 启动隧道。已经在跑或正在启动就直接返回，重复点按钮是安全的。
func cfStart(req cfStartReq) error {
	exe, src := cfFindExe()
	if exe == "" {
		return errors.New("这台机器上找不到 cloudflared（内置包缺失，系统 PATH 里也没有）")
	}

	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = cfModeQuick
	}

	var args []string
	var fixedURL string
	switch mode {
	case cfModeNamed:
		token := strings.TrimSpace(req.Token)
		if token == "" {
			return errors.New("固定地址模式需要 Cloudflare 的 Tunnel Token")
		}
		host := strings.TrimSpace(req.Host)
		host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
		host = strings.Trim(host, "/")
		if host == "" {
			return errors.New("固定地址模式需要一个你自己的域名（比如 nas.example.com）")
		}
		args = []string{"tunnel", "--no-autoupdate", "run", "--token", token}
		fixedURL = "https://" + host
	default:
		mode = cfModeQuick
		args = []string{"tunnel", "--url", cfTarget(), "--no-autoupdate",
			// QUIC(UDP 443) 在国内常被干扰，http2 走 TCP 反而更稳；
			// 实测 http2 下 6 秒左右就能拿到地址。
			"--protocol", "http2"}
	}

	cfMu.Lock()
	if cfState == "starting" || cfState == "running" {
		cfMu.Unlock()
		return nil
	}
	cmd := exec.Command(exe, args...)
	// 关掉 cloudflared 自己的自动更新：它更新会写自己的目录，而那是我们释放出来的缓存，
	// 下次启动被覆盖/校验失败反而添乱。版本跟着程序走。
	cmd.Env = append(os.Environ(), "NO_AUTOUPDATE=1", "TUNNEL_LOGLEVEL=info")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cfMu.Unlock()
		return err
	}
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		cfMu.Unlock()
		return fmt.Errorf("启动 cloudflared 失败：%w", err)
	}

	cfGen++
	gen := cfGen
	cfCmd = cmd
	cfState = "starting"
	cfErr = ""
	cfURL = fixedURL
	cfMode = mode
	cfSrc = src
	cfLogs.lines = nil
	// 本次启动之前分到的地址。抓到新地址后配置里会立刻被覆盖，
	// 所以必须先把旧的存一份下来，前端才知道「发出去的那个已经失效了」。
	cfPrev = cfg.CfLastURL
	cfMu.Unlock()

	log.Printf("[外网] cloudflared 已启动（%s 模式，%s）→ %s", mode, src, cfTarget())
	go cfPump(gen, cmd, stderr, fixedURL != "")
	go cfWatchdog(gen)
	return nil
}

// cfPump 读 cloudflared 的 stderr：抓地址、抓失败原因、等进程退出。
// cloudflared 把「分给你的域名」当普通日志打在 stderr（banner 框里），
// 没有机器可读的接口，所以只能抓文本 —— 这也是为什么版本要锁死，
// 上游一改日志格式我们就得跟着调。
func cfPump(gen int, cmd *exec.Cmd, stderr io.Reader, named bool) {
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	sawUp := false

	for sc.Scan() {
		line := cfClean(sc.Text())
		if line == "" {
			continue
		}
		cfMu.Lock()
		if cfGen != gen {
			cfMu.Unlock()
			return
		}
		cfLogs.add(line)
		// 只留真正有用的几行：cloudflared 启动期会刷几十行 DERP/QUIC 细节
		if u := cfURLRe.FindString(line); u != "" && cfURL == "" {
			cfURL = u
			cfState = "running"
			cfMu.Unlock()
			log.Printf("[外网] 临时地址已分配：%s", u)
			// 记下这条地址，重启后能对比出「变了」好提醒用户。
			// 必须解锁后再存：saveConfig 会动 cfg，别和 cfMu 交叉。
			cfRememberURL(u)
			continue
		}
		if !sawUp {
			for _, m := range cfUpMarkers {
				if strings.Contains(line, m) {
					sawUp = true
					if named {
						cfState = "running"
						log.Printf("[外网] 隧道已连上：%s", cfURL)
					}
					break
				}
			}
		}
		cfMu.Unlock()
	}

	// 进程退出：要么用户点了关闭，要么炸了
	waitErr := cmd.Wait()

	cfMu.Lock()
	defer cfMu.Unlock()
	if cfGen != gen {
		return
	}
	cfCmd = nil
	if cfState == "off" {
		// 主动关闭，stop() 已经把状态置好，这里什么都不用做
		return
	}
	if cfState == "error" && cfErr != "" {
		// 看门狗已经给出「连不上 / 超时」这类更准确的原因，别用 exit status 覆盖掉
		return
	}
	cfState = "error"
	cfURL = ""
	cfErr = cfExplain(fmt.Sprint(waitErr))
	log.Printf("[外网] cloudflared 退出：%s", cfErr)
}

// cfWatchdog 兜住「进程没死、但一直连不上」的情况：cloudflared 会安静地重试，
// 前端就永远停在「正在连接」。超过 cfReadyTimeout 直接判失败并杀掉，给出人话原因。
func cfWatchdog(gen int) {
	t := time.NewTimer(cfReadyTimeout)
	defer t.Stop()
	<-t.C
	cfMu.Lock()
	if cfGen != gen || cfState != "starting" {
		cfMu.Unlock()
		return
	}
	cfErr = fmt.Sprintf("%d 秒内没连上 Cloudflare：网络或防火墙拦了 443，或 DNS 解析不到 argotunnel.com", int(cfReadyTimeout.Seconds()))
	cfState = "error"
	cmd := cfCmd
	cfMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
	log.Printf("[外网] 连接超时：%s", cfErr)
}

// cfExplain 把 cloudflared 的报错翻译成人话。翻不出来就退回原始尾部日志，
// 至少让用户能搜得到。
func cfExplain(detail string) string {
	tail := strings.Join(cfLogs.tail(4), " / ")
	for _, f := range cfFailMap {
		for _, k := range f.keys {
			if strings.Contains(detail, k) || strings.Contains(tail, k) {
				return f.msg
			}
		}
	}
	if strings.Contains(detail, "exit status 1") || strings.Contains(detail, "<nil>") {
		if tail != "" {
			return "cloudflared 异常退出：" + tail
		}
		return "cloudflared 异常退出（没有更多信息）"
	}
	return "cloudflared 异常退出：" + detail
}

// cfStop 停掉隧道。进程杀掉即可 —— quick tunnel 关掉后 CF 那边自动回收地址。
func cfStop() {
	cfMu.Lock()
	cfGen++
	cmd := cfCmd
	cfCmd = nil
	cfState = "off"
	cfURL = ""
	cfErr = ""
	cfMode = ""
	cfMu.Unlock()

	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
	log.Printf("[外网] 已关闭 Cloudflare 隧道")
}

// cfSnapshot 给前端的状态快照。
func cfSnapshot() map[string]any {
	cfMu.Lock()
	defer cfMu.Unlock()

	d := map[string]any{
		"state":   cfState,
		"mode":    cfMode,
		"exe":     cfSrc,
		"version": cfBundleVersion,
	}
	if cfURL != "" {
		d["url"] = cfURL
		// named 模式下地址是固定的，不需要二维码反复扫；quick 才给。
		if cfMode == cfModeQuick {
			d["qr"] = cfQR(cfURL)
		}
	}
	if cfErr != "" {
		d["error"] = cfErr
	}
	// 上次的临时地址：重启后地址变了要让用户知道「发出去的旧地址失效了」
	if cfPrev != "" && cfMode == cfModeQuick {
		d["prevUrl"] = cfPrev
		if cfURL != "" && cfURL != cfPrev {
			d["changed"] = true
		}
	}
	if cfg.CfMode != "" {
		d["savedMode"] = cfg.CfMode
	}
	if cfg.CfHost != "" {
		d["savedHost"] = cfg.CfHost
	}
	d["hasToken"] = strings.TrimSpace(cfg.CfToken) != ""
	d["target"] = cfTarget()
	// 内置包是否存在，决定前端要不要提示「这台机器没有可用的 cloudflared」
	d["bundled"] = len(cfBundle) > 0
	return d
}

// cfRememberURL 记下这次的临时地址，重启后能对比出「变了」。
// 只在 quick 模式调用 —— named 模式的地址是用户的固定域名，没必记。
func cfRememberURL(u string) {
	if u == "" || cfg.CfLastURL == u {
		return
	}
	cfg.CfLastURL = u
	if err := saveConfig(); err != nil {
		log.Printf("[外网] 记录临时地址失败（不影响使用）: %v", err)
	}
}

// cfQR 复用 tsQR 的编码逻辑，只是换了个语义上的名字。
func cfQR(s string) string { return tsQR(s) }

// cfAutoStart 开机按上次的模式自动恢复。quick 会拿到**新地址**（CF 的规则），
// 这是预期行为，前端靠 prevUrl/changed 提示用户。
func cfAutoStart() {
	if !cfg.CfAuto {
		return
	}
	mode := cfg.CfMode
	if mode == "" {
		mode = cfModeQuick
	}
	req := cfStartReq{Mode: mode, Host: cfg.CfHost, Token: cfg.CfToken}
	if err := cfStart(req); err != nil {
		log.Printf("[外网] 自动开启失败：%v", err)
	}
	// 地址落定时 cfPump 会自动记到 cfg.CfLastURL，这里不用再等
}

// ---------- HTTP ----------

func handleCfStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 0, "ok", cfSnapshot())
}

// handleCfStart 开启外网访问。配置先落盘再启动 —— 用户填的 token 不能因为
// 「启动失败」而不见，下次还得重填一遍。
func handleCfStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	var b cfStartReq
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, 400, "参数格式不对", nil)
		return
	}
	mode := strings.ToLower(strings.TrimSpace(b.Mode))
	if mode == "" {
		mode = cfModeQuick
	}
	if mode == cfModeNamed {
		cfg.CfHost = strings.TrimSpace(b.Host)
		cfg.CfToken = strings.TrimSpace(b.Token)
	} else {
		mode = cfModeQuick
	}
	cfg.CfMode = mode
	cfg.CfAuto = true
	if b.Auto != nil {
		cfg.CfAuto = *b.Auto
	}
	if err := saveConfig(); err != nil {
		writeJSON(w, 500, "保存配置失败："+err.Error(), nil)
		return
	}

	if err := cfStart(b); err != nil {
		writeJSON(w, 400, err.Error(), nil)
		return
	}
	writeJSON(w, 0, "正在连接 Cloudflare，稍等几秒", nil)
}

// handleCfStop 关闭。地址立刻失效（quick 模式的地址本来就只在这一进程生命周期内有效）。
func handleCfStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	cfStop()
	cfg.CfAuto = false
	if err := saveConfig(); err != nil {
		writeJSON(w, 500, "已关闭，但保存配置失败："+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已关闭", nil)
}
