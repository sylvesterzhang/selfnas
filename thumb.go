package main

// 文件列表缩略图：图片直接降采样，视频用 ffmpeg 抓一帧。
//
// 为什么走独立接口而不是把缩略图塞进 list 响应：一个文件夹几十上百张图，
// 全部生成会把 list 请求拖到天荒地老。做法是 list 里只标 thumb=true，
// 前端用 <img loading="lazy"> 滚到哪加载到哪，服务端同时只生成 2 张。
//
// 缓存：cache/thumbs/<key>.jpg，key 含 修改时间 —— 文件被替换自动失效，跟 prep 缓存同一套约定。
// 解码只支持纯 Go 能吃下的格式（JPEG/PNG/GIF/BMP/WEBP/TIFF）；iPhone 的 HEIC 解不了，
// 前端 <img> onerror 会退回图标，不会白屏。

import (
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
	"golang.org/x/image/webp"
)

const thumbMaxSide = 320 // 最长边，列表里显示 40~48px，两倍屏也够用

var (
	thumbMu   sync.Mutex
	thumbJobs = map[string]*cacheJob{} // key -> 生成任务（跟 prep 缓存共用同一把锁/结构）
	thumbSema = make(chan struct{}, 2) // 并发生成上限：缩略图是 IO 活，多了挤占读盘带宽
)

func thumbPath(key string) string {
	return filepath.Join(cacheDir, "thumbs", key[:2], key+".jpg")
}

// thumbableExt 判断这个扩展名能不能出缩略图。视频取决于 ffmpeg 是否在。
func thumbableExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".png", ".gif", ".bmp", ".tif", ".tiff", ".webp":
		return true
	case ".mp4", ".m4v", ".mov", ".webm", ".mkv", ".avi", ".ts", ".m2ts", ".mts",
		".flv", ".wmv", ".rmvb", ".rm", ".mpg", ".mpeg", ".vob", ".3gp", ".f4v":
		ffm, _ := findFF()
		return ffm != ""
	}
	return false
}

// handleFSThumb GET /api/fs/thumb?root=&path= → 320px JPEG
func handleFSThumb(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "只支持 GET", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	full, _, err := resolve(q.Get("root"), q.Get("path"))
	if err != nil {
		http.Error(w, "禁止访问", http.StatusForbidden)
		return
	}
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}
	if !thumbableExt(filepath.Ext(full)) {
		http.Error(w, "该格式不支持缩略图", http.StatusNotFound)
		return
	}
	if cacheDir == "" {
		// 缓存目录不可用时宁可不出图，也不要往进程当前目录里乱写
		http.Error(w, "缓存目录不可用", http.StatusInternalServerError)
		return
	}

	key := cacheKey(full, st, "thumb320")
	target := thumbPath(key)
	if fi, err := os.Stat(target); err == nil && fi.Size() > 0 {
		serveThumb(w, r, target)
		return
	}

	// 同一个缩略图被并发请求时只生成一次，其余等结果
	thumbMu.Lock()
	if j, ok := thumbJobs[key]; ok {
		thumbMu.Unlock()
		<-j.done
		if j.err != "" {
			http.Error(w, j.err, http.StatusInternalServerError)
			return
		}
		serveThumb(w, r, target)
		return
	}
	j := &cacheJob{done: make(chan struct{})}
	thumbJobs[key] = j
	thumbMu.Unlock()

	defer func() {
		thumbMu.Lock()
		delete(thumbJobs, key)
		thumbMu.Unlock()
		close(j.done)
	}()

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		j.err = "缓存目录不可用"
		http.Error(w, j.err, http.StatusInternalServerError)
		return
	}
	// 生成失败的结果不落缓存文件，下次请求重试（比如 ffmpeg 正在忙时超时）
	if err := generateThumb(full, filepath.Ext(full), target); err != nil {
		j.err = err.Error()
		http.Error(w, j.err, http.StatusInternalServerError)
		return
	}
	serveThumb(w, r, target)
}

