package main

// 远程访问（内网穿透）：Tailscale 一条线，并且把客户端**编进本程序**（2026-11-15 用户拍板）。
//
// 为什么不再调外部 tailscale.exe：
//   - 用户机器上十有八九没装，管理台只能甩一句「去下载安装包」，等于没做；
//   - 想免装就得随程序分发各平台安装包，五平台各一份，还要处理服务/注册表/权限，
//     用户卸载时还留一堆垃圾。
// 所以改用 Tailscale 官方给内嵌场景写的库 tailscale.com/tsnet：同一份源码（BSD-3），
// 编译进本程序后**就是一个完整的 Tailscale 节点**，不装客户端、不要管理员/root、
// 不建虚拟网卡（走用户态 netstack），五平台行为完全一致。
//
// 代价与边界（写清楚，别让用户猜）：
//   - 二进制 +21MB 左右（tsnet 带 wireguard-go + gvisor 用户态协议栈）；
//   - 仍然需要一个 Tailscale 账号（免费版够用），**首次要登录授权一次**：本程序会给出
//     一条 login.tailscale.com 的授权链接 + 二维码，手机扫一下登录即可，之后永久记住；
//   - 要用它的另一头（手机/电脑）也必须装 Tailscale —— 这是 WireGuard 组网的本质，
//     任何点对点方案都要求两端都有客户端，省不掉的只有「这台 NAS」这一头。
//   - 用户态 netstack 只服务我们主动 Listen 的端口（这里是 80），所以流量模型很干净：
//     只暴露站点本身，不会把整台机器的网络拉进 tailnet。
//
// 状态机：off（没开）→ starting → needslogin（等授权，带 authUrl）→ running；
// 出错落 error 并带一行原因。所有状态在内存里，重启后按 config.tsEnabled 自动恢复。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// tsPort 是「在 tailnet 上」监听的端口。它活在用户态 netstack 里，
// 跟本机 0.0.0.0:80 是两个命名空间，互不冲突 —— 所以这里固定 80，
// 换来的是一条干净的 http://100.x.y.z/files（不带端口，手机上好记好敲）。
const tsPort = 80

const tsDefaultHost = "selfnas"

// mainHandler 由 main() 在起服务时注入：tailnet 上收到的请求直接交给同一套路由，
// 权限、鉴权、限速全都和局域网访问完全一致，不需要第二套逻辑。
var mainHandler http.Handler

var (
	tsMu      sync.Mutex
	tsSrv     *tsnet.Server
	tsHTTP    *http.Server
	tsLn      net.Listener
	tsState   = "off" // off / starting / needslogin / needsauth / running / stopped / error
	tsErrText string
	tsAuthURL string
	tsIP      string
	tsDNS     string
	tsMode    string
	tsRelay   string
	tsOnline  bool
	tsPeers   int
	tsVer     string
	// tsGen 是「第几次启动」的代号。启停一次 +1，轮询协程发现自己代不上就自杀，
	// 免得上一次启动的协程把新一次的状态改写回旧的（点两次开启很容易撞上）。
	tsGen  int
	tsLogs = &tsRing{max: 60}
)

// tsRing 是 tsnet 日志的环形缓冲。tsnet 启动期会刷几十行（wireguard、DERP、netmap），
// 直接进 logs/ 会把有用的业务日志冲没，但出问题时又确实需要它 —— 所以只留内存，
// 出错时取末尾几行拼进 error 给用户看。
type tsRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func (r *tsRing) add(s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, s)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

func (r *tsRing) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) < n {
		n = len(r.lines)
	}
	out := make([]string, n)
	copy(out, r.lines[len(r.lines)-n:])
	return out
}

// tsStateDir 放节点身份（machine key / node key / 登录态）。
// 必须放在 cache 之外：缓存目录有容量清理逻辑，丢了身份就等于每次重启都要重新授权。
func tsStateDir() string { return filepath.Join(filepath.Dir(cfgPath), "tsnet") }

