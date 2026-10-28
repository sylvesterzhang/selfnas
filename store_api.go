package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 「最近」与「收藏」的接口。全部挂在 guardAuth 后面 —— 数据是跟账号走的，
// 用户名直接取当前登录态（现在只有一个账号 admin，但表里带 username，
// 以后加多用户不用动表结构）。

func currentUser() string { return cfg.User }

func decodeBody(r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		return false
	}
	return json.NewDecoder(r.Body).Decode(v) == nil
}

type pathBody struct {
	Root string `json:"root"`
	Path string `json:"path"`
}

// canonicalRoot 把客户端传来的根路径，对到配置里那一条的写法上。
//
// 为什么要做：表的主键是 (username, root, path)，root 只要差一个斜杠就算新的，
// 同一个文件会在「最近」里出现两条（"E:\" 一条、"E://" 一条）。
// 这种脏 root 来得比想象中容易 —— 手输地址、经过 shell、经过代理都可能改写。
// 配置里的写法是唯一权威，其余一律归一到它。
func canonicalRoot(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, r := range cfg.Roots {
		if strings.EqualFold(filepath.Clean(r.Path), filepath.Clean(s)) {
			return r.Path
		}
	}
	return s
}

func handleRecentList(w http.ResponseWriter, r *http.Request) {
	items, err := listStored(tblRecent, currentUser(), 30)
	if err != nil {
		writeJSON(w, 500, "读取失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "ok", map[string]any{"items": items})
}

func handleRecentAdd(w http.ResponseWriter, r *http.Request) {
	var b pathBody
	if !decodeBody(r, &b) {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	root := canonicalRoot(b.Root)
	rel := strings.TrimSpace(b.Path)
	if root == "" || rel == "" {
		writeJSON(w, 400, "参数不完整", nil)
		return
	}
	full, _, err := resolve(root, rel)
	if err != nil {
		writeJSON(w, 403, "无权访问该路径", nil)
		return
	}
	st, err := os.Stat(full)
	if err != nil {
		writeJSON(w, 404, "文件不存在", nil)
		return
	}
	// 「最近」只记文件：文件夹去重意义不大，用户要的是「上次看了哪个片子」
	if st.IsDir() {
		writeJSON(w, 400, "只记录文件", nil)
		return
	}
	if err = upsertStored(tblRecent, currentUser(), root, rel); err != nil {
		writeJSON(w, 500, "保存失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "ok", nil)
}

func handleRecentRemove(w http.ResponseWriter, r *http.Request) {
	var b pathBody
	if !decodeBody(r, &b) {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	if err := removeStored(tblRecent, currentUser(), canonicalRoot(b.Root), strings.TrimSpace(b.Path)); err != nil {
		writeJSON(w, 500, "删除失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已移除", nil)
}

func handleRecentClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	if err := clearStored(tblRecent, currentUser()); err != nil {
		writeJSON(w, 500, "清空失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已清空", nil)
}

func handleFavList(w http.ResponseWriter, r *http.Request) {
	items, err := listStored(tblFav, currentUser(), 500)
	if err != nil {
		writeJSON(w, 500, "读取失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "ok", map[string]any{"items": items})
}

// handleFavToggle 收藏 / 取消收藏同一个接口：调用方不用先查状态再决定调哪个，
// 少一次往返，也不会出现「以为收藏了其实没有」的中间态。
func handleFavToggle(w http.ResponseWriter, r *http.Request) {
	var b pathBody
	if !decodeBody(r, &b) {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	root := canonicalRoot(b.Root)
	rel := strings.TrimSpace(b.Path)
	if root == "" || rel == "" {
		// 根目录本身允许收藏（path 为空），但前端现在不提供这个入口；
		// 这里放行的话会出现一条"收藏了盘"的记录，跟「最近不记盘」的口径不一致
		writeJSON(w, 400, "参数不完整", nil)
		return
	}
	full, _, err := resolve(root, rel)
	if err != nil {
		writeJSON(w, 403, "无权访问该路径", nil)
		return
	}
	if _, err = os.Stat(full); err != nil {
		// 文件已经没了也必须能取消收藏 —— 否则那条记录永远删不掉，
		// 「取消收藏」这个动作不该依赖文件还在不在
		if isFavorited(currentUser(), root, rel) {
			if err = removeStored(tblFav, currentUser(), root, rel); err != nil {
				writeJSON(w, 500, "保存失败: "+err.Error(), nil)
				return
			}
			writeJSON(w, 0, "已取消收藏", map[string]any{"favorited": false})
			return
		}
		writeJSON(w, 404, "文件不存在", nil)
		return
	}

	user := currentUser()
	fav := isFavorited(user, root, rel)
	if fav {
		err = removeStored(tblFav, user, root, rel)
	} else {
		err = upsertStored(tblFav, user, root, rel)
	}
	if err != nil {
		writeJSON(w, 500, "保存失败: "+err.Error(), nil)
		return
	}
	msg := "已收藏"
	if fav {
		msg = "已取消收藏"
	}
	writeJSON(w, 0, msg, map[string]any{"favorited": !fav})
}

// ---------- 刚点开标记 ----------
//
// 每个账号记「刚刚点开的那个文件」，列表里把它标出来——关掉播放器、刷新页面、
// 换设备都还认得出。与进度同口径：进库、跟账号走。
// GET  → { root, path, name }（没有就全空）
// POST {root, path} → 覆盖写入（name 由 path 取末段）

func handleLastOpenGet(w http.ResponseWriter, r *http.Request) {
	root, path, name := getLastOpen(currentUser())
	writeJSON(w, 0, "ok", map[string]any{"root": root, "path": path, "name": name})
}

func handleLastOpenSet(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Root string `json:"root"`
		Path string `json:"path"`
	}
	if !decodeBody(r, &b) {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	root := canonicalRoot(b.Root)
	rel := strings.TrimSpace(b.Path)
	if root == "" || rel == "" {
		writeJSON(w, 400, "参数不完整", nil)
		return
	}
	name := rel
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		writeJSON(w, 400, "参数不完整", nil)
		return
	}
	if err := setLastOpen(currentUser(), root, rel, name); err != nil {
		writeJSON(w, 500, "保存失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "ok", nil)
}

// ---------- 播放进度 ----------
//
// 视频看到哪、下次接着看。与「最近 / 收藏」同口径：进库、跟账号走。
// GET  ?root=&path=          → { pos, dur }（没记录就 0）
// POST {root, path, pos, dur} → 保存；pos<=0（看完/拖回开头）即删记录

func handleProgressGet(w http.ResponseWriter, r *http.Request) {
	root := canonicalRoot(r.URL.Query().Get("root"))
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if root == "" || rel == "" {
		writeJSON(w, 400, "参数不完整", nil)
		return
	}
	pos, dur := getProgress(currentUser(), root, rel)
	writeJSON(w, 0, "ok", map[string]any{"pos": pos, "dur": dur})
}

func handleProgressSet(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Root string  `json:"root"`
		Path string  `json:"path"`
		Pos  float64 `json:"pos"`
		Dur  float64 `json:"dur"`
	}
	if !decodeBody(r, &b) {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	root := canonicalRoot(b.Root)
	rel := strings.TrimSpace(b.Path)
	if root == "" || rel == "" {
		writeJSON(w, 400, "参数不完整", nil)
		return
	}
	// 不 resolve/stat：上报来自正在播放的播放器，文件必然存在；
	// 看完后文件被删也不该报错——删记录这个动作永远应该成功
	if err := setProgress(currentUser(), root, rel, b.Pos, b.Dur); err != nil {
		writeJSON(w, 500, "保存失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "ok", nil)
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	var recent, fav, prog int
	_ = db.QueryRow(`SELECT COUNT(1) FROM recent WHERE username = ?`, currentUser()).Scan(&recent)
	_ = db.QueryRow(`SELECT COUNT(1) FROM favorite WHERE username = ?`, currentUser()).Scan(&fav)
	_ = db.QueryRow(`SELECT COUNT(1) FROM progress WHERE username = ? AND pos > 0`, currentUser()).Scan(&prog)
	writeJSON(w, 0, "ok", map[string]any{
		"recent": recent,
		"fav":    fav,
		"prog":   prog,
		"dbFile": dbFile,
		"dbSize": func() string {
			if st, err := os.Stat(dbFile); err == nil {
				return strconv.FormatInt(st.Size()/1024+1, 10) + " KB"
			}
			return "-"
		}(),
	})
}
