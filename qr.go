package main

import (
	"encoding/base64"
	"fmt"
	"net/http"

	qrcode "github.com/skip2/go-qrcode"
)

// 固定二维码：内容就是一个永久地址，扫码进来输一次后台密码即可。
//
// 早先做成「一次性 + 120 秒有效期」是过度设计了：手机上每次都要跑回电脑点刷新，
// 比直接输个密码麻烦得多。二维码贴一次就长期有效才是这个场景该有的样子。

// qrHost 二维码里用哪个地址：局域网 IP 优先 → Tailscale → 本机
func qrHost() string {
	if ip := lanIPv4(); ip != "" {
		return ip
	}
	if _, ip := tailscaleStatus(); ip != "" {
		return ip
	}
	return "127.0.0.1"
}

// hostURL 拼对外地址。80 是 http 默认端口，写出来只是噪音：
// 手机地址栏里多一个 :80，用户还得怀疑自己是不是输错了
func hostURL(host, path string) string {
	if cfg.Port == 80 {
		return fmt.Sprintf("http://%s%s", host, path)
	}
	return fmt.Sprintf("http://%s:%d%s", host, cfg.Port, path)
}

func accessURL(host string) string { return hostURL(host, "/files") }

// handleQRToken 返回固定二维码（保留原路径名，前端和书签都不用改）
func handleQRToken(w http.ResponseWriter, r *http.Request) {
	host := qrHost()
	u := accessURL(host)

	png, err := qrcode.Encode(u, qrcode.Medium, 512)
	if err != nil {
		writeJSON(w, 500, "二维码生成失败: "+err.Error(), nil)
		return
	}

	_, tsIP := tailscaleStatus()
	writeJSON(w, 0, "ok", map[string]any{
		"url":         u,
		"host":        host,
		"port":        cfg.Port,
		"png":         "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
		"lan":         lanIPv4(),
		"tailscale":   tsIP,
		"sessionDays": cfg.SessionDays,
	})
}

// handleScan 老二维码的落地地址（形如 /s/<token>）。
// 一次性 token 机制已经取消，但以前发出去/打出来的链接不能变成死链，统一跳到文件页。
func handleScan(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/files", http.StatusFound)
}
