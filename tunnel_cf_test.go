package main

// cloudflared 隧道的纯逻辑单测：地址解析、错误归类、日志清洗、状态快照。
// 真起进程那部分只能端到端验（要联网到 Cloudflare），不在这里。

import (
	"strings"
	"testing"
)

func TestCFParseURL(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		// quick tunnel 把地址打在 banner 框里，两边有竖线和空格
		{"|  https://decades-thumbnails-conservative-interstate.trycloudflare.com  |",
			"https://decades-thumbnails-conservative-interstate.trycloudflare.com"},
		{"2026-11-15T13:44:03Z INF Your quick Tunnel has been created! Visit it at https://for-tutorial-etc-any.trycloudflare.com",
			"https://for-tutorial-etc-any.trycloudflare.com"},
		// 不是 quick 地址的一律不认
		{"INF Registered tunnel connection connIndex=0 location=hkg09", ""},
		{"https://api.cloudflare.com/client/v4/accounts", ""},
		{"https://nas.example.com/files", ""},
	}
	for _, c := range cases {
		got := cfURLRe.FindString(c.line)
		if got != c.want {
			t.Fatalf("解析 %q\n  得到 %q\n  期望 %q", c.line, got, c.want)
		}
	}
}

func TestCFCleanANSI(t *testing.T) {
	// 部分 Windows 终端下 cloudflared 会带颜色码，不清掉会混进错误提示
	in := "\x1b[36mINF\x1b[0m Your quick Tunnel: \x1b[32mhttps://a-b.trycloudflare.com\x1b[0m"
	got := cfClean(in)
	if strings.Contains(got, "\x1b") {
		t.Fatalf("ANSI 没清干净: %q", got)
	}
	if !strings.HasPrefix(got, "INF Your quick Tunnel") {
		t.Fatalf("清洗后内容不对: %q", got)
	}
	if u := cfURLRe.FindString(got); u != "https://a-b.trycloudflare.com" {
		t.Fatalf("清洗后应还能抓到地址，得到 %q", u)
	}
}

func TestCFExplain(t *testing.T) {
	cases := []struct {
		detail string
		logs   []string
		want   string // 期望命中的关键词
	}{
		{"failed to request quick Tunnel: Post \"https://api.trycloudflare.com\": dial tcp: i/o timeout", nil, "连不上 Cloudflare"},
		{"", []string{"ERR failed to request quick Tunnel: no such host"}, "连不上 Cloudflare"},
		{"exit status 1", []string{"ERR Invalid tunnel secret"}, "Token 不对"},
		{"exit status 1", []string{"ERR tunnel not found"}, "隧道在 Cloudflare 那边不存在"},
		{"exit status 1", []string{"ERR Unable to reach the origin service"}, "回不到本机服务"},
	}
	for _, c := range cases {
		cfLogs.lines = nil
		for _, l := range c.logs {
			cfLogs.add(l)
		}
		got := cfExplain(c.detail)
		if !strings.Contains(got, c.want) {
			t.Fatalf("detail=%q logs=%v\n  得到 %q\n  期望包含 %q", c.detail, c.logs, got, c.want)
		}
	}
	// 翻不出来的要退回原始信息，不能吞掉
	cfLogs.lines = nil
	got := cfExplain("exit status 1")
	if !strings.Contains(got, "异常退出") {
		t.Fatalf("无法归类时也该说清是异常退出，得到 %q", got)
	}
}

func TestCFSnapshotStates(t *testing.T) {
	saveCfg := cfg
	defer func() { cfg = saveCfg }()
	cfg.CfHost = ""
	cfg.CfLastURL = ""

	// 未开启：不该带 url / qr，但要有 bundled 和 target
	cfMu.Lock()
	cfState, cfURL, cfMode, cfErr, cfSrc = "off", "", "", "", ""
	cfMu.Unlock()
	d := cfSnapshot()
	if d["state"] != "off" {
		t.Fatalf("state 应为 off，得到 %v", d["state"])
	}
	if _, has := d["url"]; has {
		t.Fatal("未开启时不该有 url")
	}
	if _, has := d["qr"]; has {
		t.Fatal("未开启时不该有二维码")
	}

	// quick 运行中：要有地址 + 二维码
	cfMu.Lock()
	cfState, cfURL, cfMode, cfSrc = "running", "https://x-y.trycloudflare.com", cfModeQuick, "builtin"
	cfMu.Unlock()
	d = cfSnapshot()
	if d["url"] != "https://x-y.trycloudflare.com" {
		t.Fatalf("url 不对: %v", d["url"])
	}
	if q, _ := d["qr"].(string); !strings.HasPrefix(q, "data:image/png;base64,") {
		t.Fatal("quick 模式应生成二维码")
	}

	// named 运行中：地址固定，不给二维码（没必要让人反复扫）
	cfMu.Lock()
	cfState, cfURL, cfMode = "running", "https://nas.example.com", cfModeNamed
	cfMu.Unlock()
	cfg.CfHost = "nas.example.com"
	d = cfSnapshot()
	if _, has := d["qr"]; has {
		t.Fatal("named 模式不该生成二维码")
	}

	// 地址变了：prevUrl 与当前不同才置 changed
	cfMu.Lock()
	cfState, cfURL, cfMode, cfPrev = "running", "https://new.trycloudflare.com", cfModeQuick,
		"https://old.trycloudflare.com"
	cfMu.Unlock()
	d = cfSnapshot()
	if d["prevUrl"] != "https://old.trycloudflare.com" {
		t.Fatalf("prevUrl 不对: %v", d["prevUrl"])
	}
	if d["changed"] != true {
		t.Fatal("地址不同时应标记 changed")
	}

	// 地址没变：不能误报
	cfMu.Lock()
	cfPrev = "https://new.trycloudflare.com"
	cfMu.Unlock()
	d = cfSnapshot()
	if _, has := d["changed"]; has {
		t.Fatal("地址相同时不该报 changed")
	}

	// 复位，别影响后面别的用例
	cfMu.Lock()
	cfState, cfURL, cfMode, cfErr, cfPrev = "off", "", "", "", ""
	cfMu.Unlock()
}

func TestCFTargetUsesEffPort(t *testing.T) {
	savePort := effPort
	defer func() { effPort = savePort }()
	// 用 -p 起的测试实例必须指到自己那个端口，不是配置里的 80
	effPort = 8899
	if got := cfTarget(); got != "http://127.0.0.1:8899" {
		t.Fatalf("转发目标应跟随实际端口，得到 %q", got)
	}
}
