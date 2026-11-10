package main

import (
	"os/exec"
	"strings"
	"testing"
)

// 内置 ffmpeg 是「装上就能用」的兜底，必须自己验证它能真的释放并跑起来 ——
// 开发机上往往已经有个系统 ffmpeg 抢先命中，findFF 那条路测不出这里的问题。
func TestFFReleaseBundled(t *testing.T) {
	if len(ffmpegBundle) == 0 {
		t.Skipf("当前平台 %s 没有内置 ffmpeg，跳过", ffmpegBundlePlat)
	}
	p := ffReleaseBundled()
	if p == "" {
		t.Fatal("释放内置 ffmpeg 失败")
	}
	out, err := exec.Command(p, "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("释放出来的 ffmpeg 跑不起来: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ffmpeg version") {
		t.Fatalf("输出不像 ffmpeg: %s", out)
	}
	t.Logf("内置 ffmpeg: %s\n%s", p, firstLine(string(out)))

	// 光跑起来不够：音效/响度要用的滤镜、以及输出 320k mp3 要用的编码器必须在。
	// lgpl 构建里都该有（lame 是 LGPL），但没有的话这两个开关就是摆设。
	filters, _ := exec.Command(p, "-hide_banner", "-filters").CombinedOutput()
	for _, need := range []string{"loudnorm", "equalizer", "volume", "aresample"} {
		if !strings.Contains(string(filters), need) {
			t.Errorf("内置 ffmpeg 缺滤镜: %s", need)
		}
	}
	enc, _ := exec.Command(p, "-hide_banner", "-encoders").CombinedOutput()
	for _, need := range []string{"libmp3lame", "flac", "aac"} {
		if !strings.Contains(string(enc), need) {
			t.Errorf("内置 ffmpeg 缺编码器: %s", need)
		}
	}
	dec, _ := exec.Command(p, "-hide_banner", "-decoders").CombinedOutput()
	for _, need := range []string{"flac", "mp3", "aac"} {
		if !strings.Contains(string(dec), need) {
			t.Errorf("内置 ffmpeg 缺解码器: %s", need)
		}
	}

	// 第二次必须走缓存（不应该重解压）：路径一致，且文件还在
	if again := ffReleaseBundled(); again != p {
		t.Fatalf("二次释放路径不一致: %s vs %s", again, p)
	}
}
