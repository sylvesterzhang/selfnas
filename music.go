package main

// 音乐库：管理台把若干文件夹标记进库 → 扫描进 music_track 表 → 歌手/专辑聚合查询。
//
// 设计要点（详见 E:\desktop\SelfNAS音乐播放器需求文档.md §2）：
//   · **不自动全盘扫**：只有管理台标记的目录进库，下载目录里散落的几个音频不该被扫成"专辑"。
//   · **不驻留 inotify**：个人库变更频率极低。进入音乐页时做一次轻量盘点（只遍历文件名+
//     mtime+size，不读标签），和库表有差异就后台增量扫；管理台另有手动「重新扫描」。
//   · **一张表**：歌手/专辑在查询时按 album_artist + album 聚合，个人库几千首的量级
//     聚合毫无压力，不为它们单独建表——少两张表就少两份同步逻辑。
//   · 受限账号能看到的曲目 = 他的白名单文件夹 ∩ 音乐库（与文件站权限同源）。
//   · 时长用 ffprobe 补（本机装了 ffmpeg 才有），没装存 0，前端显示 --。

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 库范围 ----------

// musicLibDirs 管理台标记的库目录（规范化为绝对路径）。
func musicLibDirs() []string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	out := make([]string, 0, len(cfg.MusicLibrary))
	for _, p := range cfg.MusicLibrary {
		if abs := normPath(p); abs != "" {
			out = append(out, abs)
		}
	}
	return out
}

// musicTrackRoot 把一个绝对路径换算成「根 + 相对路径」——库表和 recent/favorite 的
// 主键口径一致，播放时直接喂 /api/fs/download。不在任何启用根下的文件没法播，跳过。
func musicTrackRoot(full string) (root, rel string, ok bool) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	for _, rt := range cfg.Roots {
		if !rt.Enabled {
			continue
		}
		base := normPath(rt.Path)
		if base == "" {
			continue
		}
		rel, err := filepath.Rel(base, full)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue
		}
		return rt.Path, filepath.ToSlash(rel), true
	}
	return "", "", false
}

// musicScopedOK 受限账号的白名单过滤：曲目绝对路径落在他的任一白名单文件夹下才可见。
func musicScopedOK(user, full string) bool {
	folders, scoped := scopedFoldersFor(user)
	if !scoped {
		return true // admin / 非受限账号看全库
	}
	for _, f := range folders {
		if hasPathPrefix(full, f.Path) {
			return true
		}
	}
	return false
}

