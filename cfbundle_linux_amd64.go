//go:build linux && amd64

package main

import _ "embed"

//go:embed assets/cloudflared/linux-amd64/cloudflared.zst
var cfBundle []byte

const cfBundlePlat = "linux-amd64"
