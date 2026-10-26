package main

import (
	"crypto/sha1"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 把「换封装」的结果落盘成真正的 MP4，而不是每次都临时推流。
//
// 为什么值得：流式 fMP4 没有索引，浏览器不知道往哪跳 —— **不能拖进度**。
// 落盘一个带 faststart 的普通 MP4，就能交给 http.ServeFile 走 Range：
// 拖哪播哪、秒开、而且第二次播放完全不占 CPU。
// 代价是第一次要等几秒（copy 不解码不编码，700MB 的 4K 集约 4 秒）。

var (
	cacheMu    sync.Mutex
	cacheJobs  = map[string]*cacheJob{} // key -> 正在生成的任务
	cacheDir   string
	cacheLimit int64 = 30 << 30 // 30GB：超了就删最旧的，别把盘塞满
)

type cacheJob struct {
	done chan struct{}
	err  string
}

func initCacheDir() {
	cacheDir = filepath.Join(filepath.Dir(cfgPath), "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		log.Printf("[缓存] 建目录失败 %s: %v", cacheDir, err)
		cacheDir = ""
		return
	}
	log.Printf("[缓存] 目录 %s，上限 %.0fGB", cacheDir, float64(cacheLimit)/(1<<30))
}

// cacheKey 把「同一个文件 + 同一种处理方式」映射到同一个缓存文件。
// 用 大小 + 修改时间 参与计算：文件被替换过就自然失效，不用额外记版本。
func cacheKey(full string, st os.FileInfo, tag string) string {
	h := sha1.Sum([]byte(tag + "|" + full + "|" + st.ModTime().UTC().String()))
	return hex.EncodeToString(h[:])
}

func cachePath(key string) string {
	return filepath.Join(cacheDir, key[:2], key+".mp4")
}

// prepMP4 同步生成（或直接命中）可拖动的 MP4 缓存，返回它的绝对路径。
// 同一个文件被同时请求多次时只跑一次 ffmpeg，其它请求等结果。
func prepMP4(full string, st os.FileInfo, key string, tag string, enc string, rot bool) (string, string) {
	target := cachePath(key)
	if fi, err := os.Stat(target); err == nil && fi.Size() > 0 {
		return target, ""
	}

	cacheMu.Lock()
	if j, ok := cacheJobs[key]; ok {
		cacheMu.Unlock()
		<-j.done
		if j.err != "" {
			return "", j.err
		}
		return target, ""
	}
	j := &cacheJob{done: make(chan struct{})}
	cacheJobs[key] = j
	cacheMu.Unlock()

	defer func() {
		cacheMu.Lock()
		delete(cacheJobs, key)
		cacheMu.Unlock()
		close(j.done)
	}()

	ffm, _ := findFF()
	if ffm == "" {
		j.err = "服务器没有 ffmpeg"
		return "", j.err
	}
	if cacheDir == "" {
		j.err = "缓存目录不可用"
		return "", j.err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		j.err = "建缓存目录失败"
		return "", j.err
	}

	vcodec, acodec := probeCodecs(full)
	args := []string{"-nostdin", "-loglevel", "error", "-fflags", "+genpts"}
	// 旋转靠的是 MP4 的 display matrix（tkhd 里那个 3x3 矩阵），不是转像素：
	// 播放器读到矩阵自己转，所以**可以 copy**，700MB 的 4K 集照样 1 秒出结果。
	// 注意 ffmpeg 的 -display_rotation 是逆时针为正，要顺时针 90° 得给 -90。
	// 必须放在 -i 之前 —— 它是输入侧选项。
	if rot {
		args = append(args, "-display_rotation", "-90")
	}
	args = append(args, "-i", full, "-map", "0:v:0", "-map", "0:a:0?")
	switch {
	case rot:
		// 带了旋转就不要再编码：编出来的像素是转过的，矩阵再转一次就转回去了
		if vcodec == "hevc" {
			args = append(args, "-c:v", "copy", "-tag:v", "hvc1")
		} else {
			args = append(args, "-c:v", "copy")
		}
	case enc == "h264":
		// 客户端解不了 HEVC（Windows 版 Chrome）时的缓存版本：真转码，只做一次
		args = append(args, "-c:v", "h264_nvenc", "-preset", "p4", "-cq", "23", "-pix_fmt", "yuv420p")
	case vcodec == "hevc":
		// Apple 只认 hvc1，这条对 iPhone 是刚需
		args = append(args, "-c:v", "copy", "-tag:v", "hvc1")
	default:
		args = append(args, "-c:v", "copy")
	}
	if acodec == "aac" {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
	}
	// faststart：把索引(moov)挪到文件头，浏览器拿到第一块就知道总时长、能立刻拖
	//
	// 临时文件名必须**唯一**：曾经用固定的 xx.mp4.part，两个任务撞在一起时
	// 会互相覆盖，第二个 Rename 时文件还被 ffmpeg 占着，Windows 上直接
	// ERROR_ACCESS_DENIED —— 日志里就一句「落盘失败」，很难查。
	tmp := target + "." + strconv.FormatInt(time.Now().UnixNano(), 36) + ".part"
	args = append(args, "-sn", "-dn", "-movflags", "+faststart", "-f", "mp4", tmp)

	t0 := time.Now()
	out, err := exec.Command(ffm, args...).CombinedOutput()
	if err != nil {
		_ = os.Remove(tmp)
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		j.err = "准备失败: " + msg
		log.Printf("[缓存] 生成失败 %s: %v %s", filepath.Base(full), err, msg)
		return "", j.err
	}
	// 目标已经在了（另一个任务刚做完）就直接用，别覆盖
	if fi, e := os.Stat(target); e == nil && fi.Size() > 0 {
		_ = os.Remove(tmp)
		log.Printf("[缓存] 别的任务已经做好了 %s", filepath.Base(target))
		return target, ""
	}
	// Windows 上目标可能被杀毒/索引服务短暂占用，重试几次再认输
	var rerr error
	for i := 0; i < 5; i++ {
		if rerr = os.Rename(tmp, target); rerr == nil {
			break
		}
		time.Sleep(time.Duration(150*(i+1)) * time.Millisecond)
	}
	if rerr != nil {
		_ = os.Remove(tmp)
		j.err = "落盘失败: " + rerr.Error()
		log.Printf("[缓存] 落盘失败 %s -> %s: %v", filepath.Base(tmp), filepath.Base(target), rerr)
		return "", j.err
	}
	// 打产物的大小，不是源文件的大小 —— 这俩在转码时差好几倍，写错了看日志会误判
	outSz := float64(st.Size())
	if fi, e := os.Stat(target); e == nil {
		outSz = float64(fi.Size())
	}
	log.Printf("[缓存] 生成 %s 用时=%.1fs 产物=%.1fMB 源=%.1fMB", filepath.Base(target), time.Since(t0).Seconds(),
		outSz/(1<<20), float64(st.Size())/(1<<20))
	trimCache()
	return target, ""
}

// trimCache 超过上限就按最旧优先删。只在生成完一个新文件时跑，够用且不打扰播放。
func trimCache() {
	var files []struct {
		p     string
		size  int64
		mtime time.Time
	}
	var total int64
	_ = filepath.Walk(cacheDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || strings.HasSuffix(p, ".part") {
			return nil
		}
		files = append(files, struct {
			p     string
			size  int64
			mtime time.Time
		}{p, fi.Size(), fi.ModTime()})
		total += fi.Size()
		return nil
	})
	if total <= cacheLimit {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	for _, f := range files {
		if total <= cacheLimit {
			break
		}
		if os.Remove(f.p) == nil {
			total -= f.size
			log.Printf("[缓存] 超上限，删掉旧的 %s (%.1fMB)", filepath.Base(f.p), float64(f.size)/(1<<20))
		}
	}
}

// handleFSPrep 前端问：「这个文件能不能给我一个**能拖进度**的地址？」
//
//   - 本来就是 MP4 系      → 直接给下载直链（ServeFile 自带 Range）
//   - MKV 里是 H.264/HEVC  → 换封装落盘成 MP4 缓存，再给直链（第一次几秒）
//   - 客户端解不了 HEVC     → 允许 enc=h264 转码后再缓存
//   - 真要重编码的老编码    → 说清楚做不到，前端退回流式播放
//
// mode：
//   - sync（默认）：没有就现场生成，生成完再回（第一次会等几秒到几分钟）
//   - nowait：有就给，没有就说没有（前端用它判断"能不能直接拖"）
//   - bg：有就给；没有就立刻回、后台慢慢生成（用户先流式看着，下次就能拖）
func handleFSPrep(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	full, _, err := resolve(q.Get("root"), q.Get("path"))
	if err != nil {
		writeJSON(w, 403, "无权访问该路径", nil)
		return
	}
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		writeJSON(w, 404, "文件不存在", nil)
		return
	}
	name := filepath.Base(full)
	dl := "/api/fs/download?" + q.Encode() + "&inline=1"
	mode := q.Get("mode")
	enc := q.Get("enc")
	// rot=1：要一份「顺时针转 90°」的版本。靠 display matrix 实现，不重编码。
	// iOS 的系统播放器接管全屏后，网页什么都改不了 —— 唯一能让它转过来的办法
	// 就是进去之前把源换成这个带矩阵的版本。
	rot := q.Get("rot") == "1"
	if rot {
		enc = "" // 旋转只用 copy；转码会连像素一起转，矩阵再转一次就白转了
	}

	ext := strings.ToLower(filepath.Ext(name))
	// 原生 mp4 平时直接给直链（ServeFile 带 Range，本来就能拖）；
	// 但要旋转时必须过一遍缓存，makemp4 才能把矩阵写进去
	if !rot && !needRemux(name) && ext != ".mp4" && ext != ".m4v" && ext != ".mov" {
		writeJSON(w, 0, "", map[string]any{"url": dl, "how": "原生", "ready": true})
		return
	}
	if !rot && (ext == ".mp4" || ext == ".m4v" || ext == ".mov") {
		writeJSON(w, 0, "", map[string]any{"url": dl, "how": "原生", "ready": true})
		return
	}

	vcodec, acodec := probeCodecs(full)
	if enc != "h264" && vcodec != "h264" && vcodec != "hevc" {
		writeJSON(w, 0, "", map[string]any{
			"stream": true, "vcodec": vcodec, "acodec": acodec,
			"msg": "这个编码（" + vcodec + "）没法直接换封装，只能边转边播、不能拖进度",
		})
		return
	}

	tag := "remux-" + vcodec + "-" + acodec
	if enc == "h264" {
		tag = "h264-" + vcodec + "-" + acodec
	}
	if rot {
		tag += "-rot90"
	}
	key := cacheKey(full, st, tag)
	target := cachePath(key)
	hit := false
	if fi, e := os.Stat(target); e == nil && fi.Size() > 0 {
		hit = true
	}

	if !hit && mode == "nowait" {
		writeJSON(w, 0, "", map[string]any{"ready": false, "vcodec": vcodec, "acodec": acodec})
		return
	}

	if !hit && mode == "bg" {
		go func() {
			t0 := time.Now()
			if _, perr := prepMP4(full, st, key, tag, enc, rot); perr != "" {
				log.Printf("[缓存] 后台生成失败 name=%s: %s", name, perr)
				return
			}
			log.Printf("[缓存] 后台生成完成 name=%s 用时=%.1fs", name, time.Since(t0).Seconds())
		}()
		writeJSON(w, 0, "", map[string]any{"ready": false, "preparing": true, "vcodec": vcodec, "acodec": acodec})
		return
	}

	t0 := time.Now()
	got, perr := prepMP4(full, st, key, tag, enc, rot)
	if perr != "" {
		writeJSON(w, 500, perr, nil)
		return
	}
	log.Printf("[缓存] %s name=%s 用时=%.1fs", map[bool]string{true: "命中", false: "生成"}[hit], name, time.Since(t0).Seconds())
	writeJSON(w, 0, "", map[string]any{
		"url": "/api/fs/cache/" + filepath.Base(got), "how": "换封装", "ready": true,
		"vcodec": vcodec, "acodec": acodec,
	})
}

// handleFSCacheFile 把缓存目录里的文件发出去（同样走 ServeFile，支持 Range）。
func handleFSCacheFile(w http.ResponseWriter, r *http.Request) {
	if cacheDir == "" {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/fs/cache/")
	// 只允许「两段定长 hex + .mp4」，避免任何路径穿越
	if len(name) != 44 || !strings.HasSuffix(name, ".mp4") {  // sha1 40 + ".mp4"
		http.NotFound(w, r)
		return
	}
	for _, c := range name[:len(name)-4] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			http.NotFound(w, r)
			return
		}
	}
	full := filepath.Join(cacheDir, name[:2], name)
	if _, err := os.Stat(full); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, full)
}
