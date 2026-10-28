package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- 路径解析 ----------

var errOutside = errors.New("路径越界")

func findRoot(rootPath string) (Root, bool) {
	for _, r := range cfg.Roots {
		if strings.EqualFold(normPath(r.Path), normPath(rootPath)) {
			return r, true
		}
	}
	return Root{}, false
}

// resolve 把「根 + 相对路径」解析成绝对路径，并确保不越出根目录
func resolve(rootPath, rel string) (string, Root, error) {
	rt, ok := findRoot(rootPath)
	if !ok {
		return "", Root{}, errOutside
	}
	base, err := filepath.Abs(rt.Path)
	if err != nil {
		return "", rt, err
	}
	rel = strings.ReplaceAll(rel, "\\", "/")
	rel = strings.TrimPrefix(rel, "/")
	full := filepath.Join(base, filepath.FromSlash(rel))
	full = filepath.Clean(full)
	// 用 Rel 判断是否逃出根目录：base 形如 D:\ 时自带尾部反斜杠，不能用「base+分隔符」做前缀比较
	inside, err := filepath.Rel(base, full)
	if err != nil {
		return "", rt, errOutside
	}
	if inside == ".." || strings.HasPrefix(inside, ".."+string(os.PathSeparator)) || filepath.IsAbs(inside) {
		return "", rt, errOutside
	}
	return full, rt, nil
}

// relOf 反向：绝对路径 → 相对根的路径（用 / 分隔）
func relOf(base, full string) string {
	rel, err := filepath.Rel(base, full)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return ""
	}
	return rel
}

func safeName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\:*?"<>|`) {
		return false
	}
	return true
}

func fileKind(name string, isDir bool) string {
	if isDir {
		return "dir"
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4", ".m4v", ".mov", ".webm", ".mkv", ".avi", ".ts", ".flv", ".wmv":
		return "video"
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".heic", ".heif", ".bmp", ".tif", ".tiff":
		return "image"
	case ".mp3", ".m4a", ".wav", ".flac", ".aac", ".ogg":
		return "audio"
	case ".zip", ".7z", ".rar", ".tar", ".gz":
		return "archive"
	}
	return "file"
}

// ---------- 接口 ----------

func handleFSRoots(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name     string `json:"name"`
		Path     string `json:"path"`
		Drive    string `json:"drive"`
		Readonly bool   `json:"readonly"`
		FreeGB   string `json:"freeGB"`
		TotalGB  string `json:"totalGB"`
	}
	var out []item
	for _, rt := range cfg.Roots {
		if !rt.Enabled {
			continue
		}
		if st, err := os.Stat(rt.Path); err != nil || !st.IsDir() {
			continue
		}
		it := item{Name: rt.Name, Path: rt.Path, Drive: driveOf(rt.Path), Readonly: rt.Readonly}
		if total, free := volumeSpace(rt.Path); total > 0 {
			it.TotalGB = humanGB(total)
			it.FreeGB = humanGB(free)
		}
		out = append(out, it)
	}
	writeJSON(w, 0, "ok", out)
}