func hasPathPrefix(full, prefix string) bool {
	f, p := normPath(full), normPath(prefix)
	if p == "" {
		return false
	}
	rel, err := filepath.Rel(p, f)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// ---------- 数据模型 ----------

type musicTrack struct {
	Root        string  `json:"root"`
	Path        string  `json:"path"`
	Mtime       int64   `json:"-"`
	Size        int64   `json:"-"`
	Title       string  `json:"title"`
	Artist      string  `json:"artist"`
	AlbumArtist string  `json:"albumArtist"`
	Album       string  `json:"album"`
	TrackNo     int     `json:"trackNo"`
	DiscNo      int     `json:"-"`
	Year        int     `json:"year"`
	HasCover    bool    `json:"hasCover"`
	Duration    float64 `json:"duration"`
	// 规格标记：hires / lossless / 空（见 musicfmt.go）。只给列表看，别的地方不带。
	Q string `json:"q,omitempty"`
}

func (t *musicTrack) full() string { return filepath.Join(normPath(t.Root), filepath.FromSlash(t.Path)) }

// 显示口径：合辑里 albumArtist 缺失时退回 artist，都空归「未知歌手」；
// 专辑同理。聚合、分组、搜索全走这两个口径。
func (t *musicTrack) showArtist() string {
	if t.AlbumArtist != "" {
		return t.AlbumArtist
	}
	if t.Artist != "" {
		return t.Artist
	}
	return "未知歌手"
}
func (t *musicTrack) showAlbum() string {
	if t.Album != "" {
		return t.Album
	}
	return "未知专辑"
}

const musicUnknownArtist = "未知歌手"

func musicLoadAll() ([]*musicTrack, error) {
	rows, err := db.Query(`SELECT root, path, mtime, size, title, artist, album_artist,
		album, track_no, disc_no, year, has_cover, duration FROM music_track`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*musicTrack
	for rows.Next() {
		t := &musicTrack{}
		var hc int
		if err := rows.Scan(&t.Root, &t.Path, &t.Mtime, &t.Size, &t.Title, &t.Artist,
			&t.AlbumArtist, &t.Album, &t.TrackNo, &t.DiscNo, &t.Year, &hc, &t.Duration); err != nil {
			continue
		}
		t.HasCover = hc != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// musicVisibleTracks 该账号能看到的全部曲目（受限账号 = 白名单 ∩ 库）。
func musicVisibleTracks(user string) []*musicTrack {
	all, err := musicLoadAll()
	if err != nil {
		log.Printf("[音乐] 读库失败: %v", err)
		return nil
	}
	out := make([]*musicTrack, 0, len(all))
	for _, t := range all {
		if musicScopedOK(user, t.full()) {
			out = append(out, t)
		}
	}
	return out
}

// ---------- 扫描 ----------

type musicScanState struct {
	mu       sync.Mutex
	running  bool
	done     int
	total    int
	lastScan int64
}

var musicScan musicScanState

// musicInventory 轻量盘点：只遍历文件名 + mtime + size，一个标签都不读。
// 几千个文件也就毫秒级，进音乐页时跑一次完全无感。
func musicInventory(dirs []string) map[string]struct{ m, s int64 } {
	inv := make(map[string]struct{ m, s int64 })
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			full := filepath.Join(dir, e.Name())
			if e.IsDir() {
				walk(full)
				continue
			}
		if fileKind(e.Name(), false) != "audio" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if _, _, ok := musicTrackRoot(full); ok {
			inv[normPath(full)] = struct{ m, s int64 }{info.ModTime().Unix(), info.Size()}
		}
		}
	}
	for _, d := range dirs {
		walk(d)
	}
	return inv
}

// musicCheckDirty 库表和实际文件有没有对不上。有差异返回 true，调用方决定要不要扫。
func musicCheckDirty() (bool, int) {
	inv := musicInventory(musicLibDirs())
	rows, err := db.Query(`SELECT root, path, mtime, size FROM music_track`)
	if err != nil {
		return true, 0
	}
	defer rows.Close()
	seen := make(map[string]bool, len(inv))
	for rows.Next() {
		var root, path string
		var m, s int64
		if err := rows.Scan(&root, &path, &m, &s); err != nil {
			continue
		}
		key := normPath(filepath.Join(normPath(root), filepath.FromSlash(path)))
		seen[key] = true
		v, ok := inv[key]
		if !ok || v.m != m || v.s != s {
			return true, len(inv)
		}
	}
	return len(inv) != len(seen), len(inv)
}

// musicStartScan 幂等的后台扫描。已经在跑就返回 false。
func musicStartScan() bool {
	musicScan.mu.Lock()
	if musicScan.running {
		musicScan.mu.Unlock()
		return false
	}
	musicScan.running = true
	musicScan.done = 0
	musicScan.total = 0
	musicScan.mu.Unlock()
	go musicScanRun()
	return true
}

func musicScanRun() {
	defer func() {
		musicScan.mu.Lock()
		musicScan.running = false
		musicScan.lastScan = time.Now().Unix()
		musicScan.mu.Unlock()
	}()

	dirs := musicLibDirs()
	inv := musicInventory(dirs)

	// 表里现有条目 → key → (mtime, size)，用来判增量
	type old struct{ m, s int64 }
	existing := map[string]old{}
	noDur := map[string]bool{} // 已入库但时长为 0：可能是当时没装 ffprobe，这次顺手补
	rows, err := db.Query(`SELECT root, path, mtime, size, duration FROM music_track`)
	if err == nil {
		for rows.Next() {
			var root, path string
			var m, s int64
			var dur float64
			if rows.Scan(&root, &path, &m, &s, &dur) == nil {
				key := normPath(filepath.Join(normPath(root), filepath.FromSlash(path)))
				existing[key] = old{m, s}
				if dur <= 0 {
					noDur[key] = true
				}
			}
		}
		rows.Close()
	}

	_, ffprobe := findFF()

	musicScan.mu.Lock()
	musicScan.total = len(inv)
	musicScan.mu.Unlock()

	var dead []string // 表里有、盘上没了/换掉了 → 删
	scanned := 0
	for key, v := range inv {
		if o, ok := existing[key]; ok && o.m == v.m && o.s == v.s {
			// 没变过的直接跳过：增量扫描的成本全在这一行。
			// 唯独时长还空着（上次扫描时没装 ffprobe）的，这次补一针。
			if ffprobe != "" && noDur[key] {
				full := key
				if root, rel, ok := musicTrackRoot(full); ok {
					if d := musicDuration(ffprobe, full); d > 0 {
						if _, err := db.Exec(`UPDATE music_track SET duration = ? WHERE root = ? AND path = ?`, d, root, rel); err == nil {
							flush()
						}
					}
				}
			}
			continue
		}
		full := key
		root, rel, ok := musicTrackRoot(full)
		if !ok {
			continue
		}
		t := &musicTrack{Root: root, Path: rel, Mtime: v.m, Size: v.s}
		tag := readTrackTag(full, filepath.Base(full), v.m, v.s)
		t.Title, t.Artist, t.Album = tag.Title, tag.Artist, tag.Album
		t.AlbumArtist, t.TrackNo, t.DiscNo, t.Year, t.HasCover =
			tag.AlbumArtist, tag.Track, tag.Disc, tag.Year, tag.HasCover
		if ffprobe != "" {
			t.Duration = musicDuration(ffprobe, full)
		}
		if err := musicUpsert(t); err != nil {
			log.Printf("[音乐] 写库失败 %s: %v", rel, err)
		}
		scanned++
		if scanned%32 == 0 {
			musicScan.mu.Lock()
			musicScan.done = scanned
			musicScan.mu.Unlock()
			runtime.Gosched()
			time.Sleep(15 * time.Millisecond) // 别把磁盘/进程吃满，列表请求还指着它
		}
	}
	for key := range existing {
		if _, ok := inv[key]; !ok {
			dead = append(dead, key)
		}
	}
	for _, key := range dead {
		if root, rel, ok := musicTrackRoot(key); ok {
			_, _ = db.Exec(`DELETE FROM music_track WHERE root = ? AND path = ?`, root, rel)
		}
	}
	log.Printf("[音乐] 扫描完成：共 %d 首，更新 %d，清理 %d", len(inv), scanned, len(dead))
}

// musicDuration ffprobe 拿秒数。拿不到就 0，前端显示 --，不值得为它报错。
// 参数不带 -nostdin：goexec 下 stdin 本来就不是终端，且部分构建（如 Jellyfin 版）不认这个旗标。
func musicDuration(ffprobe, abs string) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries",
		"format=duration", "-of", "csv=p=0", abs).Output()
	if err != nil {
		return 0
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if d < 0 || d > 86400 {
		return 0
	}
	return d
}

func musicUpsert(t *musicTrack) error {
	_, err := db.Exec(`INSERT INTO music_track
		(root, path, mtime, size, title, artist, album_artist, album, track_no, disc_no, year, has_cover, duration)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(root, path) DO UPDATE SET mtime=excluded.mtime, size=excluded.size,
		title=excluded.title, artist=excluded.artist, album_artist=excluded.album_artist,
		album=excluded.album, track_no=excluded.track_no, disc_no=excluded.disc_no,
		year=excluded.year, has_cover=excluded.has_cover, duration=excluded.duration`,
		t.Root, t.Path, t.Mtime, t.Size, t.Title, t.Artist, t.AlbumArtist, t.Album,
		t.TrackNo, t.DiscNo, t.Year, boolInt(t.HasCover), t.Duration)
	if err == nil {
		flush()
	}
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- 查询聚合（全部在 Go 里做：几千首排序分组毫秒级，不写聚合 SQL） ----------

type musicArtist struct {
	Name   string `json:"name"`
	Albums int    `json:"albums"`
	Tracks int    `json:"tracks"`
}

type musicAlbum struct {
	Artist   string  `json:"artist"`
	Album    string  `json:"album"`
	Year     int     `json:"year"`
	Tracks   int     `json:"tracks"`
	Duration float64 `json:"duration"`
	HasCover bool    `json:"hasCover"`
	// 第一首的 root+path：专辑封面去哪取
	Root string `json:"root"`
	Path string `json:"path"`
}

func musicMatch(t *musicTrack, q string) bool {
	if q == "" {
		return true
	}
	q = strings.ToLower(q)
	return strings.Contains(strings.ToLower(t.Title), q) ||
		strings.Contains(strings.ToLower(t.Artist), q) ||
		strings.Contains(strings.ToLower(t.AlbumArtist), q) ||
		strings.Contains(strings.ToLower(t.Album), q)
}

// ---------- Handlers ----------

// GET/POST/DELETE /api/music/lib —— 管理台管理库目录（admin only，guard 在路由上）
// handleMusicBrowse /api/music/browse?path=
//
// 曲库挑目录专用。跟 /api/browse（管理台给整机挑目录、只认 admin、不做范围过滤）的区别
// 只有一条，但很关键：**只在当前账号看得见的根里走**。
//   - 管理员：只看得到配置里启用的根（没启用的 C: 不会出现，也就选不进曲库）
//   - 受限账号：只看得到自己的白名单文件夹（将来开放给他们管理时天然合规）
// 根以外的路径一律 403，不靠前端自觉。
func handleMusicBrowse(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		IsDir bool   `json:"isDir"`
	}
	rootsList := visibleRoots(webUserOf(r))
	p := strings.TrimSpace(r.URL.Query().Get("path"))

	if p == "" {
		var items []item
		for _, rt := range rootsList {
			name := rt.Name
			// 盘根带上盘符（"资料 (D:)"），否则几个根名字一样时没法分
			if d := driveOf(rt.Path); d != "" && len(normPath(rt.Path)) <= 3 {
				name += " (" + d + ")"
			}
			items = append(items, item{Name: name, Path: rt.Path, IsDir: true})
		}
		writeJSON(w, 0, "ok", map[string]any{"path": "", "items": items})
		return
	}

	abs := normPath(p)
	if abs == "" {
		writeJSON(w, 400, "路径非法", nil)
		return
	}
	// 落在哪个根里？取最长匹配 —— 白名单里可能有父子目录同时被授权
	var hit Root
	best := -1
	for _, rt := range rootsList {
		if pathWithin(rt.Path, abs) && len(normPath(rt.Path)) > best {
			hit, best = rt, len(normPath(rt.Path))
		}
	}
	if best < 0 {
		writeJSON(w, 403, "这个文件夹不在你的权限范围里", nil)
		return
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		writeJSON(w, 400, "无法读取目录: "+err.Error(), nil)
		return
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

	parent := ""
	if !strings.EqualFold(filepath.Clean(abs), filepath.Clean(normPath(hit.Path))) {
		parent = filepath.Dir(abs)
	}
	writeJSON(w, 0, "ok", map[string]any{"path": abs, "parent": parent, "items": items})
}

func handleMusicLib(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 0, "ok", map[string]any{"dirs": musicLibDirs()})

	case http.MethodPost:
		var b struct {
			Path string `json:"path"`
		}
		if !decodeBody(r, &b) {
			writeJSON(w, 400, "请求格式错误", nil)
			return
		}
		abs := normPath(b.Path)
		if abs == "" {
			writeJSON(w, 400, "路径不能为空", nil)
			return
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			writeJSON(w, 400, "目录不存在或不可访问", nil)
			return
		}
		// 范围校验在服务端做，不能只靠前端只给可见选项 —— 接口能被直接调。
		// 口径同 handleMusicBrowse：得落在某个看得见的根里。
		inRange := false
		for _, rt := range visibleRoots(webUserOf(r)) {
			if pathWithin(rt.Path, abs) {
				inRange = true
				break
			}
		}
		if !inRange {
			writeJSON(w, 403, "这个文件夹不在你的权限范围里", nil)
			return
		}
		cfgMu.Lock()
		for _, p := range cfg.MusicLibrary {
			if strings.EqualFold(normPath(p), abs) {
				cfgMu.Unlock()
				writeJSON(w, 409, "该目录已在音乐库里", nil)
				return
			}
		}
		cfg.MusicLibrary = append(cfg.MusicLibrary, abs)
		err := saveConfigLocked()
		cfgMu.Unlock()
		if err != nil {
			writeJSON(w, 500, "保存失败: "+err.Error(), nil)
			return
		}
		musicStartScan() // 新目录进库，后台立刻扫
		writeJSON(w, 0, "已加入音乐库", nil)

	case http.MethodDelete:
		p := normPath(r.URL.Query().Get("path"))
		cfgMu.Lock()
		out := cfg.MusicLibrary[:0]
		for _, x := range cfg.MusicLibrary {
			if !strings.EqualFold(normPath(x), p) {
				out = append(out, x)
			}
		}
		cfg.MusicLibrary = out
		err := saveConfigLocked()
		cfgMu.Unlock()
		if err != nil {
			writeJSON(w, 500, "保存失败: "+err.Error(), nil)
			return
		}
		musicStartScan() // 范围变了，重算一遍顺便清掉出库的条目（status 轮询会自动补漏）
		writeJSON(w, 0, "已移出音乐库", nil)

	default:
		writeJSON(w, 405, "只支持 GET/POST/DELETE", nil)
	}
}

