package main

// 音频标签：让文件站列表显示「歌名 · 歌手」而不是一堆 01.mp3，播放器拿去做锁屏信息。
//
// 为什么不塞进 /api/fs/list：一个音乐文件夹几百个文件，逐个解 ID3 会把列表拖到
// 天荒地老 —— 跟 thumb.go 是同一个教训。做法是列表渲完再单独拉一次本目录的标签，
// 服务端按「绝对路径 + mtime + size」缓存，同一个文件只解一次，文件换了自动失效
// （跟 cache/thumbs 的 key 约定一致）。

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/dhowden/tag"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
)

type trackTag struct {
	Name        string `json:"name"` // 文件名，前端按它对上列表里的那一行
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	Album       string `json:"album"`
	AlbumArtist string `json:"albumArtist"`
	Track       int    `json:"track"`
}

type tagEntry struct {
	mtime int64
	size  int64
	t     trackTag
}

var (
	tagMu    sync.Mutex
	tagCache = map[string]tagEntry{}
)

// 老 MP3 的 ID3v1、以及不少国内下载站写进去的 ID3v2 都是 GBK，直接当 UTF-8 读是乱码。
// 先认 UTF-8，不合法再试 GBK / GB18030；都失败原样返回 —— 显示乱码也好过整行空白。
func fixTagEnc(s string) string {
	s = strings.TrimSpace(strings.Trim(s, "\x00"))
	if s == "" {
		return s
	}
	// 第一坑：ID3v2.3 的 encoding=0 是 ISO-8859-1，dhowden/tag 老老实实按它解，
	// GBK 字节就变成「Ò¹Çú」这种**合法 UTF-8** 的拉丁字符 —— 后面那道
	// utf8.ValidString 拦不住。所以先把拉丁字符还原成原始字节，再按 GBK 试。
	if d, ok := latin1ToGBK(s); ok {
		return d
	}
	if utf8.ValidString(s) {
		return s
	}
	// 第二坑：ID3v2.4 / 部分 v1 标签直接给 GBK 原始字节，上面那关过不了这里兜
	if d, err := simplifiedchinese.GBK.NewDecoder().String(s); err == nil && utf8.ValidString(d) {
		return strings.TrimSpace(d)
	}
	if d, err := simplifiedchinese.GB18030.NewDecoder().String(s); err == nil {
		return strings.TrimSpace(d)
	}
	return s
}

// 把「每个字符都落在 U+0000–U+00FF」的字符串还原成字节，再按 GBK / GB18030 解。
// 只在解出来**确实含汉字**时才采纳：否则西欧歌名（Café）会被误转成一堆方块。
func latin1ToGBK(s string) (string, bool) {
	raw := make([]byte, 0, len(s))
	high := false
	for _, r := range s {
		if r > 0xff {
			return "", false // 有真正的非拉丁字符（比如正常中文），不是这个坑
		}
		if r >= 0x80 {
			high = true
		}
		raw = append(raw, byte(r))
	}
	if !high {
		return "", false // 纯 ASCII 不用折腾
	}
	for _, dec := range []*encoding.Decoder{
		simplifiedchinese.GBK.NewDecoder(),
		simplifiedchinese.GB18030.NewDecoder(),
	} {
		d, err := dec.Bytes(raw)
		if err != nil || !utf8.Valid(d) {
			continue
		}
		if hasHan(string(d)) {
			return strings.TrimSpace(string(d)), true
		}
	}
	return "", false
}

func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// 「歌手 - 歌名」是下载来的文件最常见的命名法：没标签时靠它兜底，比直接显示
// 带序号前缀的文件名好看得多。分隔符按常见程度从长到短试，避免把歌名里的
// 连字符误当分隔（所以 "-" 放最后）。
func splitByName(base string) (artist, title string) {
	for _, sep := range []string{" - ", " – ", " — ", "－"} {
		if i := strings.Index(base, sep); i > 0 {
			return strings.TrimSpace(base[:i]), strings.TrimSpace(base[i+len(sep):])
		}
	}
	return "", base
}

func readTrackTag(abs, name string, mtime, size int64) trackTag {
	tagMu.Lock()
	if e, ok := tagCache[abs]; ok && e.mtime == mtime && e.size == size {
		tagMu.Unlock()
		return e.t
	}
	tagMu.Unlock()

	base := strings.TrimSuffix(name, filepath.Ext(name))
	out := trackTag{Name: name}

	if f, err := os.Open(abs); err == nil {
		// tag.ReadFrom 支持 mp3(ID3v1/v2) / m4a / flac / ogg / dsf；
		// wav、ape 之类它认不出来，走下面的文件名兜底，不算错误。
		if m, err := tag.ReadFrom(f); err == nil {
			out.Title = fixTagEnc(m.Title())
			out.Artist = fixTagEnc(m.Artist())
			out.Album = fixTagEnc(m.Album())
			out.AlbumArtist = fixTagEnc(m.AlbumArtist())
			out.Track, _ = m.Track()
		}
		_ = f.Close()
	}

	if out.Title == "" || out.Artist == "" {
		a, t := splitByName(base)
		if out.Artist == "" {
			out.Artist = a
		}
		if out.Title == "" {
			out.Title = t
		}
	}
	if out.Title == "" {
		out.Title = base
	}
	if out.AlbumArtist == "" {
		out.AlbumArtist = out.Artist
	}

	tagMu.Lock()
	// 个人库量级（几千首）不会撑爆；真到 2 万条就整体清一次，比做 LRU 划算
	if len(tagCache) > 20000 {
		tagCache = map[string]tagEntry{}
	}
	tagCache[abs] = tagEntry{mtime: mtime, size: size, t: out}
	tagMu.Unlock()
	return out
}

// GET /api/fs/tags?root=&path=<目录相对路径>
// 返回该目录下所有音频文件的标签（不递归）。目录里没有音频就给空数组，不算错。
func handleFSTags(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("root")
	rel := r.URL.Query().Get("path")
	full, _, err := resolveFor(webUserOf(r), root, rel)
	if err != nil {
		writeJSON(w, 403, "无权访问该路径", nil)
		return
	}
	if st, err := os.Stat(full); err != nil || !st.IsDir() {
		writeJSON(w, 404, "目录不存在", nil)
		return
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		writeJSON(w, 500, "读取失败: "+err.Error(), nil)
		return
	}
	out := make([]trackTag, 0, 8)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if fileKind(name, false) != "audio" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, readTrackTag(
			filepath.Join(full, name), name,
			info.ModTime().Unix(), info.Size(),
		))
	}
	writeJSON(w, 0, "", out)
}
