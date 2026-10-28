package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MKV / AVI / TS 这些容器 iOS 的 <video> 直接播不了（Safari 只认 MP4/MOV 系的容器）。
// 但里面的编码往往是 iPhone 硬件能解的（HEVC / H.264 + AAC）——
// 所以不需要转码，只要**换个包装**：-c:v copy 把码流原样搬进 MP4。
// 4K 的片子也是这个道理，CPU 几乎不费，几毫秒就能开始出流。

// 「浏览器播不了，但换封装就能播」的容器
var remuxExt = map[string]bool{
	".mkv": true, ".avi": true, ".ts": true, ".m2ts": true, ".mts": true,
	".flv": true, ".wmv": true, ".rmvb": true, ".rm": true, ".divx": true,
	".f4v": true, ".ogv": true, ".vob": true, ".mpg": true, ".mpeg": true,
}

func needRemux(name string) bool {
	return remuxExt[strings.ToLower(filepath.Ext(name))]
}

var (
	ffMu           sync.Mutex
	ffScanned      bool
	ffForceMissing bool // -no-ffmpeg 测试钩子：假装没装，验证引导安装流程
	ffmpegBin      string
	ffprobeBin     string
	transcoding    int32 // 正在转的个数，用于日志和限流
)

// ffInvalidate 让下一次 findFF 重新探测。
// ffmpeg 引导安装完成后调用：新装的 exe 在磁盘上了，但结果还缓存在旧探测里。
func ffInvalidate() {
	ffMu.Lock()
	ffScanned = false
	ffMu.Unlock()
}

// findFF 找 ffmpeg。顺序：配置里写死的 → exe 旁边 → 各盘符下的已知位置 → PATH。
//
// 为什么把 Jellyfin 那份排前面：显卡是 RTX 4070，但驱动 591.86 只到 nvenc API 13.0，
// 而 ffmpeg 9.x 的 nvenc 要求 13.1（驱动 ≥610）。9.0.2 因此**编不了码**，
// 只能用来 copy。Jellyfin 自带 7.0.2，对应 13.0，nvenc 实测 4K 转 H.264 有 92fps。
// 等哪天驱动升到 610+，把配置里的 ffmpeg 指回 9.x 就行。
//
// 已知位置不写死盘符：双系统/加减分区都会让同一个卷换盘符，
// 写死 E:\ 在另一个系统里就是「找不到 ffmpeg」（2026-10-27 踩过：MKV 全部打不开）。
// 所以逐个现存盘符去探测同样的相对位置。
func findFF() (string, string) {
	ffMu.Lock()
	defer ffMu.Unlock()
	if ffScanned {
		return ffmpegBin, ffprobeBin
	}
	ffScanned = true
	var cands []string
	if cfg.FFmpeg != "" {
		cands = append(cands, cfg.FFmpeg)
	}
	// 测试钩子：-no-ffmpeg 假装没装，用来验证引导安装流程
	if ffForceMissing {
		ffmpegBin, ffprobeBin = "", ""
		log.Printf("[转封装] -no-ffmpeg 生效，假装没有 ffmpeg")
		return "", ""
	}
	// exe 旁边放一份就能用：便携部署的标准位置，跟盘符、系统都无关
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "ffmpeg.exe"))
	}
	relCands := []string{
		`TD_WinNAS\Jellyfin\Server\ffmpeg.exe`,
		`ffmpeg_portable\ffmpeg-9.0.2-essentials_build\bin\ffmpeg.exe`,
		`TD_WinNAS\ffmpeg.exe`,
		`ffmpeg\bin\ffmpeg.exe`,
		`ffmpeg.exe`,
	}
	for _, d := range listDrives() {
		for _, rel := range relCands {
			cands = append(cands, filepath.Join(d.Path, rel))
		}
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			ffmpegBin = c
			p := filepath.Join(filepath.Dir(c), "ffprobe.exe")
			if st2, err2 := os.Stat(p); err2 == nil && !st2.IsDir() {
				ffprobeBin = p
			}
			break
		}
	}
	if ffmpegBin == "" {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegBin = p
			if pp, err2 := exec.LookPath("ffprobe"); err2 == nil {
				ffprobeBin = pp
			}
		}
	}
	if ffmpegBin != "" {
		log.Printf("[转封装] ffmpeg=%s ffprobe=%s", ffmpegBin, ffprobeBin)
	} else {
		log.Printf("[转封装] 没找到 ffmpeg，MKV 这类文件将无法在线播放")
	}
	return ffmpegBin, ffprobeBin
}

