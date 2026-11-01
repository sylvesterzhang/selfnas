package main

// systemd unit 模板与反解析。故意不带平台标签：这样在 Windows 开发机上也能直接
// 跑单元测试验证生成结果——Linux 那边只有交叉编译，跑不起来，靠测试兜住格式。
//（Windows 构建里这两个函数没人调用，Go 允许，不会报错。）

import (
	"os"
	"strings"
)

// sdEscape 转义路径里的空格等字符。
//
// systemd 的 unit 是「按空白切词」解析的：`WorkingDirectory=/opt/my nas` 会被当成
// 非法值，服务直接起不来，而且 systemctl status 只给一句很含糊的 "Invalid argument"。
// 中文路径不用转义（UTF-8 原样即可），但空格必须写成 \x20 —— 这在 macOS 用户目录
// （/Users/zhang huan）和某些 Linux 安装位置里很常见。
func sdEscape(p string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		" ", `\x20`,
		"\t", `\t`,
		`"`, `\"`,
	)
	return r.Replace(p)
}

// asUnitContent 生成 unit 文本。
//
// user 参数：
//   - 普通用户名 → 写 User=<user>，服务以该用户运行，靠 AmbientCapabilities
//     授权绑 80/445 特权端口（systemd 标准姿势）。
//   - "root" 或空 → 不写 User=（以 root 运行），也**不能**写
//     CapabilityBoundingSet —— 它会把 root 的 DAC_OVERRIDE 一起剥掉，
//     变成「被削权的 root」，连普通用户 600 权限的 config.json 都读不了
//     （0.19.5 真机验证踩过：服务反复 exit 1，journalctl 报 permission denied）。
func asUnitContent(exe, dir, user string) string {
	svc := `[Unit]
Description=selfnas — 把你自己电脑上的文件搬到手机上
Documentation=https://github.com/sylvesterzhang/selfnas
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# 工作目录必须是 exe 所在目录：config.json 默认就在它旁边。
# 不设的话 systemd 会用 "/"，程序读不到配置，会当成全新安装重新生成一套
#（新密码、新根目录），看起来就像"数据全丢了"。
WorkingDirectory=` + sdEscape(dir) + `
ExecStart="` + sdEscape(exe) + `"
Restart=on-failure
RestartSec=5s
`
	if user != "" && user != "root" {
		svc += `User=` + user + `
# 80 和 445 都是 1024 以下的特权端口。以安装用户运行 + 只授绑定端口这一个能力，
# 不把整个服务跑成 root —— 少一分权限就少一分风险。
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
`
	} else {
		svc += `# 以 root 运行：特权端口本来就能绑。不要写 CapabilityBoundingSet——
# 它会把 root 的 DAC_OVERRIDE 一起剥掉，读不了普通用户的文件。
`
	}
	return svc + `
[Install]
WantedBy=multi-user.target
`
}

// asUnitExecStart 从已安装的 unit 里抠出 ExecStart 的可执行文件路径，
// 用来对比「装的是不是当前这个 exe」。
func asUnitExecStart(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, "ExecStart="))
		var path string
		if strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) && len(v) > 1 {
			// 带引号 = 路径里可能有空格，整段就是路径，别按空白切
			path = v[1 : len(v)-1]
		} else if f := strings.Fields(v); len(f) > 0 {
			path = f[0]
		}
		// 还原 sdEscape：先解 \x20（完整序列），再解 \"，最后解 \\ —— 顺序不能反
		return strings.NewReplacer(
			`\x20`, " ",
			`\t`, "\t",
			`\"`, `"`,
			`\\`, `\`,
		).Replace(path)
	}
	return ""
}
