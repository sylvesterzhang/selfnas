package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	ip := clientIP(r)
	if isLocked(ip) {
		writeJSON(w, 423, "失败次数过多，请 60 秒后再试", nil)
		return
	}
	var body struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	if body.User != cfg.User || !verifyPassword(body.Pass, cfg.PassHash) {
		markFail(ip)
		writeJSON(w, 401, "账号或密码错误", nil)
		return
	}
	clearFail(ip)
	exp := setSessionCookie(w)
	writeJSON(w, 0, "登录成功", map[string]any{
		"user": cfg.User,
		"exp":  exp,
		"days": cfg.SessionDays,
	})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "sn_token", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 0, "已退出", nil)
}

func handleState(w http.ResponseWriter, r *http.Request) {
	tsUp, tsIP := tailscaleStatus()
	type rootView struct {
		Name      string `json:"name"`
		Path      string `json:"path"`
		Readonly  bool   `json:"readonly"`
		Enabled   bool   `json:"enabled"`
		TotalGB   string `json:"totalGB"`
		FreeGB    string `json:"freeGB"`
		Available bool   `json:"available"`
	}
	var roots []rootView
	for _, rt := range cfg.Roots {
		v := rootView{Name: rt.Name, Path: rt.Path, Readonly: rt.Readonly, Enabled: rt.Enabled}
		if st, err := os.Stat(rt.Path); err == nil && st.IsDir() {
			v.Available = true
			if total, free := volumeSpace(rt.Path); total > 0 {
				v.TotalGB = humanGB(total)
				v.FreeGB = humanGB(free)
			}
		}
		roots = append(roots, v)
	}
	data := map[string]any{
		"version":     version,
		"startedAt":   startedAt.Format("2006-01-02 15:04:05"),
		"uptime":      humanDuration(time.Since(startedAt)),
		"user":        cfg.User,
		"port":        cfg.Port,
		"configPath":  cfgPath,
		"lanIP":       lanIPv4(),
		"tailscale":   map[string]any{"up": tsUp, "ip": tsIP},
		"roots":       roots,
		"sessionDays": cfg.SessionDays,
		"settings": map[string]any{
			"adminOnlyLocal":    cfg.AdminOnlyLocal,
			"uploadConcurrency": cfg.UploadConcurrency,
		},
		// 自启动状态随 state 一起带回去，管理台少发一次请求
		"autostart": autostartGet(),
		// 新版本检查结果（后台每 6 小时查一次 GitHub Releases，失败为空）
		"update": updateInfo(),
	}
	writeJSON(w, 0, "ok", data)
}

