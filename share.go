package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 「让 App 去播」——这是 Infuse / VLC / nPlayer 能做到而浏览器做不到的根本原因：
// 它们自带 ffmpeg 解封装 + 自带音频解码器 + 系统级硬解，MKV/AC3/DTS/PGS 字幕全都能吃。
// 网页没法给浏览器装解码器，但可以**把文件 URL 交给这些 App**。
//
// 难点是认证：外部播放器不带我们的 cookie。所以发一条**短期签名直链**，
// 签名里绑住 路径 + 大小 + 过期时间，密钥是 config 里的 secret。
// 链接本身即凭证，谁拿到都能读到那个文件，所以有效期故意给得很短。

const linkTTL = 6 * time.Hour

func signPath(full string, size int64, exp int64) string {
	m := hmac.New(sha1.New, []byte(cfg.Secret))
	m.Write([]byte(full + "|" + strconv.FormatInt(size, 10) + "|" + strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(m.Sum(nil))
}

// handleFSLink 生成一条给外部播放器用的直链（绝对地址，因为 App 只知道这个 URL）。
func handleFSLink(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	full, _, err := resolveFor(webUserOf(r), q.Get("root"), q.Get("path"))
	if err != nil {
		writeJSON(w, 403, "无权访问该路径", nil)
		return
	}
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		writeJSON(w, 404, "文件不存在", nil)
		return
	}
	exp := time.Now().Add(linkTTL).Unix()
	sig := signPath(full, st.Size(), exp)
	// 用请求里看到的 host 拼绝对地址：手机访问 192.168.x.x 时，App 也得能连到同一个地址
	host := r.Host
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	u := scheme + "://" + host + "/f?p=" + hex.EncodeToString([]byte(full)) +
		"&s=" + strconv.FormatInt(st.Size(), 10) + "&e=" + strconv.FormatInt(exp, 10) + "&g=" + sig
	writeJSON(w, 0, "", map[string]any{
		"url":    u,
		"name":   filepath.Base(full),
		"size":   st.Size(),
		"expire": exp,
		"ttl":    int(linkTTL.Seconds()),
	})
}

// handleSignedFile 校验签名后直接把文件发出去。支持 Range —— 播放器拖进度全靠它。
func handleSignedFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	phex := q.Get("p")
	raw, err := hex.DecodeString(phex)
	if err != nil || len(raw) == 0 {
		log.Printf("[直链] 拒绝 %s: 链接无效（p 解不开）", clientIP(r))
		http.Error(w, "链接无效", http.StatusForbidden)
		return
	}
	full := string(raw)
	size, _ := strconv.ParseInt(q.Get("s"), 10, 64)
	exp, _ := strconv.ParseInt(q.Get("e"), 10, 64)
	if exp == 0 || time.Now().Unix() > exp {
		log.Printf("[直链] 拒绝 %s %s: 链接已过期（e=%d）", clientIP(r), filepath.Base(full), exp)
		http.Error(w, "链接已过期", http.StatusForbidden)
		return
	}
	if !hmac.Equal([]byte(q.Get("g")), []byte(signPath(full, size, exp))) {
		log.Printf("[直链] 拒绝 %s %s: 签名不对", clientIP(r), filepath.Base(full))
		http.Error(w, "签名不对", http.StatusForbidden)
		return
	}
	// 签名只对「那一个路径 + 那一个大小」有效；文件被换掉就自动失效
	st, err := os.Stat(full)
	if err != nil || st.IsDir() || st.Size() != size {
		log.Printf("[直链] 拒绝 %s %s: 文件不在了或大小变了（want=%d）", clientIP(r), filepath.Base(full), size)
		http.Error(w, "文件不在了", http.StatusNotFound)
		return
	}
	// iOS 锁屏的 MediaSession artwork：系统媒体服务会自己去抓这个 URL，但它不带
	// 网页 cookie，/api/fs/cover 会被 401 挡掉，锁屏就只剩站点图标。
	// 复用这条已验签的直链，加 cover=1 把「音频流」换成「封面图」即可。
	// 签名绑的是同一个绝对路径 + 文件大小，换文件 / 过期照样失效。
	// 额外可带 w=<边长>（artwork 多尺寸，白名单见 coverSizes），只影响出图，
	// 不参与签名；非法值会被服务端收敛成默认 640。
	// placeholder=true：歌没内嵌封面时给一张通用专辑图，别让锁屏退回站点图标
	if q.Get("cover") == "1" {
		serveCoverFull(w, r, full, true)
		return
	}
	log_LinkUse(r, full)
	// inline：Safari 打开时优先就地播放；播放器会直接读流，不看这个头
	w.Header().Set("Content-Disposition", "inline; filename*=UTF-8''"+urlEscape(filepath.Base(full)))
	http.ServeFile(w, r, full)
}

func log_LinkUse(r *http.Request, full string) {
	log.Printf("[直链] %s %s 取用 %s range=%q", clientIP(r), uaTag(r.UserAgent()),
		filepath.Base(full), r.Header.Get("Range"))
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}
