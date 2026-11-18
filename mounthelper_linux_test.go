//go:build linux

package main

// 外接盘自动挂载助手的装配逻辑：文件能不能落盘、命令拼得对不对。
// 只验「我们生成了什么」，不验 udev 真的挂上了（那要在真机上拔插，见 24 的实测）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMountHelperWritesFiles(t *testing.T) {
	dir, err := mhWrite()
	if err != nil {
		t.Fatalf("mhWrite 失败: %v", err)
	}
	if !strings.HasSuffix(dir, filepath.Join(".config", "selfnas", "mount")) {
		t.Fatalf("目录不对: %s", dir)
	}
	for _, f := range mhFiles() {
		p := filepath.Join(dir, f.Name)
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s 没写出来: %v", f.Name, err)
		}
		if st.Mode().Perm() != f.Mode.Perm() {
			t.Fatalf("%s 权限是 %v，想要 %v", f.Name, st.Mode().Perm(), f.Mode.Perm())
		}
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", f.Name, err)
		}
		if string(got) != string(f.Data) {
			t.Fatalf("%s 内容跟内嵌的不一致", f.Name)
		}
	}

	// 脚本必须可执行，否则 systemd 起不来；unit/rule 不该可执行
	if st, _ := os.Stat(filepath.Join(dir, "selfnas-mount.sh")); st.Mode().Perm()&0o111 == 0 {
		t.Fatal("selfnas-mount.sh 没有执行位")
	}

	// 幂等：再写一次不该报错、内容不变
	if _, err := mhWrite(); err != nil {
		t.Fatalf("第二次 mhWrite 失败: %v", err)
	}
}

// 这三个文件的要害都在这几条字符串上，写错一个就整条链断掉，
// 而且断了不报错（udev 静默丢弃规则、systemd 静默丢弃依赖）。
func TestMountHelperContentGuards(t *testing.T) {
	sh := string(mhScript)
	for _, want := range []string{
		"mount -t \"$FSTYPE\"", // 真正挂载
		"uid=$OWNER_UID",       // 挂载属主要能被普通用户读写
		"exfat | vfat | ntfs",  // 只碰外接盘常见格式
		"/proc/mounts",         // 已挂就跳过，防重复挂
		"mountpoint -q",        // 挂没挂上的判据
		"unmount_dev",          // 拔盘清理
	} {
		if !strings.Contains(sh, want) {
			t.Errorf("脚本里缺少 %q", want)
		}
	}

	unit := string(mhUnit)
	for _, want := range []string{
		"BindsTo=dev-%i.device", // ⚠️ 必须是 %i，写成 %I 会被静默丢弃
		"ExecStart=/usr/local/bin/selfnas-mount.sh add %I",
		"ExecStop=/usr/local/bin/selfnas-mount.sh remove %I",
		"RemainAfterExit=yes",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit 里缺少 %q", want)
		}
	}
	if strings.Contains(unit, "BindsTo=dev-%I.device") {
		t.Error("unit 里出现了 BindsTo=dev-%I.device —— %I 在依赖指令里不展开，拔盘不会触发卸载")
	}

	rule := string(mhRule)
	for _, want := range []string{
		`ACTION=="add"`,
		`ENV{DEVTYPE}=="partition"`,
		`ENV{ID_FS_USAGE}=="filesystem"`,
		`ENV{SYSTEMD_WANTS}+="selfnas-mount@%k.service"`,
	} {
		if !strings.Contains(rule, want) {
			t.Errorf("udev 规则里缺少 %q", want)
		}
	}
	// 绝不能在 udev 里直接 mount：udevd 有 PrivateMounts=yes，挂载出不了它自己
	if strings.Contains(rule, "RUN+=") && strings.Contains(rule, "mount ") {
		t.Error("udev 规则里不该直接 mount（PrivateMounts=yes，外面看不见）")
	}
}

// 两件事彻底分开：自启命令里不该出现挂载助手，挂载命令里也不该出现自启。
// 用户选的是「要不要这台机器插盘即挂」，不是「要不要自启」。
func TestInstallCommandsStayAutostartOnly(t *testing.T) {
	install, start, off := asInstallCommands()
	if install == "" || start == "" || off == "" {
		t.Fatal("命令是空的")
	}
	for _, c := range []struct {
		name string
		cmd  string
		want []string
	}{
		{"安装", install, []string{"selfnas.service", "daemon-reload", "systemctl enable --now selfnas"}},
		{"仅启动", start, []string{"selfnas.service", "daemon-reload", "systemctl start selfnas"}},
		{"卸载", off, []string{"systemctl disable --now selfnas", "selfnas.service", "daemon-reload"}},
	} {
		for _, w := range c.want {
			if !strings.Contains(c.cmd, w) {
				t.Errorf("%s命令里缺少 %q：%s", c.name, w, c.cmd)
			}
		}
		// 挂载助手是独立的一块，不许掺进自启命令里
		for _, bad := range []string{"selfnas-mount", "udevadm", "udev/rules.d"} {
			if strings.Contains(c.cmd, bad) {
				t.Errorf("%s命令里不该出现 %q：%s", c.name, bad, c.cmd)
			}
		}
		if strings.Count(c.cmd, "'")%2 != 0 {
			t.Errorf("%s命令的引号不成对：%s", c.name, c.cmd)
		}
	}
	if strings.Contains(off, "enable --now") {
		t.Error("卸载命令里出现了 enable --now")
	}
}

func TestMountCmdStaysMountOnly(t *testing.T) {
	info := autostartGet()
	if info.Mode != "systemd" {
		t.Fatalf("mode 不对: %s", info.Mode)
	}
	if info.MountCmd == "" || info.MountOffCmd == "" {
		t.Fatal("mountCmd / mountOffCmd 是空的")
	}
	for _, c := range []struct {
		name string
		cmd  string
		want []string
	}{
		{"开启", info.MountCmd, []string{
			"selfnas-mount.sh", "selfnas-mount@.service",
			"99-selfnas-mount.rules", "udevadm control --reload-rules",
		}},
		{"关闭", info.MountOffCmd, []string{
			"rm -f", "selfnas-mount.sh", "99-selfnas-mount.rules", "udevadm control --reload-rules",
		}},
	} {
		for _, w := range c.want {
			if !strings.Contains(c.cmd, w) {
				t.Errorf("%s命令里缺少 %q：%s", c.name, w, c.cmd)
			}
		}
		// 反过来：挂载命令也不许去动自启 unit
		if strings.Contains(c.cmd, "selfnas.service") || strings.Contains(c.cmd, "enable --now selfnas") {
			t.Errorf("%s命令不该碰开机自启：%s", c.name, c.cmd)
		}
		if strings.Count(c.cmd, "'")%2 != 0 {
			t.Errorf("%s命令的引号不成对：%s", c.name, c.cmd)
		}
	}
}