// GET /api/music/status —— 音乐页打开时先调它；发现库脏了顺手把扫描带起来。
func handleMusicStatus(w http.ResponseWriter, r *http.Request) {
	musicScan.mu.Lock()
	st := map[string]any{
		"scanning": musicScan.running,
		"done":     musicScan.done,
		"total":    musicScan.total,
		"lastScan": musicScan.lastScan,
	}
	musicScan.mu.Unlock()
	var count int
	_ = db.QueryRow(`SELECT COUNT(*) FROM music_track`).Scan(&count)
	st["count"] = count
	if !st["scanning"].(bool) {
		dirty, files := musicCheckDirty()
		st["dirty"] = dirty
		st["files"] = files
		if dirty && len(musicLibDirs()) > 0 {
			musicStartScan()
			st["scanning"] = true
		}
	} else {
		st["dirty"] = false
		st["files"] = count
	}
	writeJSON(w, 0, "", st)
}

// POST /api/music/scan —— 管理台手动「重新扫描」
func handleMusicScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	if musicStartScan() {
		writeJSON(w, 0, "开始扫描", nil)
		return
	}
	writeJSON(w, 0, "已经在扫了", nil)
}

// GET /api/music/artists?q=
func handleMusicArtists(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	tracks := musicVisibleTracks(currentUserOf(r))
	byArtist := map[string]*musicArtist{}
	for _, t := range tracks {
		if !musicMatch(t, q) {
			continue
		}
		a := byArtist[t.showArtist()]
		if a == nil {
			a = &musicArtist{Name: t.showArtist()}
			byArtist[t.showArtist()] = a
		}
		a.Tracks++
	}
	// 专辑数要按 (artist, album) 二元组数：同一歌手的同一张专辑只算一张
	seen := map[string]bool{}
	for _, t := range tracks {
		if !musicMatch(t, q) {
			continue
		}
		key := t.showArtist() + "\x00" + t.showAlbum()
		if !seen[key] {
			seen[key] = true
			if a := byArtist[t.showArtist()]; a != nil {
				a.Albums++
			}
		}
	}
	out := make([]musicArtist, 0, len(byArtist))
	for _, a := range byArtist {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, 0, "", out)
}

