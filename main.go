package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "0.19.3"

//go:embed web
var webFS embed.FS

// Root 一个可访问的根目录
type Root struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
	Enabled  bool   `json:"enabled"`
}

// Config 全部配置，落盘为 config.json（原子替换写入）
type Config struct {
	Port              int    `json:"port"`
	Bind              string `json:"bind"`
	User              string `json:"user"`
	PassHash          string `json:"passHash"`
	Secret            string `json:"secret"`
	Roots             []Root `json:"roots"`
	AdminOnlyLocal    bool   `json:"adminOnlyLocal"`
	UploadConcurrency int    `json:"uploadConcurrency"`
	SessionDays       int    `json:"sessionDays"`
	// 可留空：留空时按「几个常见安装位置 + PATH」自动找。MKV 这类文件要靠它换封装才能播。
	FFmpeg string `json:"ffmpeg"`
}

// DriveInfo 系统盘符信息（仅用于展示与首次生成默认配置）
type DriveInfo struct {
	Letter     string `json:"letter"`
	Path       string `json:"path"`
	Label      string `json:"label"`
	Kind       string `json:"kind"`
	TotalBytes uint64 `json:"totalBytes"`
	FreeBytes  uint64 `json:"freeBytes"`
	Ready      bool   `json:"ready"`
}

var (
	cfg       Config
	cfgMu     sync.Mutex
	cfgPath   string
	startedAt = time.Now()
)

func main() {
	var port int
	var dbPath string
	flag.StringVar(&cfgPath, "c", "", "配置文件路径（默认与 exe 同目录 config.json）")
	flag.IntVar(&port, "p", 0, "监听端口（覆盖配置文件）")
	flag.StringVar(&dbPath, "db", "", "数据库文件路径（默认 config.json 同目录 selfnas.db），测试用独立库时指定")
	flag.Parse()

	if cfgPath == "" {
		exe, err := os.Executable()
		if err != nil {
			log.Fatalf("无法获取程序路径: %v", err)
		}
		cfgPath = filepath.Join(filepath.Dir(exe), "config.json")
	}
	cfgPath, _ = filepath.Abs(cfgPath)

	// 必须在读配置之前初始化：配置损坏这类启动期故障也要落到文件里
	initLogging(filepath.Join(filepath.Dir(cfgPath), "logs"))

	firstRun, err := loadConfig()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if port > 0 {
		cfg.Port = port
	}
	if firstRun {
		log.Printf("首次启动，已生成配置: %s", cfgPath)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Bind, cfg.Port)
	initCacheDir()
	// 「最近 / 收藏」的库，默认放在 config.json 旁边（跟配置一起备份就完事）；
	// 测试用 -db 指到别处，避免自动化脚本清场时把正式数据一起抹掉。
	if dbPath == "" {
		dbPath = filepath.Join(filepath.Dir(cfgPath), "selfnas.db")
	}
	dbPath, _ = filepath.Abs(dbPath)
	if err := openDBAt(dbPath); err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer closeDB()
	printBanner(firstRun)

	srv := &http.Server{
		Addr:              addr,
		Handler:           routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Ctrl+C / SIGTERM 时先停 HTTP、再关库（defer closeDB 会做 checkpoint）。
	//
	// 别用「任务管理器结束进程」或 taskkill /F 停服务：那是 TerminateProcess，
	// 进程当场消失，defer 一行都不会执行，数据库可能停在
	// 「主库改了一半、WAL 还没合并」的状态 —— 2026-10-27 就是这么丢过数据。
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Println("[退出] 收到退出信号，正在关闭（最多等 8 秒）")
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("[退出] 关闭 HTTP 出错: %v", err)
		}
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("服务启动失败（端口可能被占用）: %v", err)
	}
	log.Println("[退出] 已关闭")
}

func printBanner(firstRun bool) {
	lan := lanIPv4()
	lines := []string{
		"",
		"================================================",
		"  selfnas " + version,
		"  管理台:  " + hostURL("127.0.0.1", "/admin"),
	}
	if lan != "" {
		lines = append(lines, "  局域网:  "+hostURL(lan, "/admin"))
	}
	if why := browserBlockedPort(cfg.Port); why != "" {
		lines = append(lines, "  ⚠ 端口 "+itoa(cfg.Port)+" "+why+"，浏览器会直接拒绝访问（ERR_UNSAFE_PORT），请换端口")
	}
	lines = append(lines,
		"  配置:    "+cfgPath,
		"  数据:    "+dbFile,
		"  账号:    "+cfg.User,
	)
	if firstRun {
		lines = append(lines, "  密码:    见下方 [首次密码] 一行（请立即在管理台修改）")
	}
	lines = append(lines, "================================================", "")
	fmt.Println(strings.Join(lines, "\n"))
}