// handleAutostart 开机自启动开关。GET 查状态，POST {enable:true/false} 执行切换。
// 切换是即时生效的（重建/删除计划任务），不走「保存设置」那套批量保存。
func handleAutostart(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 0, "ok", autostartGet())
	case http.MethodPost:
		var body struct {
			Enable *bool `json:"enable"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enable == nil {
			writeJSON(w, 400, "请求格式错误（需要 enable 字段）", nil)
			return
		}
		info, err := autostartSet(*body.Enable)
		if err != nil {
			log.Printf("[自启] 切换失败 enable=%v: %v", *body.Enable, err)
			writeJSON(w, 500, "设置失败: "+err.Error(), map[string]any{"autostart": info})
			return
		}
		if *body.Enable {
			log.Printf("[自启] 已开启，方式=%s 指向=%s", info.Mode, info.Exe)
		} else {
			log.Printf("[自启] 已关闭")
		}
		writeJSON(w, 0, "ok", info)
	default:
		writeJSON(w, 405, "只支持 GET/POST", nil)
	}
}

func handleDrives(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 0, "ok", listDrives())
}

func handleBrowse(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if strings.TrimSpace(p) == "" {
		type item struct {
			Name  string `json:"name"`
			Path  string `json:"path"`
			IsDir bool   `json:"isDir"`
		}
		var items []item
		for _, d := range listDrives() {
			if !d.Ready {
				continue
			}
			label := d.Label
			if label == "" {
				label = "本地磁盘"
			}
			items = append(items, item{Name: label + " (" + d.Letter + ":)", Path: d.Path, IsDir: true})
		}
		writeJSON(w, 0, "ok", map[string]any{"path": "", "items": items})
		return
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		writeJSON(w, 400, "路径非法", nil)
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		writeJSON(w, 400, "无法读取目录: "+err.Error(), nil)
		return
	}
	type item struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		IsDir bool   `json:"isDir"`
	}
	var items []item
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "$") || strings.HasPrefix(name, ".") {
			continue
		}
		items = append(items, item{Name: name, Path: filepath.Join(abs, name), IsDir: true})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	parent := filepath.Dir(abs)
	if parent == abs {
		parent = ""
	}
	writeJSON(w, 0, "ok", map[string]any{"path": abs, "parent": parent, "items": items})
}

func handleRootAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Path     string `json:"path"`
		Readonly bool   `json:"readonly"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	p := strings.TrimSpace(body.Path)
	if p == "" {
		writeJSON(w, 400, "路径不能为空", nil)
		return
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		writeJSON(w, 400, "路径非法", nil)
		return
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		writeJSON(w, 400, "目录不存在或不可访问", nil)
		return
	}
	cfgMu.Lock()
	for _, rt := range cfg.Roots {
		if strings.EqualFold(normPath(rt.Path), abs) {
			cfgMu.Unlock()
			writeJSON(w, 409, "该目录已在列表中", nil)
			return
		}
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = filepath.Base(abs)
		if name == "" || name == "." {
			name = abs
		}
	}
	cfg.Roots = append(cfg.Roots, Root{Name: name, Path: abs, Readonly: body.Readonly, Enabled: true})
	err = saveConfigLocked()
	cfgMu.Unlock()
	if err != nil {
		writeJSON(w, 500, "保存失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已添加", nil)
}

// handleRootsRescan 重新枚举本机磁盘，把「现在存在、配置里没有」的盘追加成根目录。
//
// 为什么只增不改：配置里的条目可能被用户改过名、标过只读、禁用过 ——
// 重扫的意义是「让新出现的盘自动进来」，不是「把配置恢复成出厂」。
// 已有条目一律不碰，包括离线的（换系统导致的离线根，回来时自然恢复，不该被重扫清掉）。
//
// 触发场景：双系统共用一份 config.json 时，另一个系统多出来的盘不会自动出现；
// 插了新硬盘、挂了新分区同理。点一下省去逐个「添加目录」的麻烦。
func handleRootsRescan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	cfgMu.Lock()
	defer cfgMu.Unlock()

	exist := make(map[string]bool, len(cfg.Roots))
	for _, rt := range cfg.Roots {
		exist[strings.ToLower(normPath(rt.Path))] = true
	}
	var added []string
	for _, d := range listDrives() {
		if !d.Ready || exist[strings.ToLower(normPath(d.Path))] {
			continue
		}
		name := d.Label
		if name == "" {
			name = "磁盘 " + d.Letter
		}
		// 与首次启动保持同一口径：C 盘默认不启用，其余启用
		cfg.Roots = append(cfg.Roots, Root{Name: name, Path: d.Path, Enabled: d.Letter != "C"})
		exist[strings.ToLower(normPath(d.Path))] = true
		added = append(added, d.Letter+":")
	}
	if len(added) == 0 {
		writeJSON(w, 0, "没有发现新的磁盘", map[string]any{"added": added})
		return
	}
	if err := saveConfigLocked(); err != nil {
		writeJSON(w, 500, "保存失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "ok", map[string]any{"added": added})
}

func handleRootUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path     string `json:"path"`
		Name     string `json:"name"`
		Readonly *bool  `json:"readonly"`
		Enabled  *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	want := normPath(body.Path)
	cfgMu.Lock()
	found := false
	for i := range cfg.Roots {
		if strings.EqualFold(normPath(cfg.Roots[i].Path), want) {
			if n := strings.TrimSpace(body.Name); n != "" {
				cfg.Roots[i].Name = n
			}
			if body.Readonly != nil {
				cfg.Roots[i].Readonly = *body.Readonly
			}
			if body.Enabled != nil {
				cfg.Roots[i].Enabled = *body.Enabled
			}
			found = true
			break
		}
	}
	if !found {
		cfgMu.Unlock()
		writeJSON(w, 404, "未找到该目录", nil)
		return
	}
	err := saveConfigLocked()
	cfgMu.Unlock()
	if err != nil {
		writeJSON(w, 500, "保存失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已保存（立即生效）", nil)
}

func handleRootDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	want := normPath(body.Path)
	cfgMu.Lock()
	out := cfg.Roots[:0]
	found := false
	for _, rt := range cfg.Roots {
		if strings.EqualFold(normPath(rt.Path), want) {
			found = true
			continue
		}
		out = append(out, rt)
	}
	cfg.Roots = out
	var err error
	if found {
		err = saveConfigLocked()
	}
	cfgMu.Unlock()
	if !found {
		writeJSON(w, 404, "未找到该目录", nil)
		return
	}
	if err != nil {
		writeJSON(w, 500, "保存失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已移除（仅移除配置，不删除任何文件）", nil)
}

func handlePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	if !verifyPassword(body.Old, cfg.PassHash) {
		writeJSON(w, 403, "原密码不正确", nil)
		return
	}
	if len(body.New) < 6 {
		writeJSON(w, 400, "新密码至少 6 位", nil)
		return
	}
	cfgMu.Lock()
	cfg.PassHash = hashPassword(body.New)
	err := saveConfigLocked()
	cfgMu.Unlock()
	if err != nil {
		writeJSON(w, 500, "保存失败: "+err.Error(), nil)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "sn_token", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 0, "密码已更新，请重新登录", nil)
}

func handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	var body struct {
		AdminOnlyLocal    *bool `json:"adminOnlyLocal"`
		UploadConcurrency *int  `json:"uploadConcurrency"`
		SessionDays       *int  `json:"sessionDays"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	cfgMu.Lock()
	if body.AdminOnlyLocal != nil {
		cfg.AdminOnlyLocal = *body.AdminOnlyLocal
	}
	if body.UploadConcurrency != nil {
		n := *body.UploadConcurrency
		if n < 1 {
			n = 1
		}
		if n > 8 {
			n = 8
		}
		cfg.UploadConcurrency = n
	}
	if body.SessionDays != nil {
		// 不设上限：这是用户自己机器上的私有服务，想设十年就设十年。
		// 浏览器对单个 Cookie 有 400 天上限，靠 authedRenew 每次访问续期来兜。
		n := *body.SessionDays
		if n < 1 {
			n = 1
		}
		if n > 36500 { // 100 年，纯粹防手滑写错
			n = 36500
		}
		cfg.SessionDays = n
	}
	err := saveConfigLocked()
	cfgMu.Unlock()
	if err != nil {
		writeJSON(w, 500, "保存失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已保存（立即生效）", nil)
}

// ---------- 格式化 ----------

func humanGB(b uint64) string {
	gb := float64(b) / (1024 * 1024 * 1024)
	if gb >= 1024 {
		return strconv.FormatFloat(gb/1024, 'f', 1, 64) + " TB"
	}
	return strconv.FormatFloat(gb, 'f', 1, 64) + " GB"
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return "不到 1 分钟"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	parts := []string{}
	if days > 0 {
		parts = append(parts, itoa(days)+" 天")
	}
	if hours > 0 {
		parts = append(parts, itoa(hours)+" 小时")
	}
	if mins > 0 && days == 0 {
		parts = append(parts, itoa(mins)+" 分钟")
	}
	if len(parts) == 0 {
		return "不到 1 分钟"
	}
	return strings.Join(parts, " ")
}

// normPath 统一路径形态：去空白、正斜杠转反斜杠、转绝对路径、清理冗余分隔符。
// 用于比较根目录，避免 D:/视频 与 D:\视频 被当成两个不同目录。
func normPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = filepath.FromSlash(p)
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// driveOf 取盘符（"D:\" → "D:"）。
// 用户是靠盘符认盘的：三个根叫「资料/软件/文档」时，光看名字不知道是哪个物理盘，
// 尤其重装系统后盘符会变，所以界面上凡是出现根名的地方都要带上它。
func driveOf(p string) string {
	p = strings.TrimSpace(p)
	if len(p) >= 2 && p[1] == ':' {
		c := p[0]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		return string(c) + ":"
	}
	return ""
}

func volumeSpace(p string) (uint64, uint64) { return volumeSpaceOS(p) }