func serveThumb(w http.ResponseWriter, r *http.Request, path string) {
	// key 含修改时间，内容不可变 → 缓存一年，浏览器不用反复拉
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
}

func generateThumb(full, ext, target string) error {
	thumbSema <- struct{}{} // 同时最多 2 个生成任务
	defer func() { <-thumbSema }()

	ext = strings.ToLower(ext)
	switch ext {
	case ".mp4", ".m4v", ".mov", ".webm", ".mkv", ".avi", ".ts", ".m2ts", ".mts",
		".flv", ".wmv", ".rmvb", ".rm", ".mpg", ".mpeg", ".vob", ".3gp", ".f4v":
		return videoThumb(full, target)
	default:
		return imageThumb(full, target)
	}
}

// videoThumb 用 ffmpeg 抓一帧缩到 320px。-ss 放 -i 前面是关键帧直跳，比解码快得多。
//
// 为什么要抓两次：1~5 秒处经常是片头黑场/台标淡入，直接取 1 秒会得到一块黑方块。
// 先取 8 秒（绝大多数片头的黑屏已经过去），如果出来几乎纯黑（JPEG 压出来极小），
// 再往 45 秒找一次，两张里取大的 —— 大 = 细节多 = 不是黑屏。
func videoThumb(full, target string) error {
	ffm, _ := findFF()
	if ffm == "" {
		return fmt.Errorf("服务器没有 ffmpeg")
	}
	var best string
	var bestSize int64
	for _, ss := range []string{"8", "45"} {
		tmp := fmt.Sprintf("%s.%s.jpg", target, ss)
		if err := grabFrame(ffm, full, ss, tmp); err != nil {
			if best == "" {
				return err
			}
			break
		}
		fi, err := os.Stat(tmp)
		if err != nil {
			os.Remove(tmp)
			continue
		}
		if fi.Size() > bestSize {
			os.Remove(best)
			best, bestSize = tmp, fi.Size()
		} else {
			os.Remove(tmp)
		}
		// 3KB 以下基本就是黑屏/纯色，再往后找一帧；否则直接收工
		if bestSize >= 3000 {
			break
		}
	}
	if best == "" {
		// 片子比 8 秒还短：seek 越界抓不到帧，退到开头再试一次
		tmp := target + ".0.jpg"
		if err := grabFrame(ffm, full, "0", tmp); err != nil {
			return err
		}
		best = tmp
	}
	return os.Rename(best, target)
}

func grabFrame(ffm, full, ss, out string) error {
	args := []string{"-nostdin", "-loglevel", "error", "-y",
		"-ss", ss, "-i", full, "-frames:v", "1",
		"-vf", fmt.Sprintf("scale='min(%d,iw)':-2", thumbMaxSide),
		"-q:v", "4", out}
	cmd := exec.Command(ffm, args...)
	if o, err := cmd.CombinedOutput(); err != nil {
		os.Remove(out)
		return fmt.Errorf("ffmpeg 抓帧失败: %v: %s", err, firstLine(string(o)))
	}
	return nil
}