// browserBlockedPort 判断端口是否在 Chrome/Edge 的禁用端口表里。
// 6000 就是 X11 的端口，浏览器连都不连，直接给 ERR_UNSAFE_PORT ——
// 服务端看起来完全正常（curl 200），只有浏览器打不开，很容易查半天。
func browserBlockedPort(port int) string {
	if port == 6000 {
		return "是 X11 默认端口，被 Chromium 系浏览器禁用"
	}
	blocked := map[int]string{
		1: "是 TCPMUX", 7: "是 echo", 9: "是 discard", 11: "是 systat", 13: "是 daytime",
		15: "是 netstat", 17: "是 qotd", 19: "是 chargen", 20: "是 ftp-data", 21: "是 ftp",
		22: "是 ssh", 23: "是 telnet", 25: "是 smtp", 37: "是 time", 42: "是 nameserver",
		43: "是 whois", 53: "是 dns", 69: "是 tftp", 79: "是 finger", 87: "是任何私有服务",
		95: "是 supdup", 101: "是 hostname", 102: "是 iso-tsap", 103: "是 gppitnp",
		104: "是 acr-nema", 109: "是 pop2", 110: "是 pop3", 111: "是 sunrpc", 113: "是 auth",
		115: "是 sftp", 117: "是 uucp-path", 119: "是 nntp", 123: "是 ntp", 135: "是 epmap",
		137: "是 netbios-ns", 139: "是 netbios-ssn", 143: "是 imap", 161: "是 snmp",
		179: "是 bgp", 389: "是 ldap", 427: "是 svrloc", 465: "是 smtps", 512: "是 exec",
		513: "是 login", 514: "是 shell", 515: "是 printer", 526: "是 tempo", 530: "是 courier",
		531: "是 chat", 532: "是 netnews", 540: "是 uucp", 548: "是 afp", 554: "是 rtsp",
		556: "是 remotefs", 563: "是 nntps", 587: "是 submission", 601: "是 syslog-conn",
		636: "是 ldaps", 989: "是 ftps-data", 990: "是 ftps", 993: "是 imaps", 995: "是 pop3s",
		1719: "是 h323gatestat", 1720: "是 h323hostcall", 1723: "是 pptp", 2049: "是 nfs",
		3659: "是 apple-sasl", 4045: "是 npp", 5060: "是 sip", 5061: "是 sips",
		6566: "是 sane-port", 6697: "是 ircs-u", 10080: "是 amanda",
	}
	if port >= 6665 && port <= 6669 {
		return "是 IRC 端口，被 Chromium 系浏览器禁用"
	}
	return blocked[port]
}

// ---------- 配置读写 ----------

func loadConfig() (bool, error) {
	cfgMu.Lock()
	defer cfgMu.Unlock()

	data, err := os.ReadFile(cfgPath)
	if os.IsNotExist(err) {
		return true, bootstrapConfig()
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		// 尝试回退备份
		if bak, e2 := os.ReadFile(cfgPath + ".bak"); e2 == nil {
			if e3 := json.Unmarshal(bak, &cfg); e3 == nil {
				log.Printf("配置解析失败，已回退到 .bak: %v", err)
				return false, nil
			}
		}
		return false, fmt.Errorf("配置损坏且无可用备份: %v", err)
	}
	return false, normalizeConfig()
}

func bootstrapConfig() error {
	pw := randomPassword(14)
	cfg = Config{
		Port:              8080,
		Bind:              "0.0.0.0",
		User:              "admin",
		PassHash:          hashPassword(pw),
		Secret:            randomHex(32),
		AdminOnlyLocal:    true,
		UploadConcurrency: 2,
		SessionDays:       3650,
	}
	for _, d := range listDrives() {
		if !d.Ready {
			continue
		}
		name := d.Label
		if name == "" {
			name = "磁盘 " + d.Letter
		}
		cfg.Roots = append(cfg.Roots, Root{
			Name:    name,
			Path:    d.Path,
			Enabled: d.Letter != "C",
		})
	}
	if err := saveConfigLocked(); err != nil {
		return err
	}
	fmt.Println("[首次密码] " + pw)
	return nil
}