// GET /api/music/albums?artist=&q=
func handleMusicAlbums(w http.ResponseWriter, r *http.Request) {
	artist := strings.TrimSpace(r.URL.Query().Get("artist"))
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	tracks := musicVisibleTracks(currentUserOf(r))
	byAlbum := map[string]*musicAlbum{}
	for _, t := range tracks {
		if artist != "" && t.showArtist() != artist {
			continue
		}
		if !musicMatch(t, q) {
			continue
		}
		key := t.showArtist() + "\x00" + t.showAlbum()
		a := byAlbum[key]
		if a == nil {
			a = &musicAlbum{Artist: t.showArtist(), Album: t.showAlbum(), Root: t.Root, Path: t.Path}
			byAlbum[key] = a
		}
		a.Tracks++
		a.Duration += t.Duration
		a.HasCover = a.HasCover || t.HasCover
		if t.Year > 0 && (a.Year == 0 || t.Year < a.Year) {
			a.Year = t.Year // 取最早年份：再版/合集不至于把首发年份顶掉
		}
	}
	out := make([]musicAlbum, 0, len(byAlbum))
	for _, a := range byAlbum {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Artist != out[j].Artist {
			return out[i].Artist < out[j].Artist
		}
		if out[i].Year != out[j].Year {
			return out[i].Year < out[j].Year
		}
		return out[i].Album < out[j].Album
	})
	writeJSON(w, 0, "", out)
}

