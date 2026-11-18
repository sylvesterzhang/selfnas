package main

// 「管理台只允许本机打开」这条判据的单测。
//
// 这函数看着简单，但它决定「谁能进管理台」，判错一次就是把家底晾在局域网上；
// 或者反过来把用户自己锁在门外。所以每种来源都钉一条。

import (
	"net"
	"net/http/httptest"
	"testing"
)

func TestIsLocalRequest(t *testing.T) {
	// 找一张本机真实网卡地址，用来验「用本机 IP 访问自己」也算本机
	localIP := ""
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				localIP = ipnet.IP.String()
				break
			}
		}
	}

	cases := []struct {
		name   string
		remote string
		header map[string]string
		want   bool
	}{
		{"回环 v4", "127.0.0.1:5000", nil, true},
		{"回环 v6", "[::1]:5000", nil, true},
		{"局域网别的机器", "192.168.50.50:5000", nil, false},
		{"Tailscale 对端", "100.64.1.2:5000", nil, false},
		{"公网地址", "8.8.8.8:5000", nil, false},
		// cloudflared 把公网请求转给 127.0.0.1，光看 RemoteAddr 会误判成本机
		{"隧道转发（带 CF-Connecting-IP）", "127.0.0.1:5000", map[string]string{"Cf-Connecting-Ip": "1.2.3.4"}, false},
		{"隧道转发（带 CF-Ray）", "127.0.0.1:5000", map[string]string{"Cf-Ray": "8a1b2c3d4e5f-HKG"}, false},
		{"RemoteAddr 不规整", "garbage", nil, false},
	}
	if localIP != "" {
		cases = append(cases, struct {
			name   string
			remote string
			header map[string]string
			want   bool
		}{"本机网卡 IP（" + localIP + "）", net.JoinHostPort(localIP, "5000"), nil, true})
	}

	for _, c := range cases {
		r := httptest.NewRequest("GET", "http://nas/admin", nil)
		r.RemoteAddr = c.remote
		for k, v := range c.header {
			r.Header.Set(k, v)
		}
		if got := isLocalRequest(r); got != c.want {
			t.Errorf("%s：isLocalRequest=%v，想要 %v", c.name, got, c.want)
		}
	}
}

// 局域网机器自己塞 CF 头也没用：它的 RemoteAddr 不在本机地址里，照样进不来。
func TestIsLocalRequestSpoofedHeadersFromLAN(t *testing.T) {
	r := httptest.NewRequest("GET", "http://nas/admin", nil)
	r.RemoteAddr = "192.168.50.50:5000"
	r.Header.Set("Cf-Connecting-Ip", "127.0.0.1")
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	if isLocalRequest(r) {
		t.Fatal("局域网机器伪造 CF / XFF 头就进了管理台")
	}
}
