//go:build !((windows && amd64) || (linux && amd64) || (linux && arm64) || (darwin && amd64) || (darwin && arm64))

package main

// 没有内置包的平台（freebsd、windows-arm64 等）：cfReleaseBundled 直接返回空串，
// cfFindExe 回落去找系统 PATH 里的 cloudflared，再没有就由调用方给出明确提示。
var cfBundle []byte

const cfBundlePlat = "unsupported"
