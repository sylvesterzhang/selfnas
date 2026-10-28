package main

// ffmpeg 引导安装：用户在播放器里点「立即安装」，这里负责下载 → 解压 → 校验 → 热生效。
//
// 为什么在播放器里做而不是只在管理台：用户第一次点 MKV 播不了的那一刻才需要它，
// 在疼的地方给药，装完自动接着播，全程不用理解 ffmpeg 是什么。
//
// 为什么不让用户自己装：ffmpeg 官网跳第三方构建页、90MB、还要解压配 PATH，
// 对「想看个片」的人来说门槛太高了。

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 官方 essentials 构建：够用（ffmpeg + ffprobe）、比 full 包小一半。
// 国内直连慢的话，config.json 里 ffmpegUrl 填镜像地址即可覆盖。
const ffDefaultURL = "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip"

type ffInstallState struct {
	mu      sync.Mutex
	running bool
	stage   string // download / extract / verify / done
	err     string
	bytes   int64
	total   int64
}

var ffJob ffInstallState

func ffSetStage(stage string) {
	ffJob.mu.Lock()
	ffJob.stage = stage
	ffJob.mu.Unlock()
}

func ffSetErr(err error) {
	ffJob.mu.Lock()
	ffJob.err = err.Error()
	ffJob.mu.Unlock()
	log.Printf("[ffmpeg] 安装失败: %v", err)
}

// ffStatus 给前端轮询用：进度 + 结果 + 当前探测到的 ffmpeg。
// 顺带跑一次 -version 拿版本行（毫秒级），让用户看到「装的是什么」。
func ffStatus() map[string]any {
	ffJob.mu.Lock()
	st := map[string]any{
		"running": ffJob.running,
		"stage":   ffJob.stage,
		"error":   ffJob.err,
		"bytes":   ffJob.bytes,
		"total":   ffJob.total,
	}
	ffJob.mu.Unlock()

	ff, _ := findFF()
	if ff != "" {
		st["path"] = ff
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, ff, "-version").CombinedOutput(); err == nil {
			st["version"] = strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
		}
	}
	return st
}

// startFFInstall 异步跑安装，状态靠 /api/ffmpeg/status 轮询。
func startFFInstall() error {
	ffJob.mu.Lock()
	if ffJob.running {
		ffJob.mu.Unlock()
		return errors.New("已有安装任务在进行中")
	}
	ffJob.running = true
	ffJob.stage = "download"
	ffJob.err = ""
	ffJob.bytes, ffJob.total = 0, 0
	ffJob.mu.Unlock()

	url := ffDefaultURL
	cfgMu.Lock()
	if cfg.FFmpegURL != "" {
		url = cfg.FFmpegURL
	}
	cfgMu.Unlock()

	go func() {
		defer func() {
			ffJob.mu.Lock()
			ffJob.running = false
			ffJob.mu.Unlock()
		}()
		if err := ffDownloadAndInstall(url); err != nil {
			ffSetErr(err)
		}
	}()
	return nil
}

func ffDownloadAndInstall(zipURL string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exeDir := filepath.Dir(exe)

	// ── 下载 ──
	ffSetStage("download")
	log.Printf("[ffmpeg] 开始下载: %s", zipURL)
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", zipURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载失败（检查网络或在 config.json 的 ffmpegUrl 填镜像地址）: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("下载源返回 %s", resp.Status)
	}
	total := resp.ContentLength
	ffJob.mu.Lock()
	ffJob.total = total
	ffJob.mu.Unlock()

	tmpZip := filepath.Join(exeDir, "ffmpeg-download.zip.part")
	f, err := os.Create(tmpZip)
	if err != nil {
		return err
	}
	count := &progressWriter{total: func(n int64) {
		ffJob.mu.Lock()
		ffJob.bytes = n
		ffJob.mu.Unlock()
	}}
	_, err = io.Copy(io.MultiWriter(f, count), resp.Body)
	closeErr := f.Close()
	if err != nil {
		os.Remove(tmpZip)
		return err
	}
	if closeErr != nil {
		os.Remove(tmpZip)
		return closeErr
	}
	log.Printf("[ffmpeg] 下载完成: %.1f MB", float64(ffJob.bytes)/1048576)

	// ── 解压 ──
	ffSetStage("extract")
	zr, err := zip.OpenReader(tmpZip)
	if err != nil {
		os.Remove(tmpZip)
		return fmt.Errorf("下载的不是有效 zip: %w", err)
	}
	defer zr.Close()
	// zip 里路径形如 ffmpeg-7.1-essentials_build/bin/ffmpeg.exe，按文件名匹配
	var ffEntry, probeEntry *zip.File
	for i := range zr.File {
		base := strings.ToLower(filepath.Base(zr.File[i].Name))
		if base == "ffmpeg.exe" && ffEntry == nil {
			ffEntry = zr.File[i]
		}
		if base == "ffprobe.exe" && probeEntry == nil {
			probeEntry = zr.File[i]
		}
	}
	if ffEntry == nil {
		os.Remove(tmpZip)
		return errors.New("压缩包里没有 ffmpeg.exe（检查 ffmpegUrl 指向的包对不对）")
	}
	if err := extractTo(exeDir, ffEntry); err != nil {
		os.Remove(tmpZip)
		return err
	}
	if probeEntry != nil {
		if err := extractTo(exeDir, probeEntry); err != nil {
			os.Remove(tmpZip)
			return err
		}
	}
	os.Remove(tmpZip)

	// ── 校验：真的能跑 ──
	ffSetStage("verify")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, filepath.Join(exeDir, "ffmpeg.exe"), "-version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("装上了但跑不起来: %v: %s", err, firstLine(string(out)))
	}

	// ── 热生效 ──
	// -no-ffmpeg 是「假装没装」的测试钩子，现在真装上了，钩子的前提已不成立，
	// 不解除的话下面 findFF() 仍返回空，前端装完续播又会弹引导卡。
	ffForceMissing = false
	ffInvalidate()
	ff, _ := findFF()
	ffSetStage("done")
	log.Printf("[ffmpeg] 安装完成: %s", ff)
	return nil
}

// extractTo 解压单个文件到 dir，先写 .new 再替换，避免中断留下半个 exe。
func extractTo(dir string, zf *zip.File) error {
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	dst := filepath.Join(dir, filepath.Base(zf.Name))
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, rc); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err = out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst) // Windows 上会覆盖已存在的同名文件
}

type progressWriter struct {
	total func(int64)
	n     int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	p.total(p.n)
	return len(b), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
