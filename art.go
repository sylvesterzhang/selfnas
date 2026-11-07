package main

// 播放页要的两样东西：**封面图**和**歌词**。
//
// 为什么单独开两个接口，而不是并进 /api/fs/tags：
// 一个目录几十首歌，标签是一次性小字符串（几百字节），封面动辄上百 KB，
// 全部塞进列表响应就是灾难。跟 thumb.go 一个道理 —— 列表只标「有没有」，
// 真正的图等播到这首再去拿，一首一张，谁也不吃亏。
//
// 歌词同理：没人一口气看完整张专辑的歌词。
//
// 缓存沿用 thumb.go 的约定：cache/covers/<key[:2]>/<key>.jpg，key 含修改时间。

import (
	"bytes"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/dhowden/tag"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
)

const coverMaxSide = 640 // 播放页最大显示到 260px 圆盘，两倍屏 520，640 有富余

// MediaSession artwork 允许请求的边长。MediaMetadata 官方建议给一组尺寸让系统挑
// （Android Chrome 目标 512×512，低端机 256×256，iOS 挑最大的）。
// ⚠️ w 不在签名里 —— /f 的签名只绑「路径+大小+过期时间」，w 纯粹是出图参数。
// 所以必须白名单：不限制的话有人塞 w=100000 就能让服务端放大图、耗 CPU 和磁盘。
var coverSizes = map[int]bool{96: true, 128: true, 192: true, 256: true, 384: true, 512: true, 640: true}

func coverSide(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("w"))
	if err != nil || !coverSizes[n] {
		return coverMaxSide
	}
	return n
}

func coverPath(key string) string {
	return filepath.Join(cacheDir, "covers", key[:2], key+".jpg")
}

// GET /api/fs/cover?root=&path=<音频相对路径>
func handleFSCover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "只支持 GET", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	full, _, err := resolveFor(webUserOf(r), q.Get("root"), q.Get("path"))
	if err != nil {
		http.Error(w, "禁止访问", http.StatusForbidden)
		return
	}
	serveCoverFull(w, r, full, false)
}

// serveCoverFull 只认「已经验过权的绝对路径」，自己不做任何鉴权判断。
// 两个入口共用：网页里的 /api/fs/cover（带登录 cookie），以及 iOS 锁屏 artwork
// 用的签名直链 /f?...&cover=1 —— 系统媒体服务抓图不带网页 cookie，只能靠签名。
//
// placeholder=true 时，歌里既没内嵌封面、目录里也没有 cover.jpg，就回一张通用
// 专辑图，而不是 404：锁屏拿到 404 会退回站点图标，正是这次要消掉的现象。
// 网页播放页仍走 false，404 让它保留原来的「无封面黑胶」样式，不改桌面观感。
// 可选 w=<边长> 给 MediaSession artwork 出多尺寸图，取值走 coverSizes 白名单；
// 网页不带 w，默认仍是原来的 640。
func serveCoverFull(w http.ResponseWriter, r *http.Request, full string, placeholder bool) {
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}
	if cacheDir == "" {
		if placeholder {
			serveDefaultCover(w, r) // 缓存目录没就绪也不让锁屏掉回站点图标
			return
		}
		http.Error(w, "缓存目录不可用", http.StatusInternalServerError)
		return
	}

	// 尺寸进缓存 key：96 / 512 / 640 各存一份，互不覆盖；含 mtime → 内容不可变可长缓存
	side := coverSide(r)
	key := cacheKey(full, st, "cover"+strconv.Itoa(side))
	target := coverPath(key)
	if fi, err := os.Stat(target); err == nil && fi.Size() > 0 {
		serveThumb(w, r, target) // key 含 mtime → 内容不可变，可以直接缓存一年
		return
	}

	data := readCoverBytes(full)
	if data == nil {
		// 没有内嵌封面就看目录里有没有 cover/folder.jpg —— 整轨抓的专辑、老 MP3 常见
		data = readDirCover(filepath.Dir(full))
	}
	if data == nil {
		if placeholder {
			serveDefaultCover(w, r)
			return
		}
		http.Error(w, "没有封面", http.StatusNotFound)
		return
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		http.Error(w, "缓存目录不可用", http.StatusInternalServerError)
		return
	}
	// 缩放失败（图解码不了）就把原图直接落盘 —— 显示优先级高于体积
	if err := writeCoverJPEG(data, target, side); err != nil {
		if err := os.WriteFile(target, data, 0o644); err != nil {
			http.Error(w, "写缓存失败", http.StatusInternalServerError)
			return
		}
	}
	serveThumb(w, r, target)
}

