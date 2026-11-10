package main

// ffmpeg 内置分发：把 ffmpeg 用 gzip 压好嵌进 exe，首次用到时释放到 cache/ 再执行。
//
// 为什么不是「静态链接」：ffmpeg 是独立的可执行程序，不是库文件。Go 没办法把
// 一个 ELF/PE 链进自己的二进制里；真要「链接」得用 cgo 编译全套 libav*，
// 5 个平台交叉编译基本不可行，而且整个程序会被 GPL 传染。embed + 首次释放
// 对用户是同一种体验：装上就能用，不装系统包、不配 PATH、不联网下载。
//
// 体积账：只嵌 ffmpeg、不嵌 ffprobe。ffprobe 唯一的用途是视频转封装前探编码
// （见 probeCodecs），探不到时调用方会保守地走真转码 —— 功能不坏只是慢一点，
// 用 ffmpeg -i 解析 stderr 也能拿到同样的信息。省下来的那 30MB 不值得。
//
// 平台上没有内置包时（比如 macOS 版还没做），ffmpegBundle 为空，
// 这里直接返回空串，findFF 继续走「系统里找 → 引导下载」的老路。

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/klauspost/compress/zstd"
)

// ffBundleStamp 标记当前内置的是哪一版：换了包长度必然变，缓存自然失效。
func ffBundleStamp() string {
	return fmt.Sprintf("%s:%d", ffmpegBundlePlat, len(ffmpegBundle))
}

func ffBundleExeName() string {
	if runtime.GOOS == "windows" {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
}

func ffBundleDir() string {
	base := cacheDir
	if base == "" {
		// cache 目录没建起来（磁盘只读之类）：退到 exe 旁边，再不行用临时目录。
		// 这里不能失败返回，否则「内置了却用不上」，白搭 30MB 体积。
		if exe, err := os.Executable(); err == nil {
			base = filepath.Join(filepath.Dir(exe), "cache")
		} else {
			base = filepath.Join(os.TempDir(), "selfnas-cache")
		}
	}
	return filepath.Join(base, "ffbundle-"+ffmpegBundlePlat)
}

// ffReleaseBundled 把内置 ffmpeg 释放到磁盘并返回路径；没有内置包时返回空串。
// 只在第一次（或内置包版本变化后）真正解压，之后靠 .ok 标记直接命中。
func ffReleaseBundled() string {
	if len(ffmpegBundle) == 0 {
		return ""
	}
	dir := ffBundleDir()
	dst := filepath.Join(dir, ffBundleExeName())
	mark := filepath.Join(dir, ".ok")

	if b, err := os.ReadFile(mark); err == nil && string(b) == ffBundleStamp() {
		if st, err2 := os.Stat(dst); err2 == nil && st.Size() > 0 {
			return dst
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[ffmpeg] 内置包解压失败（建目录）: %v", err)
		return ""
	}
	zr, err := zstd.NewReader(bytes.NewReader(ffmpegBundle))
	if err != nil {
		log.Printf("[ffmpeg] 内置包损坏: %v", err)
		return ""
	}
	defer zr.Close()

	tmp := dst + ".new"
	f, err := os.Create(tmp)
	if err != nil {
		log.Printf("[ffmpeg] 内置包解压失败（建文件）: %v", err)
		return ""
	}
	n, err := io.Copy(f, zr)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		os.Remove(tmp)
		log.Printf("[ffmpeg] 内置包解压中断: %v", err)
		return ""
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		log.Printf("[ffmpeg] 内置包落盘失败: %v", err)
		return ""
	}
	os.Chmod(dst, 0o755)
	if err := os.WriteFile(mark, []byte(ffBundleStamp()), 0o644); err != nil {
		// 标记写不上只是下次多解压一遍，不影响使用
		log.Printf("[ffmpeg] 写标记失败（不影响使用）: %v", err)
	}
	log.Printf("[ffmpeg] 内置 ffmpeg 已释放到 %s（%.1f MB）", dst, float64(n)/1048576)
	return dst
}
