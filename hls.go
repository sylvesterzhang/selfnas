package main

// HLS 直播转码 —— 专给 WebKit(Safari / iPhone / iPad 全系浏览器，含 iOS 上的
// Chrome/Edge——它们渲染层都是 WKWebView)用的「边转边播」通道。
//
// 背景(2026-11-14 实锤)：WebKit 不吃「无 Content-Length、无 Range 的渐进式 fMP4
// 流」——手机实测拉 5.3MB/4.4s 就被浏览器掐线，黑屏打不开；同一条流本机拉 75 秒
// 能持续收 331MB，服务端没问题。Safari 系原生支持 <video src="*.m3u8">，所以对
// 这类客户端改走 HLS：ffmpeg 边转边写 ts 分片 + event 型 m3u8(不断追加，已转出
// 的部分还能拖)。桌面 Chrome/安卓的 Blink 对渐进式 fMP4 没问题，继续走 /api/fs/play。
//
// 生命周期：job 按「文件+编码」做 key，同一部片多人同看共享一份转码；
// 闲置 4 分钟自动杀 ffmpeg 并删分片目录(暂停超过 4 分钟再回来会重新转，可接受)。

import (
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

type hlsJob struct {
	key    string
	dir    string
	cmd    *exec.Cmd
	exited chan struct{} // 当前 ffmpeg 进程结束时关闭
	last   time.Time     // 最近一次被访问(拉 playlist 或分片)的时间
	sw     bool          // 当前进程用 CPU 编码(显卡失败后的重试)
	dead   bool          // 重试也失败，别再拉起
}

var (
	hlsMu      sync.Mutex
	hlsJobs    = map[string]*hlsJob{}
	hlsJanitor sync.Once
)

func hlsDir(key string) string { return filepath.Join(cacheDir, "hls", key) }

// hlsArgs 生成 ffmpeg 的 HLS 输出参数。swVideo=true 表示视频强制 CPU 编码。
func hlsArgs(full, dir, vcodec, acodec string, swVideo bool) []string {
	a := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-fflags", "+genpts",
		"-i", full, "-map", "0:v:0", "-map", "0:a:0?"}
	switch {
	case vcodec == "h264":
		a = append(a, "-c:v", "copy")
	case swVideo:
		a = append(a, "-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p")
	default:
		a = append(a, "-c:v", "h264_nvenc", "-preset", "p4", "-cq", "23", "-pix_fmt", "yuv420p")
	}
	if acodec == "aac" {
		a = append(a, "-c:a", "copy")
	} else {
		a = append(a, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
	}
	// event 型 playlist：只增不减，转出来的部分都能拖；temp_file 保证分片写完才可见
	a = append(a, "-sn", "-dn",
		"-f", "hls",
		"-hls_time", "4",
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_segment_filename", filepath.Join(dir, "seg%05d.ts"),
		filepath.Join(dir, "index.m3u8"))
	return a
}

// hlsStartJob 拉起一个 ffmpeg(后台常驻，janitor 负责闲置回收)。
// 必须在持有 hlsMu 时调用。
func hlsStartJob(key, full, ffm, v, a string, sw bool) *hlsJob {
	dir := hlsDir(key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[HLS] 建目录失败 %s: %v", dir, err)
		return &hlsJob{key: key, dir: dir, exited: closedChan(), last: time.Now(), dead: true}
	}
	cmd := exec.Command(ffm, hlsArgs(full, dir, v, a, sw)...)
	j := &hlsJob{key: key, dir: dir, cmd: cmd, exited: make(chan struct{}), last: time.Now(), sw: sw}
	if err := cmd.Start(); err != nil {
		log.Printf("[HLS] 启动失败 name=%s err=%v", filepath.Base(full), err)
		j.dead = true
		close(j.exited)
		return j
	}
	how := "copy"
	switch {
	case v == "h264":
		how = "copy"
	case sw:
		how = "转码(h264/x264)"
	default:
		how = "转码(h264/nvenc)"
	}
	log.Printf("[HLS] 开始 name=%s 视频=%s 音频=%s 方式=%s", filepath.Base(full), v, a, how)
	go hlsWatch(j, full, ffm, v, a)
	return j
}

func closedChan() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// hlsWatch 等 ffmpeg 退出；显卡编码一个分片都没出就换 CPU 重试一次。
func hlsWatch(j *hlsJob, full, ffm, v, a string) {
	err := j.cmd.Wait()
	close(j.exited)
	name := filepath.Base(full)
	segs, _ := filepath.Glob(filepath.Join(j.dir, "seg*.ts"))
	if len(segs) == 0 && !j.sw && v != "h264" {
		hlsMu.Lock()
		stillWanted := time.Since(j.last) < time.Minute && !j.dead
		if stillWanted {
			log.Printf("[HLS] 显卡编码没出片，改用 CPU 重试 name=%s", name)
			hlsJobs[j.key] = hlsStartJob(j.key, full, ffm, v, a, true)
			hlsMu.Unlock()
			return
		}
		j.dead = true
		hlsMu.Unlock()
		log.Printf("[HLS] 显卡编码没出片且已无人等待，放弃 name=%s", name)
		return
	}
	if err != nil && len(segs) == 0 {
		hlsMu.Lock()
		j.dead = true
		hlsMu.Unlock()
	}
	log.Printf("[HLS] 结束 name=%s 分片=%d err=%v", name, len(segs), err)
}

var (
	hlsSegURI  = regexp.MustCompile(`(?m)^([^#\r\n][^\r\n]*\.ts)[ \t]*$`)
	hlsSegName = regexp.MustCompile(`^seg\d{5}\.ts$`)
	hlsKeyRe   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// hlsRewrite 把 playlist 里的裸分片名改写成带鉴权上下文的分段接口地址。
func hlsRewrite(b []byte, key string) []byte {
	return hlsSegURI.ReplaceAll(b, []byte("/api/fs/hls/seg?k="+key+"&f=$1"))
}

// handleHLSPlaylist /api/fs/hls.m3u8?root=&path=
// 拉起(或复用)转码任务，等 playlist 出现后改写分片地址返回。
func handleHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	full, _, err := resolveFor(webUserOf(r), qp.Get("root"), qp.Get("path"))
	if err != nil {
		writeJSON(w, 403, "无权访问该路径", nil)
		return
	}
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		writeJSON(w, 404, "文件不存在", nil)
		return
	}
	ffm, _ := findFF()
	if ffm == "" {
		writeJSON(w, 503, "服务器上没有 ffmpeg，没法转码播放", nil)
		return
	}
	hlsJanitor.Do(func() { go hlsJanitorLoop() })
	key := cacheKey(full, st, "hls")
	hlsMu.Lock()
	j := hlsJobs[key]
	if j == nil {
		v, a := probeCodecs(full)
		j = hlsStartJob(key, full, ffm, v, a, false)
		hlsJobs[key] = j
	}
	j.last = time.Now()
	dir, dead := j.dir, j.dead
	hlsMu.Unlock()
	if dead {
		writeJSON(w, 502, "这个文件的转码启动失败，稍后再试", nil)
		return
	}
	// 冷启动要几秒(4K HEVC 软解偏慢)，最多等 12 秒
	deadline := time.Now().Add(12 * time.Second)
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "index.m3u8")); err == nil && len(b) > 0 {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(hlsRewrite(b, key))
			return
		}
		hlsMu.Lock()
		j.last = time.Now()
		gone := j.dead
		hlsMu.Unlock()
		if gone {
			writeJSON(w, 502, "这个文件的转码启动失败，稍后再试", nil)
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	http.Error(w, "转码启动超时，请稍后重试", http.StatusServiceUnavailable)
}

