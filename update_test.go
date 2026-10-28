package main

import "testing"

// 版本比较是“提醒”的唯一依据，错了要么永远不提醒、要么天天误报，必须锁死。
func TestHasNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"0.19.6", "0.19.5", true},
		{"v0.19.6", "0.19.5", true},   // tag 带 v 前缀
		{"0.20.0", "0.19.9", true},
		{"1.0.0", "0.99.99", true},
		{"0.19.5", "0.19.5", false},   // 相同
		{"0.19.4", "0.19.5", false},   // 更旧
		{"0.19", "0.19.5", false},     // 段数不齐，缺的补 0：0.19.0 < 0.19.5
		{"0.19.6", "0.19", true},      // 0.19.0 < 0.19.6
		{"0.19.6-beta", "0.19.5", false}, // 带 prerelease 后缀解析失败 → 宁可不提醒
		{"", "0.19.5", false},         // 空（还没查到）
		{"abc", "0.19.5", false},      // 乱数据
	}
	for _, c := range cases {
		if got := hasNewer(c.latest, c.current); got != c.want {
			t.Errorf("hasNewer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}
