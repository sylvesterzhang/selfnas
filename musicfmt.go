package main

// 曲目规格标记（列表上的 Hi-Res / 无损小标）。
//
// 为什么不调 ffprobe：FLAC 的 STREAMINFO 就写在文件头里（采样率 20bit、位深 5bit，
// 从第 18 字节开始），读 64 字节就够，一次列表 300 多首也只是几百次小读。
// ffprobe 每首 20~30ms，全列表要七八秒，不能用。
//
// 为什么不能只看扩展名：这个库里一批 275MB 的「.mp3」其实是 192kHz/24bit 的 FLAC
// （就是之前 App 报 PlaybackFailed 的那批），扩展名会撒谎，一律以文件头为准。

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	muQHires    = "hires"    // 比 CD 高：位深 >16bit 或 采样率 >48kHz
	muQLossless = "lossless" // 无损但只是 CD 规格
)

type mqEntry struct {
	size, mtime int64
	q           string
}

var mqCache sync.Map // key: root\x00path

// musicQualityOf 返回 hires / lossless / 空串（有损或认不出来）。
// 按 (root,path) 缓存，size 或 mtime 变了才重新探。
func musicQualityOf(root, path string, size, mtime int64, dur float64) string {
	key := root + "\x00" + path
	if v, ok := mqCache.Load(key); ok {
		if e := v.(mqEntry); e.size == size && e.mtime == mtime {
			return e.q
		}
	}
	full := filepath.Join(normPath(root), filepath.FromSlash(path))
	q := probeQuality(full, path, size, dur)
	mqCache.Store(key, mqEntry{size, mtime, q})
	return q
}

// probeQuality 先看文件头认容器；认不出再按后缀 + 码率粗判。
func probeQuality(full, path string, size int64, dur float64) string {
	f, err := os.Open(full)
	if err != nil {
		return ""
	}
	var h [64]byte
	n, _ := f.Read(h[:])
	f.Close()

	if n >= 42 && h[0] == 'f' && h[1] == 'L' && h[2] == 'a' && h[3] == 'C' {
		// STREAMINFO：18..21 是 20bit 采样率 + 3bit 声道数 + 5bit 位深-1
		rate := int(h[18])<<12 | int(h[19])<<4 | int(h[20])>>4
		bps := ((int(h[20])&0x01)<<4 | int(h[21])>>4) + 1
		if bps >= 24 || rate > 48000 {
			return muQHires
		}
		return muQLossless
	}
	if n >= 44 && h[0] == 'R' && h[1] == 'I' && h[2] == 'F' && h[3] == 'F' &&
		h[8] == 'W' && h[9] == 'A' && h[10] == 'V' && h[11] == 'E' {
		// WAVE 的 fmt 块：24..27 采样率(LE)，34..35 位深(LE)
		rate := int(h[24]) | int(h[25])<<8 | int(h[26])<<16 | int(h[27])<<24
		bps := int(h[34]) | int(h[35])<<8
		if bps >= 24 || rate > 48000 {
			return muQHires
		}
		return muQLossless
	}
	if n >= 12 && h[0] == 'M' && h[1] == 'A' && h[2] == 'C' && h[3] == ' ' {
		return losslessByBitrate(size, dur) // APE
	}
	if n >= 12 && h[4] == 'f' && h[5] == 't' && h[6] == 'y' && h[7] == 'p' {
		// m4a/alac 看不出有损无损，只能靠码率（ALAC 通常 >1000kbps，AAC 256~320）
		return losslessByBitrate(size, dur)
	}
	// 头没认出来：只有明确是无损后缀才敢标，别把 320k mp3 标成无损
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "flac", "wav", "ape", "dsf", "dff", "aiff", "aif", "alac":
		return losslessByBitrate(size, dur)
	}
	return ""
}

// losslessByBitrate：无损容器里，平均码率超过 CD（1411kbps）就是高解析。
// 对 24bit/44.1kHz 这种（约 1200kbps）会判成「无损」而不是 Hi-Res —— 宁可少标，
// 也不要在列表上把 CD 规格吹成 Hi-Res。
func losslessByBitrate(size int64, dur float64) string {
	if dur <= 0 || size <= 0 {
		return ""
	}
	if float64(size)*8/1000/dur > 1411 {
		return muQHires
	}
	return muQLossless
}
