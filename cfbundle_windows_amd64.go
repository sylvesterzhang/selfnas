//go:build windows && amd64

package main

import _ "embed"

//go:embed assets/cloudflared/windows-amd64/cloudflared.zst
var cfBundle []byte

const cfBundlePlat = "windows-amd64"