func handleFSList(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("root")
	rel := r.URL.Query().Get("path")
	full, rt, err := resolve(root, rel)
	if err != nil {
		writeJSON(w, 403, "无权访问该路径", nil)
		return
	}
	st, err := os.Stat(full)
	if err != nil || !st.IsDir() {
		writeJSON(w, 404, "目录不存在", nil)
		return
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		writeJSON(w, 500, "读取失败: "+err.Error(), nil)
		return
	}
	type entry struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		IsDir bool   `json:"isDir"`
		Size  int64  `json:"size"`
		MTime int64  `json:"mtime"`
		Kind  string `json:"kind"`
		Thumb bool   `json:"thumb"` // 前端据此用 <img> 请求 /api/fs/thumb，false 就显示图标
	}
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "~$") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		isDir := e.IsDir()
		out = append(out, entry{
			Name:  name,
			Path:  relOf(mustAbs(rt.Path), filepath.Join(full, name)),
			IsDir: isDir,
			Size:  info.Size(),
			MTime: info.ModTime().Unix(),
			Kind:  fileKind(name, isDir),
			Thumb: !isDir && thumbableExt(filepath.Ext(name)),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	base, _ := filepath.Abs(rt.Path)
	parent := ""
	if relOf(base, full) != "" {
		p := filepath.Dir(full)
		parent = relOf(base, p)
	}
	writeJSON(w, 0, "ok", map[string]any{
		"root":     rt.Path,
		"rootName": rt.Name,
		"path":     relOf(base, full),
		"parent":   parent,
		"readonly": rt.Readonly,
		"entries":  out,
	})
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

func handleFSDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	full, rt, err := resolve(q.Get("root"), q.Get("path"))
	if err != nil {
		log.Printf("[下载] %s %s 拒绝：路径越界 path=%q", clientIP(r), uaTag(r.UserAgent()), q.Get("path"))
		http.Error(w, "禁止访问", http.StatusForbidden)
		return
	}
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		log.Printf("[下载] %s %s 不存在 path=%q", clientIP(r), uaTag(r.UserAgent()), q.Get("path"))
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}
	rel := relOf(mustAbs(rt.Path), full)
	log.Printf("[下载] %s %s 开始 %s 大小=%d range=%q",
		clientIP(r), uaTag(r.UserAgent()), rel, st.Size(), r.Header.Get("Range"))
	recordTransfer(transferRec{
		Kind: "download", IP: clientIP(r), UA: uaTag(r.UserAgent()),
		Root: rt.Path, Path: rel, Size: st.Size(), OK: true,
	})
	inline := q.Get("inline") == "1"
	if !inline {
		w.Header().Set("Content-Disposition",
			"attachment; filename*=UTF-8''"+url.PathEscape(filepath.Base(full)))
	}
	// ServeFile 自带 Range / If-Range，手机可拖进度
	http.ServeFile(w, r, full)
}

func handleFSUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	t0 := time.Now()
	ip := clientIP(r)
	ua := uaTag(r.UserAgent())
	q := r.URL.Query()
	dirFull, rt, err := resolve(q.Get("root"), q.Get("dir"))
	if err != nil {
		log.Printf("[上传] %s %s 拒绝：路径越界 root=%q dir=%q", ip, ua, q.Get("root"), q.Get("dir"))
		writeJSON(w, 403, "无权访问该路径", nil)
		return
	}
	if rt.Readonly {
		log.Printf("[上传] %s %s 拒绝：目录只读 %s", ip, ua, dirFull)
		writeJSON(w, 403, "该目录为只读", nil)
		return
	}
	name := q.Get("name")
	if !safeName(name) {
		log.Printf("[上传] %s %s 拒绝：文件名非法 name=%q", ip, ua, name)
		writeJSON(w, 400, "文件名非法（不能包含 \\ / : * ? \" < > |）", nil)
		return
	}
	var offset, size int64
	parseInt64(q.Get("offset"), &offset)
	parseInt64(q.Get("size"), &size)

	if err := os.MkdirAll(dirFull, 0o755); err != nil {
		log.Printf("[上传] %s %s 建目录失败 %s: %v", ip, ua, dirFull, err)
		writeJSON(w, 500, "无法创建目录: "+err.Error(), nil)
		return
	}
	dst := filepath.Join(dirFull, name)
	part := dst + ".part"

	if size <= 0 {
		writeJSON(w, 400, "缺少文件大小", nil)
		return
	}
	if offset < 0 || offset >= size {
		writeJSON(w, 400, "分片位置越界", nil)
		return
	}

	// offset==0 且已有成品 → 这是重新上传，清掉旧产物（含并行用的区间记录）
	if offset == 0 {
		if st, e := os.Stat(dst); e == nil && !st.IsDir() {
			_ = os.Remove(dst)
			_ = os.Remove(part)
			_ = os.Remove(part + ".meta")
		}
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("[上传] %s %s 打开失败 %s: %v", ip, ua, part, err)
		writeJSON(w, 500, "写入失败: "+err.Error(), nil)
		return
	}
	// 允许乱序：多流并发时后一片可能先到。
	// 旧逻辑用「offset != 当前文件大小就 409」把写入顺序锁死了，并发无从谈起。
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		writeJSON(w, 500, "定位失败: "+err.Error(), nil)
		return
	}
	n, copyErr := io.Copy(f, r.Body)
	wrote := offset + n
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		log.Printf("[上传] %s %s 中断 name=%s 已写=%d 区间=%d→%d size=%d 错误=%v/%v",
			ip, ua, name, wrote, offset, wrote, size, copyErr, closeErr)
		// 已落盘的字节仍然是有效的，记进区间表，重试时不用重传
		if wrote > offset {
			updateUploadMeta(part, size, offset, wrote)
		}
		recordTransfer(transferRec{
			Kind: "upload", IP: ip, UA: ua, Root: rt.Path, Path: name,
			Size: size, Offset: wrote, OK: false, Note: "interrupted",
			MS: time.Since(t0).Milliseconds(),
		})
		writeJSON(w, 500, "写入中断，可从断点继续", map[string]any{"offset": wrote})
		return
	}
	if wrote > size {
		_ = os.Truncate(part, size)
		wrote = size
	}
	if wrote < offset {
		wrote = offset
	}

	meta, complete := updateUploadMeta(part, size, offset, wrote)
	covered := coveredBytes(meta.Ranges)

	// 逐片速度，用来判断「越来越慢」慢在链路上还是磁盘上
	{
		el := time.Since(t0)
		rate := 0.0
		if el > 0 {
			rate = float64(n) / el.Seconds() / (1024 * 1024)
		}
		log.Printf("[上传] %s %s 片 name=%s %d→%d/%d 本片 %.1fMB 用时 %s 速率 %.2fMB/s 总进度 %.1f%%",
			ip, ua, name, offset, wrote, size,
			float64(n)/(1024*1024), el.Round(time.Millisecond), rate,
			float64(covered)*100/float64(size))
	}

	if complete {
		if err := os.Rename(part, dst); err != nil {
			log.Printf("[上传] %s %s 收尾失败 name=%s: %v", ip, ua, name, err)
			writeJSON(w, 500, "收尾失败: "+err.Error(), nil)
			return
		}
		_ = os.Remove(part + ".meta")
		log.Printf("[上传] %s %s 完成 name=%s 大小=%d 用时=%s",
			ip, ua, name, wrote, time.Since(t0).Round(time.Millisecond))
		recordTransfer(transferRec{
			Kind: "upload", IP: ip, UA: ua, Root: rt.Path, Path: name,
			Size: size, Offset: size, OK: true,
			MS: time.Since(t0).Milliseconds(),
		})
		writeJSON(w, 0, "上传完成", map[string]any{"offset": size, "covered": size, "done": true})
		return
	}
	writeJSON(w, 0, "分片已写入", map[string]any{
		"offset":  highWater(meta.Ranges),
		"covered": covered,
		"done":    false,
	})
}

