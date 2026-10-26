package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	logRingMax     = 1200            // 内存里保留的行数
	logKeepDays    = 14              // 日志文件保留天数
	transferMaxLen = 24 << 20        // transfers.jsonl 超过 24MB 轮转
)

// logHub 同时承担三件事：
//  1. 作为 log 包的输出端，落盘到 logs/selfnas-YYYY-MM-DD.log
//  2. 在内存里留一份环形缓冲，管理台不必读盘就能看最近日志
//  3. 按天自动切换文件，并清理过期日志
//
// 之前日志只打 stdout，而程序由计划任务启动、没有控制台，
// 等于全部丢弃——出问题时完全查不到。
type logHub struct {
	mu   sync.Mutex
	dir  string
	day  string
	f    *os.File
	ring []string
}

var logDir string
var logH *logHub

func initLogging(dir string) {
	logDir = dir
	logH = &logHub{dir: dir}
	log.SetOutput(io.MultiWriter(os.Stdout, logH))
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	_ = os.MkdirAll(dir, 0o755)
	log.Printf("[启动] selfnas %s 启动，日志目录 %s", version, dir)
}

func (h *logHub) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	day := time.Now().Format("2006-01-02")
	if h.f == nil || day != h.day {
		h.rotate(day)
	}
	if h.f != nil {
		_, _ = h.f.Write(p)
		_ = h.f.Sync() // 崩溃/断电也不丢最后几行，代价可接受（日志量很小）
	}
	for _, ln := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		h.ring = append(h.ring, ln)
	}
	if len(h.ring) > logRingMax*2 {
		h.ring = append([]string(nil), h.ring[len(h.ring)-logRingMax:]...)
	}
	return len(p), nil
}

func (h *logHub) rotate(day string) {
	if h.f != nil {
		h.f.Close()
		h.f = nil
	}
	h.day = day
	p := filepath.Join(h.dir, "selfnas-"+day+".log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	h.f = f
	h.prune()
}

// prune 删掉超过保留期的日志，避免无限增长
func (h *logHub) prune() {
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		return
	}
	cut := time.Now().AddDate(0, 0, -logKeepDays)
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, "selfnas-") || !strings.HasSuffix(n, ".log") {
			continue
		}
		d, err := time.Parse("2006-01-02", strings.TrimSuffix(strings.TrimPrefix(n, "selfnas-"), ".log"))
		if err != nil {
			continue
		}
		if d.Before(cut) {
			_ = os.Remove(filepath.Join(h.dir, n))
		}
	}
}

// Tail 取最近 n 行
func (h *logHub) Tail(n int) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n <= 0 || n > logRingMax {
		n = 200
	}
	start := len(h.ring) - n
	if start < 0 {
		start = 0
	}
	out := make([]string, len(h.ring)-start)
	copy(out, h.ring[start:])
	return out
}

// ---------- 传输记录 ----------

type transferRec struct {
	T      string `json:"t"`
	Kind   string `json:"kind"` // upload / download / delete / mkdir / rename
	IP     string `json:"ip,omitempty"`
	UA     string `json:"ua,omitempty"`
	Root   string `json:"root,omitempty"`
	Path   string `json:"path"`
	Size   int64  `json:"size,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	OK     bool   `json:"ok"`
	Note   string `json:"note,omitempty"`
	MS     int64  `json:"ms,omitempty"`
}

var transferMu sync.Mutex

// recordTransfer 追加一行到 logs/transfers.jsonl（JSON Lines，便于事后 grep/脚本处理）
func recordTransfer(rec transferRec) {
	if logDir == "" {
		return
	}
	if rec.T == "" {
		rec.T = time.Now().Format("2006-01-02 15:04:05")
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	transferMu.Lock()
	defer transferMu.Unlock()
	p := filepath.Join(logDir, "transfers.jsonl")
	if st, err := os.Stat(p); err == nil && st.Size() > transferMaxLen {
		_ = os.Remove(p + ".1")
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// tailTransfers 读最后 n 条传输记录（倒序返回，最新的在前）
func tailTransfers(n int) []transferRec {
	p := filepath.Join(logDir, "transfers.jsonl")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if n <= 0 {
		n = 50
	}
	start := len(lines) - n
	if start < 0 {
		start = 0
	}
	out := make([]transferRec, 0, len(lines)-start)
	for i := len(lines) - 1; i >= start; i-- {
		var r transferRec
		if err := json.Unmarshal([]byte(lines[i]), &r); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// ---------- UA 归类 ----------
// 手机上出的问题只能靠请求头判断，把 UA 压成短标签方便一眼看出发请求的是谁。
func uaTag(ua string) string {
	if ua == "" {
		return "-"
	}
	low := strings.ToLower(ua)
	dev := ""
	switch {
	case strings.Contains(low, "iphone"):
		dev = "iPhone"
	case strings.Contains(low, "ipad"):
		dev = "iPad"
	case strings.Contains(low, "android"):
		dev = "Android"
	case strings.Contains(low, "windows"):
		dev = "Windows"
	case strings.Contains(low, "macintosh") || strings.Contains(low, "mac os"):
		dev = "macOS"
	case strings.Contains(low, "linux"):
		dev = "Linux"
	case strings.Contains(low, "curl"):
		return "curl"
	case strings.Contains(low, "go-http"):
		return "go"
	case strings.Contains(low, "node"):
		return "node"
	case strings.Contains(low, "headlesschrome"):
		return "headless"
	}
	br := "?"
	switch {
	case strings.Contains(low, "edg/"):
		br = "Edge"
	case strings.Contains(low, "chrome/") || strings.Contains(low, "crios"):
		br = "Chrome"
	case strings.Contains(low, "firefox/") || strings.Contains(low, "fxios"):
		br = "Firefox"
	case strings.Contains(low, "safari/"):
		br = "Safari"
	}
	if dev == "" {
		return br
	}
	return dev + "/" + br
}

// ---------- 日志接口 ----------

func handleLogTail(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	lines := logH.Tail(n)
	writeJSON(w, 0, "ok", map[string]any{
		"lines": lines,
		"total": len(lines),
		"dir":   logDir,
		"file":  filepath.Join(logDir, "selfnas-"+time.Now().Format("2006-01-02")+".log"),
	})
}

func handleTransferList(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	writeJSON(w, 0, "ok", tailTransfers(n))
}

// handleClientLog 收手机端上报的关键事件。
// 前端到底有没有触发 change、选到了几个文件，服务端日志里必须能看见——
// 否则「点了勾没反应」只能靠猜。
func handleClientLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	var body struct {
		Msg string `json:"msg"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	msg := strings.TrimSpace(body.Msg)
	if msg == "" {
		writeJSON(w, 0, "ok", nil)
		return
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	ip := clientIP(r)
	log.Printf("[客户端 %s %s] %s", ip, uaTag(r.UserAgent()), msg)
	recordTransfer(transferRec{
		Kind: "client",
		IP:   ip,
		UA:   uaTag(r.UserAgent()),
		Path: msg,
		OK:   true,
	})
	writeJSON(w, 0, "ok", nil)
}

// logFiles 列出日志目录里的文件（大小 + 时间），管理台用来确认日志确实在写
func logFiles() []map[string]any {
	out := []map[string]any{}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		out = append(out, map[string]any{
			"name":  e.Name(),
			"size":  info.Size(),
			"mtime": info.ModTime().Format("2006-01-02 15:04:05"),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["name"].(string) > out[j]["name"].(string)
	})
	return out
}