func tsHostOr() string {
	if h := strings.TrimSpace(cfg.TsHostname); h != "" {
		return h
	}
	return tsDefaultHost
}

// tsSanitizeHost 主机名会进 tailnet 的 DNS，只放行安全字符。
func tsSanitizeHost(h string) string {
	h = strings.TrimSpace(h)
	var b strings.Builder
	for _, c := range h {
		if c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			b.WriteRune(c)
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return tsDefaultHost
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// tsURL 拼 tailnet 上的访问地址。端口固定 80，所以不走 hostURL（那个跟 effPort 绑定）。
func tsURL(host string) string {
	if host == "" {
		return ""
	}
	return "http://" + host + "/files"
}

// tsStart 启动内嵌节点。已经在跑（或正在等授权）就直接返回，重复点击是安全的。
// authKey 留空 = 走交互登录（给出授权链接/二维码），这是默认路径。
func tsStart(hostname, authKey string) error {
	tsMu.Lock()
	switch tsState {
	case "running", "starting", "needslogin", "needsauth":
		tsMu.Unlock()
		return nil
	}
	host := tsSanitizeHost(hostname)
	srv := &tsnet.Server{
		Dir:      tsStateDir(),
		Hostname: host,
		AuthKey:  strings.TrimSpace(authKey),
		Logf: func(f string, a ...any) {
			tsLogs.add(fmt.Sprintf(f, a...))
		},
	}
	tsSrv = srv
	tsState = "starting"
	tsErrText = ""
	tsAuthURL = ""
	tsIP, tsDNS, tsMode, tsRelay, tsOnline = "", "", "", "", false
	tsPeers, tsVer = 0, ""
	tsGen++
	gen := tsGen
	tsMu.Unlock()

	// Start 只负责把后台组件拉起来（毫秒级），不等联网结果 ——
	// 等结果会卡住 HTTP 处理（首次登录可能要几分钟），状态交给下面的轮询。
	if err := srv.Start(); err != nil {
		tsMu.Lock()
		if tsGen == gen {
			tsState = "error"
			tsErrText = "启动内嵌节点失败：" + err.Error()
			tsSrv = nil
		}
		tsMu.Unlock()
		return err
	}
	log.Printf("[远程] 内嵌 Tailscale 节点启动，主机名 %s，状态目录 %s", host, tsStateDir())
	go tsWatch(gen, srv)
	return nil
}

// tsWatch 轮询本地后端状态，把结果翻译成前端要的几档。
// 3 秒一跳：授权时用户扫完码要几秒内就看到变化；平时开销可忽略（本地 IPC，不出网）。
func tsWatch(gen int, srv *tsnet.Server) {
	last := ""
	for {
		tsMu.Lock()
		alive := tsGen == gen
		tsMu.Unlock()
		if !alive {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		lc, err := srv.LocalClient()
		if err == nil {
			var st *ipnstate.Status
			st, err = lc.Status(ctx)
			if err == nil {
				tsApplyStatus(gen, srv, st)
				// 状态没变就别刷日志，tsnet 的状态查询每秒都在跑
				if st.BackendState != last {
					log.Printf("[远程] 状态: %s", st.BackendState)
					last = st.BackendState
				}
			}
		}
		cancel()
		if err != nil && context.Canceled != err {
			tsLogs.add("status: " + err.Error())
		}
		time.Sleep(3 * time.Second)
	}
}

// tsApplyStatus 把 ipnstate.Status 落到包级变量上，并在首次 Running 时开监听。
func tsApplyStatus(gen int, srv *tsnet.Server, st *ipnstate.Status) {
	state := tsStateFromBackend(st)

	ip := ""
	for _, a := range st.TailscaleIPs {
		if a.Is4() {
			ip = a.String()
			break
		}
	}
	dns := ""
	mode, relay := "", ""
	online := false
	if st.Self != nil {
		dns = strings.TrimSuffix(st.Self.DNSName, ".")
		if st.Self.CurAddr != "" {
			mode = "direct"
		} else if st.Self.Relay != "" {
			mode = "relay"
			relay = st.Self.Relay
		}
		online = st.Self.Online
		if ip == "" {
			for _, a := range st.Self.TailscaleIPs {
				if a.Is4() {
					ip = a.String()
					break
				}
			}
		}
	}
	peers := 0
	for _, p := range st.Peer {
		if p.Online {
			peers++
		}
	}
	ver := st.Version
	if i := strings.Index(ver, "-"); i > 0 {
		ver = ver[:i]
	}

	tsMu.Lock()
	if tsGen != gen {
		tsMu.Unlock()
		return
	}
	tsState = state
	tsAuthURL = st.AuthURL
	tsIP, tsDNS, tsMode, tsRelay, tsOnline, tsPeers, tsVer = ip, dns, mode, relay, online, peers, ver
	// 只在「首次连上」时开监听；已经开着就一直复用（重连不断线）
	needListen := state == "running" && tsLn == nil && srv != nil
	tsMu.Unlock()

	if needListen {
		tsListen(gen, srv)
	}
}

func tsStateFromBackend(st *ipnstate.Status) string {
	switch st.BackendState {
	case ipn.Running.String():
		return "running"
	case ipn.NeedsLogin.String(), ipn.NoState.String():
		return "needslogin"
	case ipn.NeedsMachineAuth.String():
		// 设备进了 tailnet，但管理员还要在控制台点一下批准
		return "needsauth"
	case ipn.Starting.String():
		return "starting"
	case ipn.Stopped.String():
		return "stopped"
	}
	return "starting"
}

// tsListen 在 netstack 上监听 80，把请求原样交给主路由。
func tsListen(gen int, srv *tsnet.Server) {
	ln, err := srv.Listen("tcp", fmt.Sprintf(":%d", tsPort))
	if err != nil {
		tsMu.Lock()
		if tsGen == gen {
			tsState = "error"
			tsErrText = "在 tailnet 上监听失败：" + err.Error()
			tsHTTP = nil
		}
		tsMu.Unlock()
		return
	}
	hs := &http.Server{Handler: mainHandler, ReadHeaderTimeout: 10 * time.Second}

	tsMu.Lock()
	if tsGen != gen {
		tsMu.Unlock()
		ln.Close()
		return
	}
	tsLn = ln
	tsHTTP = hs
	url := tsURL(tsIP)
	tsMu.Unlock()

	log.Printf("[远程] 已接入 tailnet，外网地址 %s", url)
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			tsLogs.add("serve: " + err.Error())
		}
	}()
}

// tsStop 停止节点（保留身份，下次开启不用重新授权）。
func tsStop() {
	tsMu.Lock()
	tsGen++
	hs, ln, srv := tsHTTP, tsLn, tsSrv
	tsHTTP, tsLn, tsSrv = nil, nil, nil
	tsState = "off"
	tsErrText, tsAuthURL, tsIP, tsDNS, tsMode, tsRelay = "", "", "", "", "", ""
	tsOnline, tsPeers, tsVer = false, 0, ""
	tsMu.Unlock()

	if hs != nil && hs.Handler != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		hs.Shutdown(ctx)
		cancel()
	}
	if ln != nil {
		ln.Close()
	}
	if srv != nil {
		srv.Close()
	}
	log.Printf("[远程] 已断开内嵌 Tailscale 节点")
}