func normalizeConfig() error {
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return fmt.Errorf("端口非法: %d", cfg.Port)
	}
	if cfg.Bind == "" {
		cfg.Bind = "0.0.0.0"
	}
	if cfg.Secret == "" {
		cfg.Secret = randomHex(32)
	}
	if cfg.SessionDays <= 0 {
		cfg.SessionDays = 3650
	}
	if cfg.UploadConcurrency <= 0 {
		cfg.UploadConcurrency = 2
	}
	seen := map[string]bool{}
	out := cfg.Roots[:0]
	for _, r := range cfg.Roots {
		r.Path = strings.TrimSpace(r.Path)
		if r.Path == "" || seen[strings.ToLower(r.Path)] {
			continue
		}
		seen[strings.ToLower(r.Path)] = true
		if r.Name == "" {
			r.Name = r.Path
		}
		out = append(out, r)
	}
	cfg.Roots = out
	return nil
}

func saveConfig() error {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return saveConfigLocked()
}

func saveConfigLocked() error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := cfgPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := os.Stat(cfgPath); err == nil {
		if old, e := os.ReadFile(cfgPath); e == nil {
			_ = os.WriteFile(cfgPath+".bak", old, 0o600)
		}
	}
	return os.Rename(tmp, cfgPath)
}

// ---------- 口令与令牌 ----------

func hashPassword(pw string) string {
	salt := randomBytes(16)
	dk := pbkdf2SHA256([]byte(pw), salt, 120000, 32)
	return fmt.Sprintf("pbkdf2$120000$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk))
}

func verifyPassword(pw, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	var iter int
	if _, err := fmt.Sscanf(parts[1], "%d", &iter); err != nil || iter <= 0 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	blocks := (keyLen + hLen - 1) / hLen
	dk := make([]byte, 0, blocks*hLen)
	u := make([]byte, hLen)
	t := make([]byte, hLen)
	var buf [4]byte
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf[:])
		u = prf.Sum(u[:0])
		copy(t, u)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range t {
				t[i] ^= u[i]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

func signToken(user string, exp int64) string {
	payload := fmt.Sprintf("%s|%d", user, exp)
	mac := hmac.New(sha256.New, []byte(cfg.Secret))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func verifyToken(tok string) bool {
	i := strings.LastIndex(tok, ".")
	if i <= 0 {
		return false
	}
	payloadB, err := base64.RawURLEncoding.DecodeString(tok[:i])
	if err != nil {
		return false
	}
	sigB, err := base64.RawURLEncoding.DecodeString(tok[i+1:])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(cfg.Secret))
	mac.Write(payloadB)
	if !hmac.Equal(mac.Sum(nil), sigB) {
		return false
	}
	parts := strings.Split(string(payloadB), "|")
	if len(parts) != 2 || parts[0] != cfg.User {
		return false
	}
	var exp int64
	if _, err := fmt.Sscanf(parts[1], "%d", &exp); err != nil {
		return false
	}
	return time.Now().Unix() < exp
}

// tokenExp 取出令牌里的过期时间（签名已由 verifyToken 校验过）
func tokenExp(tok string) (int64, bool) {
	i := strings.LastIndex(tok, ".")
	if i <= 0 {
		return 0, false
	}
	payloadB, err := base64.RawURLEncoding.DecodeString(tok[:i])
	if err != nil {
		return 0, false
	}
	parts := strings.Split(string(payloadB), "|")
	if len(parts) != 2 {
		return 0, false
	}
	var exp int64
	if _, err := fmt.Sscanf(parts[1], "%d", &exp); err != nil || exp <= 0 {
		return 0, false
	}
	return exp, true
}

// 浏览器（Chrome/Safari）对单个 Cookie 的生命周期有 400 天硬上限，
// 无论我们把 MaxAge 写多大都会被截断。所以有效期想设十年，靠的是「每次访问都续期」：
// 令牌本身的 exp 按用户设置走，Cookie 的 MaxAge 取两者较小值，一访问就重签。
const cookieMaxAgeSec = 400 * 24 * 3600

func sessionTotalSec() int64 {
	n := int64(cfg.SessionDays) * 86400
	if n <= 0 {
		n = 3650 * 86400
	}
	return n
}

// setSessionCookie 下发/刷新会话 Cookie，返回令牌的过期时间戳
func setSessionCookie(w http.ResponseWriter) int64 {
	total := sessionTotalSec()
	exp := time.Now().Unix() + total
	maxAge := total
	if maxAge > cookieMaxAgeSec {
		maxAge = cookieMaxAgeSec
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "sn_token",
		Value:    signToken(cfg.User, exp),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(maxAge),
	})
	return exp
}

// authedRenew 校验登录态，并在「剩余有效期不足一半」时自动续签。
// 这样即使浏览器把 Cookie 截到 400 天，只要用户偶尔用一次就永远不会掉登录。
func authedRenew(w http.ResponseWriter, r *http.Request) bool {
	c, err := r.Cookie("sn_token")
	if err != nil {
		return false
	}
	if !verifyToken(c.Value) {
		return false
	}
	if exp, ok := tokenExp(c.Value); ok {
		if exp-time.Now().Unix() < sessionTotalSec()/2 {
			setSessionCookie(w)
		}
	}
	return true
}

// ---------- 登录失败锁定 ----------

type lockEntry struct {
	fails int
	until time.Time
}

var (
	lockMu sync.Mutex
	locks  = map[string]*lockEntry{}
)

func isLocked(ip string) bool {
	lockMu.Lock()
	defer lockMu.Unlock()
	e, ok := locks[ip]
	if !ok {
		return false
	}
	if time.Now().After(e.until) {
		delete(locks, ip)
		return false
	}
	return e.fails >= 5
}

func markFail(ip string) {
	lockMu.Lock()
	defer lockMu.Unlock()
	e, ok := locks[ip]
	if !ok {
		e = &lockEntry{}
		locks[ip] = e
	}
	e.fails++
	if e.fails >= 5 {
		e.until = time.Now().Add(60 * time.Second)
	}
}

func clearFail(ip string) {
	lockMu.Lock()
	defer lockMu.Unlock()
	delete(locks, ip)
}

// ---------- 随机 ----------

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func randomHex(n int) string { return hex.EncodeToString(randomBytes(n)) }

func randomPassword(n int) string {
	const charset = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := randomBytes(n)
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

// ---------- 网络工具 ----------

func lanIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(iface.Name)
		if strings.Contains(name, "tailscale") || strings.Contains(name, "tap") ||
			strings.Contains(name, "vmware") || strings.Contains(name, "vethernet") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			return ip4.String()
		}
	}
	return ""
}