// serveDefaultCover 回内嵌的通用专辑图。走 embed 而不是磁盘文件：既不用登录，
// 也不依赖 cacheDir，部署时少一个可能漏放的资源。
// 无封面的歌也会走这里，所以同样按 w 缩到对应边长，别让声明 96x96 的 artwork
// 实际吐一张 640 的图（系统媒体服务按 sizes 挑图，尺寸对不上会白占带宽）。
var coverDefaultCache sync.Map // side(int) -> []byte，进程内的小缓存，最多 7 个尺寸

func serveDefaultCover(w http.ResponseWriter, r *http.Request) {
	data, err := webFS.ReadFile("web/cover-default.jpg")
	if err != nil {
		http.Error(w, "没有封面", http.StatusNotFound)
		return
	}
	side := coverSide(r)
	if side != coverMaxSide {
		if v, ok := coverDefaultCache.Load(side); ok {
			writeCoverBytes(w, r, v.([]byte))
			return
		}
		if src, e := jpeg.Decode(bytes.NewReader(data)); e == nil {
			var buf bytes.Buffer
			if jpeg.Encode(&buf, boxResize(src, side), &jpeg.Options{Quality: 86}) == nil {
				b := append([]byte(nil), buf.Bytes()...)
				coverDefaultCache.Store(side, b)
				writeCoverBytes(w, r, b)
				return
			}
		}
	}
	writeCoverBytes(w, r, data)
}

func writeCoverBytes(w http.ResponseWriter, r *http.Request, data []byte) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

// readCoverBytes 从音频文件里抠内嵌封面。flac/m4a/mp3 都有，wav 基本没有。
func readCoverBytes(full string) []byte {
	f, err := os.Open(full)
	if err != nil {
		return nil
	}
	defer f.Close()
	m, err := tag.ReadFrom(f)
	if err != nil {
		return nil
	}
	p := m.Picture()
	if p == nil || len(p.Data) == 0 {
		return nil
	}
	return p.Data
}

// readDirCover 目录里的封面文件。按常见程度排，命中第一个就走。
func readDirCover(dir string) []byte {
	for _, n := range []string{"cover.jpg", "folder.jpg", "album.jpg", "front.jpg",
		"cover.jpeg", "folder.jpeg", "cover.png", "folder.png"} {
		p := filepath.Join(dir, n)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Size() < 20<<20 {
			if b, err := os.ReadFile(p); err == nil {
				return b
			}
		}
	}
	return nil
}

// writeCoverJPEG 解码 → 缩到 side → 存 JPEG。解不开就返回错误，让调用方原样落盘。
func writeCoverJPEG(data []byte, target string, side int) error {
	var src image.Image
	var err error
	if src, err = jpeg.Decode(bytes.NewReader(data)); err != nil {
		src, err = png.Decode(bytes.NewReader(data))
		if err != nil {
			return err
		}
	}
	dst := boxResize(src, side)
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	// 播放页封面是视觉主体，质量给到 86（列表缩略图是 78）
	if err := jpeg.Encode(out, dst, &jpeg.Options{Quality: 86}); err != nil {
		out.Close()
		os.Remove(target)
		return err
	}
	return out.Close()
}

// ---------- 歌词 ----------

// GET /api/fs/lyrics?root=&path=<音频相对路径>
// 先找同名 .lrc（下载来的歌大多自带），没有再读内嵌 USLT/LYRICS。
// 都没有就返回空字符串 —— 前端显示「暂无歌词」，不算错误。
func handleFSLyrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "只支持 GET", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	root, rel := q.Get("root"), q.Get("path")
	full, _, err := resolveFor(webUserOf(r), root, rel)
	if err != nil {
		http.Error(w, "禁止访问", http.StatusForbidden)
		return
	}
	if st, err := os.Stat(full); err != nil || st.IsDir() {
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}

	src := ""
	text := readSidecarLRC(full)
	// 占位歌词：网易云等下载器对「没拿到版权歌词」的歌会写一个
	// `[00:00.00]暂无歌词` 的 22 字节 .lrc。这种文件按「有歌词」返回的话，
	// 播放页会把它当成唯一一句歌词显示（看着像歌词丢了），所以当它不存在。
	if text != "" && lyricIsPlaceholder(text) {
		text = ""
	}
	if text != "" {
		src = "lrc"
	} else if text = readTagLyrics(full); text != "" {
		src = "tag"
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 歌词会跟着 .lrc 变，不做长缓存，但允许浏览器短时间复用（一首歌反复播很常见）
	w.Header().Set("Cache-Control", "private, max-age=60")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": 0, "msg": "",
		"data": map[string]string{"lrc": text, "src": src},
	})
}

