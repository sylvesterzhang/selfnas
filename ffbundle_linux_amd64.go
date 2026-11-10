//go:build linux && amd64

package main

import _ "embed"

//go:embed assets/ffmpeg/linux-amd64/ffmpeg.zst
var ffmpegBundle []byte

const ffmpegBundlePlat = "linux-amd64"