// tsSnapshot 取一份状态快照给前端（不加锁外泄任何内部对象）。
func tsSnapshot() map[string]any {
	tsMu.Lock()
	defer tsMu.Unlock()
	d := map[string]any{
		"embedded": true,
		"state":    tsState,
		"hostname": tsHostOr(),
		"port":     tsPort,
		"online":   tsOnline,
		"peers":    tsPeers,
	}
	if tsIP != "" {
		d["ip"] = tsIP
		d["url"] = tsURL(tsIP)
	}
	if tsDNS != "" {
		d["dnsName"] = tsDNS
	}
	if tsMode != "" {
		d["mode"] = tsMode
	}
	if tsRelay != "" {
		d["relay"] = tsRelay
	}
	if tsVer != "" {
		d["version"] = tsVer
	}
	if tsAuthURL != "" {
		d["authUrl"] = tsAuthURL
	}
	if tsErrText != "" {
		d["error"] = tsErrText
	} else if tsState == "error" {
		t := tsLogs.tail(3)
		if len(t) > 0 {
			d["error"] = strings.Join(t, " / ")
		}
	}
	return d
}

// handleTunnelStatus 当前状态 + 外网访问地址 + 二维码（访问码 / 授权码各一张）。
func handleTunnelStatus(w http.ResponseWriter, r *http.Request) {
	data := tsSnapshot()
	data["platform"] = runtime.GOOS
	data["enabled"] = cfg.TsEnabled
	data["lan"] = lanIPv4()
	// 家里还是走局域网，Tailscale 只解决「在外面」
	data["lanURL"] = hostURL(qrHost(), "/files")

	if u, _ := data["url"].(string); u != "" {
		data["qr"] = tsQR(u)
	}
	if a, _ := data["authUrl"].(string); a != "" {
		data["authQr"] = tsQR(a)
	}
	writeJSON(w, 0, "ok", data)
}