// readSidecarLRC 找同目录下的同名 .lrc。大小写不敏感 —— 手机上同步过来的
// 文件经常是 `.LRC`，而 Linux 上这两个名字不是一回事。
func readSidecarLRC(full string) string {
	dir := filepath.Dir(full)
	base := strings.TrimSuffix(filepath.Base(full), filepath.Ext(full))
	want := strings.ToLower(base) + ".lrc"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(e.Name()) != want {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || len(b) > 512<<10 {
			continue
		}
		return decodeLyricText(b)
	}
	return ""
}

// lyricIsPlaceholder 判断这份歌词是不是下载器塞的占位符。
// 判据：剥掉时间标签和 [ti:][ar:] 这类元信息后，正文只有一句、且是
// 「暂无歌词 / 无歌词 / 纯音乐」这几种说法 —— 真歌词不会只有这样一行。
func lyricIsPlaceholder(text string) bool {
	n := 0
	var body strings.Builder
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.ReplaceAll(line, "\r", ""))
		// 去掉行首连着的多个 [mm:ss.xx]
		for strings.HasPrefix(line, "[") {
			if i := strings.Index(line, "]"); i > 0 {
				line = strings.TrimSpace(line[i+1:])
			} else {
				break
			}
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "{") {
			// 网易云 JSON 行格式：{"t":500,"c":[{"tx":"暂无歌词"}]}
			var o struct {
				C []struct{ Tx string } `json:"c"`
			}
			if err := json.Unmarshal([]byte(line), &o); err == nil {
				for _, c := range o.C {
					if t := strings.TrimSpace(c.Tx); t != "" {
						n++
						body.WriteString(t)
					}
				}
				continue
			}
		}
		n++
		body.WriteString(line)
	}
	if n == 0 || n > 1 {
		return false
	}
	s := strings.TrimSpace(body.String())
	for _, kw := range []string{"暂无歌词", "无歌词", "暂无", "纯音乐", "歌词加载失败"} {
		if s == kw {
			return true
		}
	}
	return false
}

// readTagLyrics 读音频文件内嵌的歌词（ID3 的 USLT / Vorbis 的 LYRICS）。
func readTagLyrics(full string) string {
	f, err := os.Open(full)
	if err != nil {
		return ""
	}
	defer f.Close()
	m, err := tag.ReadFrom(f)
	if err != nil {
		return ""
	}
	s := m.Lyrics()
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return fixTagEnc(s)
}

// decodeLyricText .lrc 没有编码声明，国内资源以 GBK 为主，也有带 BOM 的 UTF-8。
// 顺序：BOM → 合法 UTF-8 → GB18030。全不成立就按 GB18030 硬解（它是 GBK 超集，
// 解错也只是个别字变成问号，比整份空白强）。
func decodeLyricText(b []byte) string {
	// 去 BOM：留着的话第一行时间是「\ufeff[00:12.00]」，正则匹配不上，第一句歌词就丢了
	if bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) {
		b = b[3:]
	}
	if bytes.HasPrefix(b, []byte{0xFF, 0xFE}) || bytes.HasPrefix(b, []byte{0xFE, 0xFF}) {
		if s, err := unicode.UTF16(unicode.LittleEndian, unicode.ExpectBOM).NewDecoder().Bytes(b); err == nil {
			return string(s)
		}
	}
	if utf8.Valid(b) {
		return string(b) // 合法 UTF-8（含纯 ASCII），直接用
	}
	if s, err := simplifiedchinese.GB18030.NewDecoder().Bytes(b); err == nil {
		return string(s)
	}
	return string(b)
}