// GET /api/music/tracks?artist=&albumArtist=&album=&lib=&q=（都不带 = 全部曲目）
func handleMusicTracks(w http.ResponseWriter, r *http.Request) {
	gq := r.URL.Query()
	artist, albumArtist, album := strings.TrimSpace(gq.Get("artist")), strings.TrimSpace(gq.Get("albumArtist")), strings.TrimSpace(gq.Get("album"))
	q := strings.TrimSpace(gq.Get("q"))
	// lib 筛选：按曲库目录（cfg.MusicLibrary 的某个绝对路径）只看该文件夹下的歌。
	// 是前缀匹配而不是根匹配 —— 曲库目录可以是根下的任意一层子文件夹。
	lib := strings.TrimSpace(gq.Get("lib"))
	tracks := musicVisibleTracks(currentUserOf(r))
	out := make([]*musicTrack, 0, len(tracks))
	for _, t := range tracks {
		if artist != "" && t.showArtist() != artist {
			continue
		}
		if albumArtist != "" && t.AlbumArtist != albumArtist {
			continue
		}
		if album != "" && t.showAlbum() != album {
			continue
		}
		if lib != "" && !hasPathPrefix(t.full(), lib) {
			continue
		}
		if !musicMatch(t, q) {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if album == "" {
			// 全部曲目：歌手 → 专辑 → 碟号 → 曲号，跟实体唱片翻页一个顺序
			if a.showArtist() != b.showArtist() {
				return a.showArtist() < b.showArtist()
			}
			if a.showAlbum() != b.showAlbum() {
				return a.showAlbum() < b.showAlbum()
			}
		}
		if a.DiscNo != b.DiscNo {
			return a.DiscNo < b.DiscNo
		}
		if a.TrackNo != b.TrackNo {
			return a.TrackNo < b.TrackNo
		}
		return a.Title < b.Title
	})
	for _, t := range out {
		t.Q = musicQualityOf(t.Root, t.Path, t.Size, t.Mtime, t.Duration)
	}
	writeJSON(w, 0, "", out)
}