func tailscaleStatus() (bool, string) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false, ""
	}
	for _, iface := range ifaces {
		if !strings.Contains(strings.ToLower(iface.Name), "tailscale") {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				return iface.Flags&net.FlagUp != 0, ip4.String()
			}
		}
		return iface.Flags&net.FlagUp != 0, ""
	}
	return false, ""
}

// clientIP 取请求来源 IP（不信任 XFF，仅本机反代场景才需要）
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isLocalOrTailscale(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// Tailscale CGNAT 段 100.64.0.0/10
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// ---------- 路由 ----------

func routes() http.Handler {
	mux := http.NewServeMux()

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("内嵌资源异常: %v", err)
	}
	// 资源是内嵌的，没有 Last-Modified/ETag 可校验；iOS Safari 会启发式缓存，
	// 导致升级后手机还在跑旧的 files.js。本地读取代价可忽略，一律禁缓存。
	assets := http.StripPrefix("/assets/", http.FileServer(http.FS(sub)))
	mux.Handle("/assets/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		assets.ServeHTTP(w, r)
	}))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			// 默认进文件站（手机扫码/输入 IP 直接可用）；管理台在右上角齿轮进入
			http.Redirect(w, r, "/files", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc("/admin", adminPageHandler)
	// 没有图标就明确回 204，避免每个页面都刷一条 404 噪音进日志
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/s/", handleScan)
	mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
		serveEmbedded(w, sub, "files.html")
	})

	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/logout", handleLogout)
	mux.HandleFunc("/api/state", guard(handleState))
	mux.HandleFunc("/api/roots/add", guard(handleRootAdd))
	mux.HandleFunc("/api/roots/update", guard(handleRootUpdate))
	mux.HandleFunc("/api/roots/delete", guard(handleRootDelete))
	mux.HandleFunc("/api/password", guard(handlePassword))
	mux.HandleFunc("/api/drives", guard(handleDrives))
	mux.HandleFunc("/api/browse", guard(handleBrowse))
	mux.HandleFunc("/api/settings", guard(handleSettings))
	mux.HandleFunc("/api/qr/token", guard(handleQRToken))
	mux.HandleFunc("/api/logs/tail", guard(handleLogTail))
	mux.HandleFunc("/api/logs/transfers", guard(handleTransferList))

	// 文件站接口：只校验登录态，不做「仅本机」限制（手机要能用）
	mux.HandleFunc("/api/fs/roots", guardAuth(handleFSRoots))
	mux.HandleFunc("/api/fs/list", guardAuth(handleFSList))
	mux.HandleFunc("/api/fs/download", guardAuth(handleFSDownload))
	// MKV/AVI/TS 这类 iOS 播不了的容器，实时换封装成 MP4 流
	mux.HandleFunc("/api/fs/play", guardAuth(handleFSPlay))
	mux.HandleFunc("/api/fs/playinfo", guardAuth(handleFSPlayInfo))
	// 换封装结果落盘成 MP4 缓存 → 能拖进度、能秒开（流式那条路做不到）
	mux.HandleFunc("/api/fs/prep", guardAuth(handleFSPrep))
	mux.HandleFunc("/api/fs/cache/", guardAuth(handleFSCacheFile))
	// 给外部播放器（VLC/Infuse/nPlayer）的短期签名直链 —— 它们自带解码器，MKV 直接播
	mux.HandleFunc("/api/fs/link", guardAuth(handleFSLink))
	mux.HandleFunc("/f", handleSignedFile)
	mux.HandleFunc("/api/fs/upload", guardAuth(handleFSUpload))
	mux.HandleFunc("/api/fs/upload/status", guardAuth(handleFSUploadStatus))
	mux.HandleFunc("/api/fs/mkdir", guardAuth(handleFSMkdir))
	mux.HandleFunc("/api/fs/rename", guardAuth(handleFSRename))
	mux.HandleFunc("/api/fs/move", guardAuth(handleFSMove))
	mux.HandleFunc("/api/fs/delete", guardAuth(handleFSDelete))
	// 手机端把关键事件回报到服务端日志（change 是否触发、选到几个文件）
	mux.HandleFunc("/api/fs/clientlog", guardAuth(handleClientLog))

	// 最近 / 收藏：存服务端 SQLite，跟账号绑定（换设备还在）
	mux.HandleFunc("/api/fs/recent", guardAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			handleRecentAdd(w, r)
			return
		}
		handleRecentList(w, r)
	}))
	mux.HandleFunc("/api/fs/recent/remove", guardAuth(handleRecentRemove))
	mux.HandleFunc("/api/fs/recent/clear", guardAuth(handleRecentClear))
	mux.HandleFunc("/api/fs/fav", guardAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			handleFavToggle(w, r)
			return
		}
		handleFavList(w, r)
	}))
	mux.HandleFunc("/api/fs/stats", guardAuth(handleStats))

	return withLogging(mux)
}