// imageThumb 解码 → 盒式降采样 → JPEG 落盘。
// 降采样用两趟盒式平均：实现 40 行、零依赖，缩到 320px 的观感和 CatmullRom 没有肉眼差别。
func imageThumb(full, target string) error {
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	defer f.Close()

	var src image.Image
	ext := strings.ToLower(filepath.Ext(full))
	switch ext {
	case ".png":
		src, err = png.Decode(f)
	case ".gif":
		src, err = gif.Decode(f)
	case ".bmp":
		src, err = bmp.Decode(f)
	case ".tif", ".tiff":
		src, err = tiff.Decode(f)
	case ".webp":
		src, err = webp.Decode(f)
	default: // .jpg / .jpeg
		src, err = jpeg.Decode(f)
	}
	if err != nil {
		return fmt.Errorf("解不开这个图片（可能是 HEIC 或损坏文件）: %w", err)
	}
	if src == nil {
		return fmt.Errorf("图片内容为空")
	}

	// GIF 的调色板图要先转成 RGBA，否则缩放时颜色会串
	var sImg image.Image = src
	if _, ok := src.(*image.Paletted); ok {
		rgba := image.NewRGBA(src.Bounds())
		for y := src.Bounds().Min.Y; y < src.Bounds().Max.Y; y++ {
			for x := src.Bounds().Min.X; x < src.Bounds().Max.X; x++ {
				rgba.Set(x, y, src.At(x, y))
			}
		}
		sImg = rgba
	}

	dst := boxResize(sImg, thumbMaxSide)
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	// 大图缩到 320px，质量 78 肉眼无损，单张 10~20KB
	if err := jpeg.Encode(out, dst, &jpeg.Options{Quality: 78}); err != nil {
		out.Close()
		os.Remove(target)
		return err
	}
	return out.Close()
}

// boxResize 盒式降采样（只缩小不放大）：每个目标像素 = 源图上对应矩形块的平均色。
// 源图比目标还小时直接返回原图。块恰好铺满源图，每个源像素只被读一次。
func boxResize(src image.Image, maxSide int) image.Image {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= maxSide && sh <= maxSide {
		return src
	}
	scale := float64(maxSide) / float64(max(sw, sh))
	dw, dh := int(float64(sw)*scale), int(float64(sh)*scale)
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}

	// 中间缓冲：tmp[y][x*4..] 存每个目标像素平均后的 16 位线性 RGBA
	tmp := make([][]float64, dh)
	invX := float64(sw) / float64(dw)
	for y := 0; y < dh; y++ {
		tmp[y] = make([]float64, dw*4)
	}
	srcAt := func(x, y int) (float64, float64, float64, float64) {
		r, g, b, a := src.At(sb.Min.X+x, sb.Min.Y+y).RGBA()
		return float64(r), float64(g), float64(b), float64(a)
	}
	for y := 0; y < dh; y++ {
		sy0 := int(float64(y) * float64(sh) / float64(dh))
		sy1 := int(float64(y+1) * float64(sh) / float64(dh))
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		if sy1 > sh {
			sy1 = sh
		}
		for x := 0; x < dw; x++ {
			sx0 := int(float64(x) * invX)
			sx1 := int(float64(x+1) * invX)
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			if sx1 > sw {
				sx1 = sw
			}
			var r, g, b, a, n float64
			for yy := sy0; yy < sy1; yy++ {
				for xx := sx0; xx < sx1; xx++ {
					pr, pg, pb, pa := srcAt(xx, yy)
					r += pr
					g += pg
					b += pb
					a += pa
					n++
				}
			}
			o := x * 4
			tmp[y][o], tmp[y][o+1], tmp[y][o+2], tmp[y][o+3] = r/n, g/n, b/n, a/n
		}
	}

	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		row := tmp[y]
		i := dst.PixOffset(dst.Rect.Min.X, dst.Rect.Min.Y+y)
		for x := 0; x < dw; x++ {
			o := x * 4
			a := row[o+3] / 65535
			var pr, pg, pb uint8
			if a > 0 {
				// 有透明度的像素做去预乘，避免透明区域渗色
				pr = uint8(clampF(row[o] / row[o+3] * 255))
				pg = uint8(clampF(row[o+1] / row[o+3] * 255))
				pb = uint8(clampF(row[o+2] / row[o+3] * 255))
			}
			dst.Pix[i] = pr
			dst.Pix[i+1] = pg
			dst.Pix[i+2] = pb
			dst.Pix[i+3] = uint8(clampF(row[o+3] / 257))
			i += 4
		}
	}
	return dst
}

func clampF(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}