// ---------- 歌单 ----------

// GET    /api/music/playlist            → 列表
// POST   {name}                         → 新建
// PUT    {id, name}                     → 重命名
// DELETE ?id=                           → 删除（连带 items）
func handleMusicPlaylist(w http.ResponseWriter, r *http.Request) {
	user := currentUserOf(r)
	switch r.Method {
	case http.MethodGet:
		rows, err := db.Query(`SELECT p.id, p.name, p.ts, COUNT(i.id) FROM playlist p
			LEFT JOIN playlist_item i ON i.playlist_id = p.id
			WHERE p.username = ? GROUP BY p.id ORDER BY p.ts DESC`, user)
		if err != nil {
			writeJSON(w, 500, "读取失败", nil)
			return
		}
		defer rows.Close()
		type pl struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			Count int    `json:"count"`
		}
		out := []pl{}
		for rows.Next() {
			var p pl
			var ts int64
			if rows.Scan(&p.ID, &p.Name, &ts, &p.Count) == nil {
				out = append(out, p)
			}
		}
		writeJSON(w, 0, "", out)

	case http.MethodPost:
		var b struct{ Name string `json:"name"` }
		if !decodeBody(r, &b) || strings.TrimSpace(b.Name) == "" {
			writeJSON(w, 400, "歌单名不能为空", nil)
			return
		}
		_, err := db.Exec(`INSERT INTO playlist (username, name, ts) VALUES (?,?,?)`,
			user, strings.TrimSpace(b.Name), time.Now().Unix())
		if err == nil {
			flush()
		}
		writeJSONErr(w, err, "已创建")

	case http.MethodPut:
		var b struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		if !decodeBody(r, &b) || b.ID <= 0 || strings.TrimSpace(b.Name) == "" {
			writeJSON(w, 400, "参数不完整", nil)
			return
		}
		_, err := db.Exec(`UPDATE playlist SET name = ? WHERE id = ? AND username = ?`,
			strings.TrimSpace(b.Name), b.ID, user)
		if err == nil {
			flush()
		}
		writeJSONErr(w, err, "已保存")

	case http.MethodDelete:
		id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if id <= 0 {
			writeJSON(w, 400, "参数不完整", nil)
			return
		}
		// 先确认是自己的歌单，再删 —— 别让拼出来的 id 把别人的删了
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM playlist WHERE id = ? AND username = ?`, id, user).Scan(&n)
		if n == 0 {
			writeJSON(w, 404, "歌单不存在", nil)
			return
		}
		_, err1 := db.Exec(`DELETE FROM playlist WHERE id = ?`, id)
		_, err2 := db.Exec(`DELETE FROM playlist_item WHERE playlist_id = ?`, id)
		if err1 == nil && err2 == nil {
			flush()
		}
		writeJSONErr(w, firstErr(err1, err2), "已删除")

	default:
		writeJSON(w, 405, "只支持 GET/POST/PUT/DELETE", nil)
	}
}

// GET  ?id=                      → 曲目列表（stat 判断「已不在」）
// POST {id, tracks:[{root,path}]} → 追加
// PUT  {id, order:[{root,path}]} → 整列重写（拖拽排序 / 删除条目都走它）
func handleMusicPlaylistItems(w http.ResponseWriter, r *http.Request) {
	user := currentUserOf(r)
	type pt struct {
		Root string `json:"root"`
		Path string `json:"path"`
	}
	ownPlaylist := func(id int64) bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM playlist WHERE id = ? AND username = ?`, id, user).Scan(&n)
		return n > 0
	}
	switch r.Method {
	case http.MethodGet:
		id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if id <= 0 || !ownPlaylist(id) {
			writeJSON(w, 404, "歌单不存在", nil)
			return
		}
		rows, err := db.Query(`SELECT root, path, pos FROM playlist_item WHERE playlist_id = ? ORDER BY pos`, id)
		if err != nil {
			writeJSON(w, 500, "读取失败", nil)
			return
		}
		type item struct {
			pt
			Pos     int     `json:"pos"`
			Exists  bool    `json:"exists"`
			Title   string  `json:"title"`
			Artist  string  `json:"artist"`
			Album   string  `json:"album"`
			Dur     float64 `json:"duration"`
			Q       string  `json:"q,omitempty"`
		}
		out := []item{}
		for rows.Next() {
			var it item
			if rows.Scan(&it.Root, &it.Path, &it.Pos) != nil {
				continue
			}
			out = append(out, it)
		}
		rows.Close()
		// ⚠️ 标签/时长的补充查询必须放在 rows.Close() 之后：连接池只有 1 个连接，
		// 在迭代 rows 时再发查询会自己等自己，死锁（曾把整个库卡到超时）。
		for i := range out {
			it := &out[i]
			full := filepath.Join(normPath(it.Root), filepath.FromSlash(it.Path))
			if st, err := os.Stat(full); err == nil {
				it.Exists = true
				tag := readTrackTag(full, filepath.Base(full), st.ModTime().Unix(), st.Size())
				it.Title, it.Artist, it.Album = tag.Title, tag.Artist, tag.Album
				_ = db.QueryRow(`SELECT duration FROM music_track WHERE root=? AND path=?`, it.Root, it.Path).Scan(&it.Dur)
				it.Q = musicQualityOf(it.Root, it.Path, st.Size(), st.ModTime().Unix(), it.Dur)
			} else {
				it.Title = filepath.Base(it.Path)
			}
		}
		writeJSON(w, 0, "", out)

	case http.MethodPost:
		var b struct {
			ID     int64 `json:"id"`
			Tracks []pt  `json:"tracks"`
		}
		if !decodeBody(r, &b) || b.ID <= 0 || len(b.Tracks) == 0 {
			writeJSON(w, 400, "参数不完整", nil)
			return
		}
		if !ownPlaylist(b.ID) {
			writeJSON(w, 404, "歌单不存在", nil)
			return
		}
		var max int
		_ = db.QueryRow(`SELECT COALESCE(MAX(pos), -1) FROM playlist_item WHERE playlist_id = ?`, b.ID).Scan(&max)
		var err error
		for i, t := range b.Tracks {
			if t.Root == "" || t.Path == "" {
				continue
			}
			t.Root = canonicalRoot(t.Root) // 防「F://」这类脏 root 在库里另立一条（与 recent 同源的老坑）
			if _, e := db.Exec(`INSERT INTO playlist_item (playlist_id, root, path, pos, ts) VALUES (?,?,?,?,?)`,
				b.ID, t.Root, t.Path, max+1+i, time.Now().Unix()); e != nil {
				err = e
			}
		}
		if err == nil {
			flush()
		}
		writeJSONErr(w, err, "已加入歌单")

	case http.MethodPut:
		var b struct {
			ID    int64 `json:"id"`
			Order []pt  `json:"order"`
		}
		if !decodeBody(r, &b) || b.ID <= 0 || !ownPlaylist(b.ID) {
			writeJSON(w, 400, "参数不完整", nil)
			return
		}
		// 整列重写：拖拽排序/删条目都收敛到这一条路，不用做局部 pos 挪位
		tx, err := db.Begin()
		if err != nil {
			writeJSON(w, 500, "保存失败", nil)
			return
		}
		if _, err = tx.Exec(`DELETE FROM playlist_item WHERE playlist_id = ?`, b.ID); err != nil {
			tx.Rollback()
			writeJSON(w, 500, "保存失败", nil)
			return
		}
		for i, t := range b.Order {
			t.Root = canonicalRoot(t.Root)
			if _, err = tx.Exec(`INSERT INTO playlist_item (playlist_id, root, path, pos, ts) VALUES (?,?,?,?,?)`,
				b.ID, t.Root, t.Path, i, time.Now().Unix()); err != nil {
				tx.Rollback()
				writeJSON(w, 500, "保存失败", nil)
				return
			}
		}
		if err = tx.Commit(); err == nil {
			flush()
		}
		writeJSONErr(w, err, "已保存")

	default:
		writeJSON(w, 405, "只支持 GET/POST/PUT", nil)
	}
}