// handleHLSSeg /api/fs/hls/seg?k=&f=  —— 吐 ts 分片；分片正在生成时短暂等待。
func handleHLSSeg(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	key, name := qp.Get("k"), qp.Get("f")
	if !hlsKeyRe.MatchString(key) || !hlsSegName.MatchString(name) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	hlsMu.Lock()
	j := hlsJobs[key]
	if j != nil {
		j.last = time.Now()
	}
	hlsMu.Unlock()
	if j == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	p := filepath.Join(j.dir, name)
	deadline := time.Now().Add(6 * time.Second)
	for {
		if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
			w.Header().Set("Content-Type", "video/MP2T")
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFile(w, r, p)
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	http.Error(w, "not ready", http.StatusNotFound)
}

// hlsJanitorLoop 闲置回收：4 分钟没人访问就杀 ffmpeg、删分片目录。
func hlsJanitorLoop() {
	for {
		time.Sleep(30 * time.Second)
		hlsMu.Lock()
		now := time.Now()
		for k, j := range hlsJobs {
			if now.Sub(j.last) < 4*time.Minute {
				continue
			}
			if j.cmd != nil && j.cmd.Process != nil {
				_ = j.cmd.Process.Kill()
			}
			_ = os.RemoveAll(j.dir)
			delete(hlsJobs, k)
			log.Printf("[HLS] 闲置清理 key=%s", k[:8])
		}
		hlsMu.Unlock()
	}
}
