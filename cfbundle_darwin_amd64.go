//go:build darwin && amd64

package main

import _ "embed"

//go:embed assets/cloudflared/darwin-amd64/cloudflared.zst
var cfBundle []byte

const cfBundlePlat = "darwin-amd64"
