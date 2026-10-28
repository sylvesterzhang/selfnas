package main

// 新版本检查：**不自动查**，用户在「关于」页点「检查更新」才查一次。
//
// 为什么不自动：selfnas 常跑在离线/内网的闲机器上，定时去够 GitHub 是无意义流量，
// 还会在离线环境留下一堆失败日志。用户想升级自然会点，结果存进 cache/update.json，
// 下次启动直接读缓存展示（红点也由它驱动），不必每次都查。
//
// 检查目标：api.github.com 的 releases/latest。失败完全静默返回当前状态。

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	updateAPI    = "https://api.github.com/repos/sylvesterzhang/selfnas/releases/latest"
	updateHome   = "https://github.com/sylvesterzhang/selfnas/releases/latest"
	updateCacheF = "update.json" // 相对 cacheDir
)

var (
	updMu     sync.Mutex
	updLatest = ""      // 缓存里的最新版本号（如 "0.19.6"），空 = 从没查到过
	updURL    = ""      // 该版本的发布页
	updAt     time.Time // 上次成功检查的时间
	updFailed = false   // 上次手动检查是否失败（仅用于日志去重）
)

// loadUpdateCache 启动时读缓存。没有 / 坏了都无所谓，就当没查过。
func loadUpdateCache() {
	if cacheDir == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(cacheDir, updateCacheF))
	if err != nil {
		return
	}
	var c struct {
		Latest    string `json:"latest"`
		URL       string `json:"url"`
		CheckedAt int64  `json:"checkedAt"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Latest == "" {
		return
	}
	updMu.Lock()
	updLatest = strings.TrimPrefix(strings.TrimSpace(c.Latest), "v")
	updURL = c.URL
	if c.CheckedAt > 0 {
		updAt = time.Unix(c.CheckedAt, 0)
	}
	updMu.Unlock()
}

// checkUpdateNow 用户点了「检查更新」。同步执行（前端有按钮转圈顶着），
// 成功后写缓存文件。返回是否查到了结果。
func checkUpdateNow() bool {
	req, err := http.NewRequest("GET", updateAPI, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "selfnas/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logOnce("更新检查失败（不影响使用）: " + err.Error())
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logOnce("更新检查返回 " + strconv.Itoa(resp.StatusCode) + "，跳过")
		return false
	}
	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		logOnce("更新检查解析失败: " + err.Error())
		return false
	}
	latest := strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")
	if latest == "" {
		logOnce("更新检查返回空版本号，跳过")
		return false
	}
	url := rel.HTMLURL
	if url == "" {
		url = updateHome
	}

	now := time.Now()
	updMu.Lock()
	updLatest = latest
	updURL = url
	updAt = now
	updFailed = false
	updMu.Unlock()

	saveUpdateCache(latest, url, now)
	log.Println("[更新检查] 完成：最新版 " + latest + "，当前 " + version)
	return true
}

func saveUpdateCache(latest, url string, at time.Time) {
	if cacheDir == "" {
		return
	}
	raw, err := json.Marshal(map[string]any{
		"latest":    latest,
		"url":       url,
		"checkedAt": at.Unix(),
	})
	if err != nil {
		return
	}
	if err = os.MkdirAll(cacheDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(cacheDir, updateCacheF), raw, 0o644)
}

// hasNewer 比较语义化版本号（只比较数字段，v 前缀已去掉）。
// 段数不一致时缺的按 0 补（0.19 < 0.19.1）。解析失败一律返回 false —— 宁可不提醒也别误报。
func hasNewer(latest, current string) bool {
	a := verParts(latest)
	b := verParts(current)
	if a == nil || b == nil {
		return false
	}
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		ai, bi := 0, 0
		if i < len(a) {
			ai = a[i]
		}
		if i < len(b) {
			bi = b[i]
		}
		if ai != bi {
			return ai > bi
		}
	}
	return false
}

func verParts(s string) []int {
	ps := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
	out := make([]int, 0, len(ps))
	for _, p := range ps {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// updateInfo 给 state / 检查接口用的快照。latest 空 = 从没查到过。
func updateInfo() map[string]any {
	updMu.Lock()
	defer updMu.Unlock()
	latest := updLatest
	url := updURL
	return map[string]any{
		"current":   version,
		"latest":    latest,
		"hasUpdate": latest != "" && hasNewer(latest, version),
		"url":       url,
		"checkedAt": func() string {
			if updAt.IsZero() {
				return ""
			}
			return updAt.Format("2006-01-02 15:04")
		}(),
	}
}

// logOnce 同一类失败别刷日志：状态从成功变失败才记一条。
func logOnce(msg string) {
	updMu.Lock()
	wasFailed := updFailed
	updFailed = true
	updMu.Unlock()
	if !wasFailed {
		log.Println("[更新检查] " + msg)
	}
}
