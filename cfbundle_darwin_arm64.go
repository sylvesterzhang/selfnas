//go:build darwin && arm64

package main

import _ "embed"

//go:embed assets/cloudflared/darwin-arm64/cloudflared.zst
var cfBundle []byte

const cfBundlePlat = "darwin-arm64"