func serveEmbedded(w http.ResponseWriter, sub fs.FS, name string) {
	data, err := fs.ReadFile(sub, name)
	if err != nil {
		http.Error(w, "资源缺失: "+name, http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	if strings.HasSuffix(name, ".html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.Write(data)
}

func adminPageHandler(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if cfg.AdminOnlyLocal && !isLocalOrTailscale(ip) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "<html><body style=\"font-family:sans-serif;padding:40px\"><h3>管理台禁止从当前地址访问</h3><p>来源 %s。管理台默认只允许本机与 Tailscale 访问，可在 config.json 中把 adminOnlyLocal 改为 false。</p></body></html>", ip)
		return
	}
	sub, _ := fs.Sub(webFS, "web")
	serveEmbedded(w, sub, "admin.html")
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(lw, r)
		el := time.Since(start)

		p := r.URL.Path
		// 静态资源量大且无诊断价值，只在出错时记
		if (strings.HasPrefix(p, "/assets/") || p == "/favicon.ico") && lw.status < 400 {
			return
		}
		// 上传/下载天然长耗时且已有专门的传输记录，这里只记异常
		if (p == "/api/fs/upload" || p == "/api/fs/download") && lw.status < 400 {
			return
		}
		log.Printf("%s %s %s -> %d %dB %s ua=%s",
			clientIP(r), r.Method, r.URL.RequestURI(), lw.status,
			lw.bytes, el.Round(time.Millisecond), uaTag(r.UserAgent()))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// guard 管理类接口的鉴权 + 来源限制
func guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if cfg.AdminOnlyLocal && !isLocalOrTailscale(ip) {
			writeJSON(w, 403, "管理接口仅允许本机或 Tailscale 访问", nil)
			return
		}
		if !authedRenew(w, r) {
			writeJSON(w, 401, "未登录", nil)
			return
		}
		h(w, r)
	}
}

// guardAuth 仅校验登录态（文件站接口用，手机在局域网也要能访问）
func guardAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authedRenew(w, r) {
			writeJSON(w, 401, "未登录", nil)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": code,
		"msg":  msg,
		"data": data,
	})
}