// ---------- 歌曲收藏 ----------
// 音乐自己的收藏，跟文件站的 favorite 完全独立：文件收藏收藏的是「文件」，
// 歌曲收藏收藏的是「这首歌」，各走各的表、各走各的接口。

// GET  → 收藏列表（LEFT JOIN 曲库拿标签；文件被移走又还没扫掉的，条目保留但不返回）
// POST {root, path} → 切换：没收藏就收藏，收藏了就取消
func handleMusicFav(w http.ResponseWriter, r *http.Request) {
	user := currentUserOf(r)
	switch r.Method {
	case http.MethodGet:
		rows, err := db.Query(`SELECT f.root, f.path, COALESCE(t.title,''), COALESCE(t.artist,''),
			COALESCE(t.album_artist,''), COALESCE(t.album,''), COALESCE(t.track_no,0), COALESCE(t.duration,0),
			COALESCE(t.size,0), COALESCE(t.mtime,0)
			FROM music_fav f
			INNER JOIN music_track t ON t.root = f.root AND t.path = f.path
			WHERE f.username = ? ORDER BY f.ts DESC`, user)
		if err != nil {
			writeJSON(w, 500, "读取失败", nil)
			return
		}
		defer rows.Close()
		type favTrack struct {
			Root        string  `json:"root"`
			Path        string  `json:"path"`
			Title       string  `json:"title"`
			Artist      string  `json:"artist"`
			AlbumArtist string  `json:"albumArtist"`
			Album       string  `json:"album"`
			TrackNo     int     `json:"trackNo"`
			Duration    float64 `json:"duration"`
			Size        int64   `json:"-"`
			Mtime       int64   `json:"-"`
			Q           string  `json:"q,omitempty"`
		}
		out := []favTrack{}
		for rows.Next() {
			var t favTrack
			if rows.Scan(&t.Root, &t.Path, &t.Title, &t.Artist, &t.AlbumArtist,
				&t.Album, &t.TrackNo, &t.Duration, &t.Size, &t.Mtime) == nil {
				t.Q = musicQualityOf(t.Root, t.Path, t.Size, t.Mtime, t.Duration)
				out = append(out, t)
			}
		}
		writeJSON(w, 0, "", out)

	case http.MethodPost:
		var b struct {
			Root string `json:"root"`
			Path string `json:"path"`
		}
		if !decodeBody(r, &b) || b.Root == "" || b.Path == "" {
			writeJSON(w, 400, "参数不完整", nil)
			return
		}
		b.Root = canonicalRoot(b.Root)
		// 只能收藏自己看得见的歌：受限账号不能靠拼 path 收藏白名单外的文件
		visible := false
		for _, t := range musicVisibleTracks(user) {
			if t.Root == b.Root && t.Path == b.Path {
				visible = true
				break
			}
		}
		if !visible {
			writeJSON(w, 404, "曲目不在曲库里", nil)
			return
		}
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM music_fav WHERE username = ? AND root = ? AND path = ?`,
			user, b.Root, b.Path).Scan(&n)
		if n > 0 {
			_, err := db.Exec(`DELETE FROM music_fav WHERE username = ? AND root = ? AND path = ?`,
				user, b.Root, b.Path)
			if err != nil {
				writeJSON(w, 500, "操作失败", nil)
				return
			}
			flush()
			writeJSON(w, 0, "已取消收藏", map[string]bool{"on": false})
		} else {
			_, err := db.Exec(`INSERT INTO music_fav (username, root, path, ts) VALUES (?,?,?,?)`,
				user, b.Root, b.Path, time.Now().Unix())
			if err != nil {
				writeJSON(w, 500, "操作失败", nil)
				return
			}
			flush()
			writeJSON(w, 0, "已收藏", map[string]bool{"on": true})
		}

	default:
		writeJSON(w, 405, "只支持 GET/POST", nil)
	}
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func writeJSONErr(w http.ResponseWriter, err error, okMsg string) {
	if err != nil {
		writeJSON(w, 500, "保存失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, okMsg, nil)
}
