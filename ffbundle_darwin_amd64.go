//go:build darwin && amd64

package main

import _ "embed"

//go:embed assets/ffmpeg/darwin-amd64/ffmpeg.zst
var ffmpegBundle []byte

const ffmpegBundlePlat = "darwin-amd64"
