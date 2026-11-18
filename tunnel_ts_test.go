package main

import (
	"net/netip"
	"strings"
	"testing"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
)

// 内嵌节点的状态翻译是纯函数，可以脱离真实网络验证。
// 守两件事：① 直连/中继/待授权这几档别判反；② 地址必须是 IPv4 且不带端口
// （tailnet 上固定监听 80，带上 :8080 这种端口手机就打不开了）。

func tsReset(t *testing.T) {
	t.Helper()
	tsMu.Lock()
	tsGen = 0
	tsState = "off"
	tsErrText, tsAuthURL, tsIP, tsDNS, tsMode, tsRelay = "", "", "", "", "", ""
	tsOnline, tsPeers, tsVer = false, 0, ""
	tsSrv, tsHTTP, tsLn = nil, nil, nil
	tsLogs = &tsRing{max: 60}
	tsMu.Unlock()
}

func TestTSSanitizeHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"selfnas", "selfnas"},
		{"  My NAS 2F ", "MyNAS2F"},
		{"家里", tsDefaultHost}, // 中文全被剔掉 → 回落默认，不能生成空主机名
		{"-abc-", "abc"},      // 首尾的 - 不合法
		{"", tsDefaultHost},
		{strings.Repeat("a", 60), strings.Repeat("a", 40)},
	}
	for _, c := range cases {
		if got := tsSanitizeHost(c.in); got != c.want {
			t.Fatalf("tsSanitizeHost(%q) = %q，想要 %q", c.in, got, c.want)
		}
	}
}

func TestTSStateFromBackend(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Running", "running"},
		{"NeedsLogin", "needslogin"},
		{"NoState", "needslogin"},
		{"NeedsMachineAuth", "needsauth"}, // 进了 tailnet 但管理员还没批
		{"Starting", "starting"},
		{"Stopped", "stopped"},
		{"什么鬼", "starting"},
	}
	for _, c := range cases {
		st := &ipnstate.Status{BackendState: c.in}
		if got := tsStateFromBackend(st); got != c.want {
			t.Fatalf("%s → %s，想要 %s", c.in, got, c.want)
		}
	}
}

func TestTSApplyDirect(t *testing.T) {
	tsReset(t)
	k1, k2, k3 := key.NewNode().Public(), key.NewNode().Public(), key.NewNode().Public()
	st := &ipnstate.Status{
		Version:      "1.84.3-ERR-BuildInfo",
		BackendState: "Running",
		TailscaleIPs: []netip.Addr{
			netip.MustParseAddr("fd7a:115c:a1e0::1"), // IPv6 必须被跳过
			netip.MustParseAddr("100.101.102.103"),
		},
		Self: &ipnstate.PeerStatus{
			DNSName: "selfnas.tail1a2b3.ts.net.",
			CurAddr: "192.168.50.168:41641",
			Online:  true,
		},
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			k1: {Online: true},
			k2: {Online: true},
			k3: {Online: false},
		},
	}
	tsApplyStatus(0, nil, st)

	d := tsSnapshot()
	if d["state"] != "running" || d["online"] != true {
		t.Fatalf("状态不对: %v", d)
	}
	if d["ip"] != "100.101.102.103" {
		t.Fatalf("应取 IPv4 而不是 IPv6: %v", d["ip"])
	}
	if d["mode"] != "direct" {
		t.Fatalf("CurAddr 非空应判直连，实得 %v", d["mode"])
	}
	if d["dnsName"] != "selfnas.tail1a2b3.ts.net" {
		t.Fatalf("DNSName 末尾的点没去掉: %v", d["dnsName"])
	}
	if d["url"] != "http://100.101.102.103/files" {
		t.Fatalf("tailnet 地址不该带端口: %v", d["url"])
	}
	if d["version"] != "1.84.3" {
		t.Fatalf("版本号没裁掉构建尾巴: %v", d["version"])
	}
	if d["peers"] != 2 {
		t.Fatalf("在线设备数应为 2: %v", d["peers"])
	}
}

func TestTSApplyRelay(t *testing.T) {
	tsReset(t)
	st := &ipnstate.Status{
		BackendState: "Running",
		TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.101.102.104")},
		Self:         &ipnstate.PeerStatus{DNSName: "selfnas-arm.tail1a2b3.ts.net.", Relay: "hkg", Online: true},
	}
	tsApplyStatus(0, nil, st)
	d := tsSnapshot()
	if d["mode"] != "relay" || d["relay"] != "hkg" {
		t.Fatalf("应判中继并带出节点名: %v", d["mode"])
	}
	if d["url"] != "http://100.101.102.104/files" {
		t.Fatalf("访问地址不对: %v", d["url"])
	}
}

func TestTSApplyNeedsLogin(t *testing.T) {
	tsReset(t)
	st := &ipnstate.Status{
		BackendState: "NeedsLogin",
		AuthURL:      "https://login.tailscale.com/a/abc123",
	}
	tsApplyStatus(0, nil, st)
	d := tsSnapshot()
	if d["state"] != "needslogin" {
		t.Fatalf("应停在待授权: %v", d["state"])
	}
	if d["authUrl"] != "https://login.tailscale.com/a/abc123" {
		t.Fatalf("授权链接没带出来: %v", d["authUrl"])
	}
	if _, ok := d["url"]; ok {
		t.Fatalf("还没 IP 就不该给访问地址: %v", d["url"])
	}
}

func TestTSApplyStaleGeneration(t *testing.T) {
	tsReset(t)
	tsState = "off"
	// 上一次启动的轮询协程晚到一步：代不上就必须整条丢弃，不能把状态写成 running
	tsApplyStatus(7, nil, &ipnstate.Status{
		BackendState: "Running",
		TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.1.2.3")},
	})
	if tsState != "off" || tsIP != "" {
		t.Fatalf("过期协程不该改状态: %s %s", tsState, tsIP)
	}
}

func TestTSRing(t *testing.T) {
	r := &tsRing{max: 3}
	for _, s := range []string{"a", "b", "c", "d"} {
		r.add(s)
	}
	got := strings.Join(r.tail(10), ",")
	if got != "b,c,d" {
		t.Fatalf("环形缓冲应只留最后 3 条，实得 %q", got)
	}
	if n := len(r.tail(2)); n != 2 {
		t.Fatalf("tail(2) 应返回 2 条，实得 %d", n)
	}
}
