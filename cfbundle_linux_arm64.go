//go:build linux && arm64

package main

import _ "embed"

//go:embed assets/cloudflared/linux-arm64/cloudflared.zst
var cfBundle []byte

const cfBundlePlat = "linux-arm64"
