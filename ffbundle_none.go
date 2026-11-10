//go:build !(windows && amd64) && !(linux && amd64) && !(linux && arm64) && !(darwin && amd64) && !(darwin && arm64)

package main

// 兜底分支：这五个主流平台之外（freebsd / windows-arm64 之类）没有内置 ffmpeg，
// 不嵌任何字节，findFF 继续走「系统里找 → 引导下载」那套。
var ffmpegBundle []byte

const ffmpegBundlePlat = ""
