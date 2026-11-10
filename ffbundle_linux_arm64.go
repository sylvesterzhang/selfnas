//go:build linux && arm64

package main

import _ "embed"

//go:embed assets/ffmpeg/linux-arm64/ffmpeg.zst
var ffmpegBundle []byte

const ffmpegBundlePlat = "linux-arm64"
