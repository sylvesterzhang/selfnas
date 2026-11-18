package main

// cloudflared 内置分发：官方二进制 zstd 压好嵌进 exe，首次用到时释放到 cache/ 再执行。
//
// 为什么不 import cloudflared 的 Go 包：它内部依赖锁定得很死，拉进任何工程都会撞
// 版本（实测 `undefined: altsrc.ApplyInputSource`），官方也从没打算让人 import。
// 而它的 CLI 极稳 —— quick tunnel 分配的域名直接打在 stderr 里，抓一行正则即可。
// 所以走和 ffmpeg 完全一样的老路：官方二进制 → zstd → go:embed → 首次释放。
//
// 为什么不用 go:embed 直接放裸二进制：五平台裸文件 38~53MB，压缩后 11~16MB，
// 差出来的 20MB+ 全在用户下载包上。压缩是白赚的。
//
// 平台上没有内置包时（cfBundle 为空），调用方会回落到「系统 PATH 里找 cloudflared」，
// 再没有就明确告诉用户在哪些平台不提供，而不是静默失败。

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/klauspost/compress/zstd"
)

// cfBundleVersion 与 scripts/make_cfbundle.py 的 TAG 保持一致，
// 只用于日志和「这版内置的是哪个 cloudflared」的说明；失效判断看长度。
const cfBundleVersion = "2026.11.0"

// cfBundleStamp 标记当前内置的是哪一版：换了包长度必然变，缓存自然失效。
// 前缀带上版本号，升级 cloudflared 时即使长度巧合相同也不会误命中。
func cfBundleStamp() string {
	return fmt.Sprintf("%s:%s:%d", cfBundleVersion, cfBundlePlat, len(cfBundle))
}

func cfBundleExeName() string {
	if runtime.GOOS == "windows" {
		return "cloudflared.exe"
	}
	return "cloudflared"
}

func cfBundleDir() string {
	base := cacheDir
	if base == "" {
		// cache 目录没建起来（磁盘只读之类）：退到 exe 旁边，再不行用临时目录。
		if exe, err := os.Executable(); err == nil {
			base = filepath.Join(filepath.Dir(exe), "cache")
		} else {
			base = filepath.Join(os.TempDir(), "selfnas-cache")
		}
	}
	return filepath.Join(base, "cloudflared-"+cfBundlePlat)
}

// cfReleaseBundled 把内置 cloudflared 释放到磁盘并返回路径；没有内置包时返回空串。
// 只在第一次（或内置包变化后）真正解压，之后靠 .ok 标记直接命中。
func cfReleaseBundled() string {
	if len(cfBundle) == 0 {
		return ""
	}
	dir := cfBundleDir()
	dst := filepath.Join(dir, cfBundleExeName())
	mark := filepath.Join(dir, ".ok")

	if b, err := os.ReadFile(mark); err == nil && string(b) == cfBundleStamp() {
		if st, err2 := os.Stat(dst); err2 == nil && st.Size() > 0 {
			return dst
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[cloudflared] 内置包解压失败（建目录）: %v", err)
		return ""
	}
	zr, err := zstd.NewReader(bytes.NewReader(cfBundle))
	if err != nil {
		log.Printf("[cloudflared] 内置包损坏: %v", err)
		return ""
	}
	defer zr.Close()

	tmp := dst + ".new"
	f, err := os.Create(tmp)
	if err != nil {
		log.Printf("[cloudflared] 内置包解压失败（建文件）: %v", err)
		return ""
	}
	n, err := io.Copy(f, zr)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		os.Remove(tmp)
		log.Printf("[cloudflared] 内置包解压中断: %v", err)
		return ""
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		log.Printf("[cloudflared] 内置包落盘失败: %v", err)
		return ""
	}
	// Linux / macOS 必须可执行，否则 exec 直接 permission denied。
	os.Chmod(dst, 0o755)
	if err := os.WriteFile(mark, []byte(cfBundleStamp()), 0o644); err != nil {
		// 标记写不上只是下次多解压一遍，不影响使用
		log.Printf("[cloudflared] 写标记失败（不影响使用）: %v", err)
	}
	log.Printf("[cloudflared] 内置 cloudflared %s 已释放到 %s（%.1f MB）",
		cfBundleVersion, dst, float64(n)/1048576)
	return dst
}

// cfFindExe 找可用的 cloudflared：内置优先，其次系统 PATH。
// 返回 (路径, 来源)；来源用于前端显示「内置」还是「系统的」。
func cfFindExe() (string, string) {
	if p := cfReleaseBundled(); p != "" {
		return p, "builtin"
	}
	if p, err := exec.LookPath("cloudflared"); err == nil {
		return p, "system"
	}
	return "", ""
}
