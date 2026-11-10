//go:build darwin && arm64

package main

import _ "embed"

//go:embed assets/ffmpeg/darwin-arm64/ffmpeg.zst
var ffmpegBundle []byte

const ffmpegBundlePlat = "darwin-arm64"
