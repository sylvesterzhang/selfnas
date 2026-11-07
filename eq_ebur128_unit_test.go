package main

import "testing"

// ebur128 的 Summary 是给人看的文本，格式一变解析就静默失效（会退回 loudnorm，
// 慢 3 倍但不报错）——所以把真实输出钉成用例。
const ebur128Sample = `[Parsed_ebur128_2 @ 0000009ba963ab80] t: 96.099977  TARGET:-23 LUFS    M: -68.2 S: -67.2     I: -17.6 LUFS       LRA:   9.6 LU  FTPK: -51.0 -50.5 dBFS  TPK:  -3.3  -3.9 dBFS
[Parsed_ebur128_2 @ 0000009ba963ab80] Summary:

  Integrated loudness:
    I:         -17.8 LUFS
    Threshold: -28.4 LUFS

  Loudness range:
    LRA:         9.5 LU
    Threshold: -38.3 LUFS
    LRA low:   -24.5 LUFS
    LRA high:  -15.0 LUFS

  True peak:
    Peak:       -3.7 dBFS
[out#0/null @ 0000009ba9613e00] video:0KiB audio:16556KiB subtitle:0KiB other streams:0KiB global headers:0KiB muxing overhead: unknown
`

func TestParseEBUR128(t *testing.T) {
	m, err := parseEBUR128(ebur128Sample)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 周期日志里也有 I: -17.6，必须取 Summary 里那个 -17.8
	if m["input_i"] != -17.8 {
		t.Errorf("input_i = %v, 要 -17.8", m["input_i"])
	}
	if m["input_tp"] != -3.7 {
		t.Errorf("input_tp = %v, 要 -3.7", m["input_tp"])
	}
	if m["input_lra"] != 9.5 {
		t.Errorf("input_lra = %v, 要 9.5", m["input_lra"])
	}
}

func TestParseEBUR128Silence(t *testing.T) {
	// ebur128 的下限是 -70，静音文件会命中这个值；此时线性增益没意义，
	// 必须报错让上层退化成动态模式。
	_, err := parseEBUR128("Summary:\n    I:         -70.0 LUFS\n    Peak:      -70.0 dBFS\n")
	if err == nil {
		t.Fatal("静音应当报错")
	}
}

func TestParseEBUR128NoSummary(t *testing.T) {
	if _, err := parseEBUR128("t: 0.1 TARGET:-23 LUFS I: -17.6 LUFS\n"); err == nil {
		t.Fatal("没有 Summary 应当报错")
	}
}