// probeCodecs 取第一条视频流和第一条音频流的编码名。
// 只为了决定「能不能 copy」——探不出来就返回空，调用方按「需要转」处理。
func probeCodecs(file string) (vcodec, acodec string) {
	_, probe := findFF()
	if probe == "" {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, probe,
		"-v", "error",
		"-show_entries", "stream=codec_type,codec_name",
		"-of", "csv=p=0", file).Output()
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Split(strings.TrimSpace(line), ",")
		if len(parts) < 2 {
			continue
		}
		// csv=p=0 的字段顺序是「按 stream 结构体里的顺序」，实测是 codec_name,codec_type
		// （hevc,video），不是文档直觉上的 type 在前。所以按内容认，别按位置认。
		typ, codec := "", ""
		for _, p := range parts {
			switch p {
			case "video", "audio":
				typ = p
			default:
				if codec == "" {
					codec = p
				}
			}
		}
		switch typ {
		case "video":
			if vcodec == "" {
				vcodec = codec
			}
		case "audio":
			if acodec == "" {
				acodec = codec
			}
		}
	}
	return vcodec, acodec
}

// stderrTail 只留最后几 KB 的错误输出：ffmpeg 的报错够用就行，多了占内存
type stderrTail struct {
	buf []byte
	max int
}

func (s *stderrTail) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	if len(s.buf) > s.max {
		s.buf = s.buf[len(s.buf)-s.max:]
	}
	return len(p), nil
}

func (s *stderrTail) String() string {
	return strings.TrimSpace(string(s.buf))
}

