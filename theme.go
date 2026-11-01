package main

// 文件站皮肤。皮肤 = 一组 CSS 变量预设（见 style.css 的 html[data-theme=...] 块），
// 管理台「系统设置 → 文件站外观」选择，files 页启动时挂到 <html data-theme>。
//
// 曾经有过「跟随系统」（空值）选项，2026-11-01 用户拍板去掉：服务端就一套界面，
// 「跟随」跟随不到任何东西，没有意义；旧配置里的空值在 normalizeConfig 里迁移为 light。
//
// 边界（讨论时定下的）：皮肤只动色调/字体/行距，不动布局；
// 只作用于 files 页，管理台保持自己的简洁工具脸。

var themeKeys = []string{"light", "blue", "pink", "ebook", "green", "dark"}

func themeAllowed(t string) bool {
	for _, k := range themeKeys {
		if k == t {
			return true
		}
	}
	return false
}
