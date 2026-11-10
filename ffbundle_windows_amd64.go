//go:build windows && amd64

package main

import _ "embed"

//go:embed assets/ffmpeg/windows-amd64/ffmpeg.zst
var ffmpegBundle []byte

const ffmpegBundlePlat = "windows-amd64"