func tsQR(s string) string {
	png, err := qrcode.Encode(s, qrcode.Medium, 512)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

// handleTunnelUp 开启远程访问。
// authKey 是可选的高级用法（自动化/无人值守），留空就走「扫二维码授权」。
func handleTunnelUp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	var b struct {
		AuthKey  string `json:"authKey"`
		Hostname string `json:"hostname"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, 400, "参数格式不对", nil)
		return
	}
	key := strings.TrimSpace(b.AuthKey)
	if key != "" && !strings.HasPrefix(key, "tskey-") {
		writeJSON(w, 400, "这不像 authKey（应该以 tskey- 开头）", nil)
		return
	}
	host := tsSanitizeHost(b.Hostname)
	if host != tsHostOr() {
		cfg.TsHostname = host
	}
	cfg.TsEnabled = true
	if err := saveConfig(); err != nil {
		writeJSON(w, 500, "保存配置失败："+err.Error(), nil)
		return
	}
	if err := tsStart(host, key); err != nil {
		writeJSON(w, 500, "开启失败："+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已开始接入，稍等几秒", nil)
}

// handleTunnelDown 断开（不注销设备，身份留在本地，下次开启立刻回来）。
func handleTunnelDown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	tsStop()
	cfg.TsEnabled = false
	if err := saveConfig(); err != nil {
		writeJSON(w, 500, "断开已完成，但保存配置失败："+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已断开", nil)
}

// tsActiveIP 这台机器在 tailnet 上的地址：优先内嵌节点，其次系统里另外装的官方客户端
// （用户自己装了并登录，也认它 —— 概述页那个「已连接/未连接」和二维码里都用到）。
func tsActiveIP() (bool, string) {
	tsMu.Lock()
	st, embIP := tsState, tsIP
	tsMu.Unlock()
	if st == "running" && embIP != "" {
		return true, embIP
	}
	if up, ip := tailscaleStatus(); up && ip != "" {
		return true, ip
	}
	return false, ""
}

// tsAutoStart 开机按上次的状态自动恢复。
// 放在 goroutine 里且延后几秒：服务刚起步时网卡/DNS 可能还没就绪，
// 立刻去连控制面容易一次失败就停在 error。
func tsAutoStart() {
	if !cfg.TsEnabled {
		return
	}
	time.Sleep(3 * time.Second)
	if err := tsStart(cfg.TsHostname, ""); err != nil {
		log.Printf("[远程] 自动恢复失败：%v", err)
	}
}