// handleFSPlay 把 MKV/AVI/TS… 实时换封装成 MP4 流喂给 <video>。
//
// 为什么不做 Range：fragmented MP4 是为「边下边播」设计的，没有文件末尾的索引，
// 所以这个流只能从头放、不能拖动。要拖动就得等转码完落盘（那是另一套东西）。
// 前端因此会提示「暂不支持拖动」，而不是让用户拖半天没反应。
func handleFSPlay(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	full, _, err := resolve(q.Get("root"), q.Get("path"))
	if err != nil {
		log.Printf("[转封装] %s %s 拒绝：路径越界 path=%q", clientIP(r), uaTag(r.UserAgent()), q.Get("path"))
		http.Error(w, "禁止访问", http.StatusForbidden)
		return
	}
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}
	ffm, _ := findFF()
	if ffm == "" {
		http.Error(w, "服务器上没有 ffmpeg，没法在线换封装", http.StatusNotImplemented)
		return
	}

	ip := clientIP(r)
	ua := uaTag(r.UserAgent())
	name := filepath.Base(full)

	vcodec, acodec := probeCodecs(full)
	// 客户端自己说「我解不了 HEVC」时传 vc=h264 —— 浏览器能力差异很大：
	// Safari/iPhone 能硬解 HEVC，但 Windows 上的 Chrome 没装 HEVC 扩展就完全不能解，
	// 表现为「有声音、黑屏、videoWidth=0」。那种情况只能真转码。
	wantH264 := q.Get("vc") == "h264"

	// buildArgs 生成一次 ffmpeg 的参数。swVideo=true 表示视频强制用 CPU 编码（显卡失败后的退路）。
	buildArgs := func(swVideo bool) ([]string, string) {
		a := []string{"-nostdin", "-loglevel", "error", "-fflags", "+genpts"}
		// t= 秒：从第几秒开始。前端「重新从这儿放」用得上（因为拖不了进度）
		if ts := q.Get("t"); ts != "" {
			if f, e := strconv.ParseFloat(ts, 64); e == nil && f > 0 {
				a = append(a, "-ss", strconv.FormatFloat(f, 'f', 3, 64))
			}
		}
		a = append(a, "-i", full, "-map", "0:v:0", "-map", "0:a:0?")

		how := ""
		switch {
		case vcodec == "h264" && !swVideo:
			a = append(a, "-c:v", "copy")
			how = "copy"
		case vcodec == "hevc" && !wantH264 && !swVideo:
			// Apple 只认 hvc1 这个 tag，MKV 里是 hev1/裸流，不标 Safari 直接黑屏
			a = append(a, "-c:v", "copy", "-tag:v", "hvc1")
			how = "copy(hevc→hvc1)"
		case swVideo:
			a = append(a, "-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p")
			how = "转码(" + vcodec + "→h264/x264)"
		default:
			// 要么编码本身不能进 MP4（VC-1/MPEG-2/AV1/VP9），要么客户端解不了 HEVC。
			// 先用显卡；显卡不行再退 CPU（外面 logic 会重试）
			a = append(a, "-c:v", "h264_nvenc", "-preset", "p4", "-cq", "23", "-pix_fmt", "yuv420p")
			how = "转码(" + vcodec + "→h264/nvenc)"
		}

		if acodec == "aac" {
			a = append(a, "-c:a", "copy")
		} else {
			// AC3 / DTS / EAC3 / FLAC 这些 Safari 一律不认，统一转 AAC；立体声够手机外放
			a = append(a, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
		}
		a = append(a, "-sn", "-dn",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-f", "mp4", "pipe:1")
		return a, how
	}

	// streamOnce 跑一次 ffmpeg 并把输出转发给浏览器。返回发出的字节数和是否已经回过 200。
	streamOnce := func(swVideo bool) (int64, bool) {
		args, vHow := buildArgs(swVideo)
		ctx := r.Context()
		cmd := exec.CommandContext(ctx, ffm, args...)
		stdout, perr := cmd.StdoutPipe()
		if perr != nil {
			return 0, false
		}
		se := &stderrTail{max: 4096}
		cmd.Stderr = se
		t0 := time.Now()
		if err := cmd.Start(); err != nil {
			log.Printf("[转封装] %s %s 启动失败 name=%s err=%v", ip, ua, name, err)
			return 0, false
		}
		n := atomic.AddInt32(&transcoding, 1)
		defer atomic.AddInt32(&transcoding, -1)
		// 客户端断开（用户退出播放页）时把 ffmpeg 杀掉，别让它在后台空跑
		defer func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		}()

		// 先读到第一块再回 200：如果这个文件根本解不了，还能给浏览器一个明确的错误，
		// 而不是「200 然后视频永远转圈」。
		buf := make([]byte, 64*1024)
		var total int64
		first := true
		for {
			rn, rerr := stdout.Read(buf)
			if rn > 0 {
				if first {
					first = false
					w.Header().Set("Content-Type", "video/mp4")
					w.Header().Set("Cache-Control", "no-store")
					w.Header().Set("Accept-Ranges", "none")
					w.WriteHeader(http.StatusOK)
					log.Printf("[转封装] %s %s 开始 name=%s 视频=%s 音频=%s 方式=%s 在转=%d",
						ip, ua, name, vcodec, acodec, vHow, n)
				}
				if _, werr := w.Write(buf[:rn]); werr != nil {
					break
				}
				total += int64(rn)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			if rerr != nil {
				break
			}
		}
		used := time.Since(t0).Seconds()
		if total == 0 {
			log.Printf("[转封装] %s %s 失败 name=%s 视频=%s 音频=%s 方式=%s 用时=%.1fs err=%s",
				ip, ua, name, vcodec, acodec, vHow, used, se.String())
		} else {
			log.Printf("[转封装] %s %s 结束 name=%s 方式=%s 发出=%.1fMB 用时=%.1fs err=%s",
				ip, ua, name, vHow, float64(total)/1048576, used, se.String())
		}
		return total, !first
	}

	needTranscode := !(vcodec == "h264" || (vcodec == "hevc" && !wantH264))
	total, wroteHeader := streamOnce(false)
	// 显卡编码失败（驱动/显存/不支持）时退到 CPU 再来一次。
	// 前面读不到任何字节就不会写过响应头，所以这里还能安全地重试。
	if total == 0 && !wroteHeader && needTranscode {
		log.Printf("[转封装] %s %s 显卡编码没成功，改用 CPU 重试 name=%s", ip, ua, name)
		total, wroteHeader = streamOnce(true)
	}
	if total == 0 && !wroteHeader {
		http.Error(w, "这个文件解不开，可能不是有效的视频", http.StatusUnsupportedMediaType)
	}
}

// handleFSPlayInfo 让前端先问一句「这个文件能不能播、要不要换封装」。
// 前端据此决定：直链播放 / 换封装播放 / 只给下载。
func handleFSPlayInfo(w http.ResponseWriter, r *http.Request) {
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
	ffm, _ := findFF()
	out := map[string]any{
		"name":       name,
		"size":       st.Size(),
		"remux":      needRemux(name),
		"ffmpeg":     ffm != "",
		"playable":   !needRemux(name),
		"unplayable": needRemux(name) && ffm == "",
	}
	if needRemux(name) && ffm != "" {
		v, a := probeCodecs(full)
		out["vcodec"] = v
		out["acodec"] = a
		// 视频是 H.264/HEVC 才能只换壳；其它得靠显卡转，4K 转码会吃满 GPU，
		// 前端据此给一句「可能要等一下」的提示
		out["copyVideo"] = v == "h264" || v == "hevc"
	}
	writeJSON(w, 0, "", out)
}
