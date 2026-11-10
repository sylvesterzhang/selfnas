// 构建期小工具：把 ffmpeg 二进制压成 zstd，供 go:embed 使用。
//
// 为什么单独写个 Go 程序而不是用 xz 命令行：压缩和解压必须用同一套实现，
// 否则格式细节容易对不上（zstd 的 frame 参数、checksum 开关等）。
// 这里和 ffbundle.go 用的是同一个库、同一种编码级别。
//
// 用法：go run ./scripts/mkzstd <源文件> <目标文件>
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "用法: mkzstd <源文件> <目标文件>")
		os.Exit(2)
	}
	src, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer src.Close()
	dst, err := os.Create(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer dst.Close()

	// SpeedBestCompression 对大二进制最划算：80MB 级文件多花十几秒，
	// 体积能比默认档小 15% 左右 —— 这体积是每个用户的下载量，值得。
	enc, err := zstd.NewWriter(dst, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	n, err := io.Copy(enc, src)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := enc.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("  %s → %s  %.1f MB → %.1f MB\n", os.Args[1], os.Args[2],
		float64(n)/1048576, float64(fileSize(os.Args[2]))/1048576)
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}