// ---------- 并行分片的区间记录 ----------
//
// 多流并发时写入是乱序的，单看文件大小无法判断「收齐了没有」。
// 用 name.part.meta 记一份已完成区间表：每片写完合并进去，区间并成 [0,size) 才算完成。
// 顺带把断点续传做准了——重传时只需要补缺失的区间。

type uploadMeta struct {
	Size   int64      `json:"size"`
	Ranges [][2]int64 `json:"ranges"`
}

var upMetaMu sync.Mutex

// updateUploadMeta 合并已写入区间并落盘（原子替换），返回最新表和是否已收齐
func updateUploadMeta(part string, size, from, to int64) (uploadMeta, bool) {
	upMetaMu.Lock()
	defer upMetaMu.Unlock()

	m := uploadMeta{Size: size}
	if b, err := os.ReadFile(part + ".meta"); err == nil {
		var old uploadMeta
		if json.Unmarshal(b, &old) == nil && old.Size == size {
			m = old
		}
	}
	m.Ranges = mergeRanges(m.Ranges, from, to)

	if b, err := json.Marshal(m); err == nil {
		tmp := part + ".meta.tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			_ = os.Rename(tmp, part + ".meta")
		}
	}
	return m, coveredBytes(m.Ranges) >= size
}

// mergeRanges 把 [from,to) 并入有序区间表，相邻/重叠的合并
func mergeRanges(rs [][2]int64, from, to int64) [][2]int64 {
	if to <= from {
		return rs
	}
	out := make([][2]int64, 0, len(rs)+1)
	placed := false
	for _, r := range rs {
		if r[1] < from || r[0] > to {
			if !placed && r[0] > to {
				out = append(out, [2]int64{from, to})
				placed = true
			}
			out = append(out, r)
			continue
		}
		// 有重叠 → 扩到并集
		if r[0] < from {
			from = r[0]
		}
		if r[1] > to {
			to = r[1]
		}
	}
	if !placed {
		out = append(out, [2]int64{from, to})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })

	// 二次合并（扩并集后可能和前一段接上）
	merged := out[:0]
	for _, r := range out {
		if n := len(merged); n > 0 && r[0] <= merged[n-1][1] {
			if r[1] > merged[n-1][1] {
				merged[n-1][1] = r[1]
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

func coveredBytes(rs [][2]int64) int64 {
	var sum int64
	for _, r := range rs {
		sum += r[1] - r[0]
	}
	return sum
}

// highWater 从 0 开始连续已写入到哪（顺序上传时等于「已写字节数」）
func highWater(rs [][2]int64) int64 {
	if len(rs) == 0 || rs[0][0] != 0 {
		return 0
	}
	return rs[0][1]
}

// missingRanges 还没收到的区间
func missingRanges(m uploadMeta) [][2]int64 {
	var out [][2]int64
	var cur int64
	for _, r := range m.Ranges {
		if r[0] > cur {
			out = append(out, [2]int64{cur, r[0]})
		}
		if r[1] > cur {
			cur = r[1]
		}
	}
	if cur < m.Size {
		out = append(out, [2]int64{cur, m.Size})
	}
	return out
}

func handleFSUploadStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dirFull, _, err := resolve(q.Get("root"), q.Get("dir"))
	if err != nil {
		writeJSON(w, 403, "无权访问该路径", nil)
		return
	}
	name := q.Get("name")
	if !safeName(name) {
		writeJSON(w, 400, "文件名非法", nil)
		return
	}
	var want int64
	parseInt64(q.Get("size"), &want)

	part := filepath.Join(dirFull, name) + ".part"
	partSize := int64(0)
	if st, err := os.Stat(part); err == nil {
		partSize = st.Size()
	}
	exists := false
	if st, err := os.Stat(filepath.Join(dirFull, name)); err == nil && !st.IsDir() {
		exists = true
	}

	// 有区间表就按区间算（并行上传可能留空洞）；没有就退回「已连续写入 partSize」
	meta := uploadMeta{Size: want}
	hasMeta := false
	if b, err := os.ReadFile(part + ".meta"); err == nil {
		var m uploadMeta
		if json.Unmarshal(b, &m) == nil && (want <= 0 || m.Size == want) {
			meta = m
			hasMeta = true
		}
	}
	if !hasMeta {
		meta = uploadMeta{Size: want}
		if partSize > 0 {
			meta.Ranges = [][2]int64{{0, partSize}}
			if want > 0 && partSize > want {
				meta.Ranges = [][2]int64{{0, want}}
			}
		}
	}

	data := map[string]any{
		"offset":      partSize,                  // 兼容旧客户端：已写入的文件大小
		"covered":     coveredBytes(meta.Ranges), // 已收到的不重复字节数
		"exists":      exists,
		"concurrency": cfg.UploadConcurrency, // 客户端按这个开并发流
	}
	if want > 0 {
		if exists {
			// 成品已经在，其余字段就没有意义了，明确成「齐了」免得客户端误判
			data["covered"] = want
			data["missing"] = [][2]int64{}
		} else {
			data["missing"] = missingRanges(meta)
		}
	}
	writeJSON(w, 0, "ok", data)
}

func handleFSMkdir(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Root string `json:"root"`
		Dir  string `json:"dir"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	dirFull, rt, err := resolve(body.Root, body.Dir)
	if err != nil {
		writeJSON(w, 403, "无权访问该路径", nil)
		return
	}
	if rt.Readonly {
		writeJSON(w, 403, "该目录为只读", nil)
		return
	}
	if !safeName(body.Name) {
		writeJSON(w, 400, "文件夹名非法", nil)
		return
	}
	target := filepath.Join(dirFull, body.Name)
	if _, err := os.Stat(target); err == nil {
		writeJSON(w, 409, "同名文件夹已存在", nil)
		return
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		writeJSON(w, 500, "创建失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已创建", nil)
}

func handleFSRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Root    string `json:"root"`
		Path    string `json:"path"`
		NewName string `json:"newName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	full, rt, err := resolve(body.Root, body.Path)
	if err != nil {
		writeJSON(w, 403, "无权访问该路径", nil)
		return
	}
	if rt.Readonly {
		writeJSON(w, 403, "该目录为只读", nil)
		return
	}
	if !safeName(body.NewName) {
		writeJSON(w, 400, "名称非法", nil)
		return
	}
	target := filepath.Join(filepath.Dir(full), body.NewName)
	if strings.EqualFold(target, full) {
		writeJSON(w, 0, "名称未变化", nil)
		return
	}
	if _, err := os.Stat(target); err == nil {
		writeJSON(w, 409, "同名文件已存在", nil)
		return
	}
	if err := os.Rename(full, target); err != nil {
		writeJSON(w, 500, "重命名失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已重命名", nil)
}

func handleFSDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Root string `json:"root"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	if strings.TrimSpace(body.Path) == "" {
		writeJSON(w, 400, "不允许删除根目录", nil)
		return
	}
	full, rt, err := resolve(body.Root, body.Path)
	if err != nil {
		writeJSON(w, 403, "无权访问该路径", nil)
		return
	}
	if rt.Readonly {
		writeJSON(w, 403, "该目录为只读", nil)
		return
	}
	base, _ := filepath.Abs(rt.Path)
	if strings.EqualFold(filepath.Clean(full), filepath.Clean(base)) {
		writeJSON(w, 400, "不允许删除根目录", nil)
		return
	}
	if err := os.RemoveAll(full); err != nil {
		log.Printf("[删除] %s %s 失败 %s: %v", clientIP(r), uaTag(r.UserAgent()), full, err)
		writeJSON(w, 500, "删除失败: "+err.Error(), nil)
		return
	}
	log.Printf("[删除] %s %s 已删除 %s", clientIP(r), uaTag(r.UserAgent()), full)
	recordTransfer(transferRec{
		Kind: "delete", IP: clientIP(r), UA: uaTag(r.UserAgent()),
		Root: rt.Path, Path: body.Path, OK: true,
	})
	writeJSON(w, 0, "已删除", nil)
}

func parseInt64(s string, out *int64) {
	if s == "" {
		return
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return
		}
		n = n*10 + int64(c-'0')
	}
	*out = n
}

var _ = time.Now

// handleFSMove 把文件/文件夹移到同一根目录下的另一个文件夹。
// 只支持同一根（同一块盘）内的移动：跨盘用 os.Rename 在 Windows 上必然失败，
// 而"跨盘移动"本质是拷贝+删除，语义和风险都不一样，先不做。
func handleFSMove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Root    string `json:"root"`
		Path    string `json:"path"`
		DestDir string `json:"destDir"`
		NewName string `json:"newName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	src, rt, err := resolve(body.Root, body.Path)
	if err != nil {
		writeJSON(w, 403, "无权访问该路径", nil)
		return
	}
	if rt.Readonly {
		writeJSON(w, 403, "该目录为只读", nil)
		return
	}
	if strings.EqualFold(mustAbs(src), mustAbs(rt.Path)) {
		writeJSON(w, 400, "不能移动根目录", nil)
		return
	}
	st, err := os.Stat(src)
	if err != nil {
		writeJSON(w, 404, "源文件不存在", nil)
		return
	}
	dstDir, _, err := resolve(body.Root, body.DestDir)
	if err != nil {
		writeJSON(w, 403, "无权访问目标路径", nil)
		return
	}
	ds, err := os.Stat(dstDir)
	if err != nil || !ds.IsDir() {
		writeJSON(w, 400, "目标不是文件夹", nil)
		return
	}
	// 文件夹不能移进自己或自己的子孙目录里（否则 Rename 会把整棵树弄坏）
	if st.IsDir() {
		s := strings.TrimRight(mustAbs(src), string(os.PathSeparator)) + string(os.PathSeparator)
		d := strings.TrimRight(mustAbs(dstDir), string(os.PathSeparator)) + string(os.PathSeparator)
		if strings.HasPrefix(strings.ToLower(d), strings.ToLower(s)) {
			writeJSON(w, 400, "不能把文件夹移到它自己里面", nil)
			return
		}
	}
	name := body.NewName
	if name == "" {
		name = filepath.Base(src)
	}
	if !safeName(name) {
		writeJSON(w, 400, "名称非法", nil)
		return
	}
	target := filepath.Join(dstDir, name)
	if strings.EqualFold(filepath.Dir(target), filepath.Dir(src)) {
		writeJSON(w, 0, "已经在这个文件夹里了", nil)
		return
	}
	if _, err := os.Stat(target); err == nil {
		writeJSON(w, 409, "目标文件夹里有同名文件", nil)
		return
	}
	if err := os.Rename(src, target); err != nil {
		log.Printf("[移动] %s %s 失败 %s -> %s: %v", clientIP(r), uaTag(r.UserAgent()), src, target, err)
		writeJSON(w, 500, "移动失败（跨盘不能移动）: "+err.Error(), nil)
		return
	}
	log.Printf("[移动] %s %s %s -> %s", clientIP(r), uaTag(r.UserAgent()), src, target)
	writeJSON(w, 0, "已移动", map[string]any{"path": relOf(mustAbs(rt.Path), target)})
}
