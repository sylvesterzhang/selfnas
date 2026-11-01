package main

// systemd unit 生成 / 反解析的单元测试。
//
// 为什么专门测这个：Linux 那边我只能交叉编译，二进制在本机跑不起来，
// 而 unit 写错的表现是「服务静默起不来」（systemctl 只回一句 Invalid argument），
// 用户排查成本极高。所以格式这块必须靠测试兜住。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnitContentBasics 必需段落一个都不能少，否则 systemd 直接拒绝加载。
func TestUnitContentBasics(t *testing.T) {
	u := asUnitContent("/opt/selfnas/selfnas", "/opt/selfnas", "zhanghuan")
	for _, want := range []string{
		"[Unit]", "[Service]", "[Install]",
		"Type=simple",
		"WorkingDirectory=/opt/selfnas",
		`ExecStart="/opt/selfnas/selfnas"`,
		"Restart=on-failure",
		"User=zhanghuan",
		"AmbientCapabilities=CAP_NET_BIND_SERVICE",
		"CapabilityBoundingSet=CAP_NET_BIND_SERVICE",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit 里缺少 %q\n---\n%s", want, u)
		}
	}
}

// TestUnitContentRootBranch root 身份不能写 CapabilityBoundingSet：
// 它会把 root 的 DAC_OVERRIDE 一起剥掉，服务读不了普通用户 600 权限的
// config.json（0.19.5 真机踩过：服务反复 exit 1 + permission denied）。
func TestUnitContentRootBranch(t *testing.T) {
	for _, user := range []string{"root", ""} {
		u := asUnitContent("/opt/selfnas/selfnas", "/opt/selfnas", user)
		if strings.Contains(u, "User="+user+"\n") && user != "" {
			t.Errorf("user=%q 不应写 User= 行\n%s", user, u)
		}
		if strings.Contains(u, "CapabilityBoundingSet=") {
			t.Errorf("user=%q 不应写 CapabilityBoundingSet（会剥掉 DAC_OVERRIDE）\n%s", user, u)
		}
	}
}

// TestUnitContentEscapesSpace 路径含空格必须转义成 \x20：
// systemd 按空白切词，`WorkingDirectory=/opt/my nas` 会让整个 unit 解析失败。
// macOS 的用户目录（/Users/zhang huan、Macintosh HD）经常带空格。
func TestUnitContentEscapesSpace(t *testing.T) {
	u := asUnitContent("/Users/zhang huan/selfnas", "/Users/zhang huan", "zhang huan")
	if strings.Contains(u, "WorkingDirectory=/Users/zhang huan\n") {
		t.Fatalf("空格没有被转义，systemd 会解析失败\n%s", u)
	}
	if !strings.Contains(u, `WorkingDirectory=/Users/zhang\x20huan`) {
		t.Errorf("期望 \\x20 转义，实际：\n%s", u)
	}
	if !strings.Contains(u, `ExecStart="/Users/zhang\x20huan/selfnas"`) {
		t.Errorf("ExecStart 里的空格没转义：\n%s", u)
	}
}

// TestUnitExecStartRoundTrip 写出去再读回来，必须还原成原路径
// （管理台靠它判断「装的还是不是当前这个 exe」）。
func TestUnitExecStartRoundTrip(t *testing.T) {
	cases := []string{
		"/opt/selfnas/selfnas",
		"/Users/zhang huan/selfnas",
		`/tmp/weird"dir/selfnas`,
		`/tmp/back\slash/selfnas`,
	}
	for _, exe := range cases {
		dir := filepath.Dir(exe)
		p := filepath.Join(t.TempDir(), "selfnas.service")
		if err := os.WriteFile(p, []byte(asUnitContent(exe, dir, "testuser")), 0o644); err != nil {
			t.Fatalf("写 unit 失败: %v", err)
		}
		if got := asUnitExecStart(p); got != exe {
			t.Errorf("往返不一致：写入 %q，读回 %q", exe, got)
		}
	}
}

// TestUnitExecStartMissing 读一个不存在的文件不该 panic，回空串即可。
func TestUnitExecStartMissing(t *testing.T) {
	if got := asUnitExecStart(filepath.Join(t.TempDir(), "nope.service")); got != "" {
		t.Errorf("期望空串，得到 %q", got)
	}
}
