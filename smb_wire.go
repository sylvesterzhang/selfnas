package main

// SMB 线级报文转储（诊断开关）。
//
// 为什么需要它：
// iOS「文件」App 一旦把共享判成「只读」，就**不会再发任何写请求**。此时服务端
// 日志里只剩读操作，无论怎么加日志都看不到「它到底读了什么、我们答得对不对」。
// 更麻烦的是，判「只读」的依据（Apple 客户端 smbfs_mount 里的两个条件）全部
// 在**响应报文的字段**里，光看我们自己打的「我打算发什么」不算数 ——
// 必须确认线上真正发出去的字节。
//
// 用法：设 SELFNAS_SMB_WIRE=1 启动，收发两个方向的完整 PDU 会按十六进制落到
// logs/smb-wire-<日期>.log。SMB2 头固定 64 字节，可以离线逐字段还原。
//
// 只记控制类命令（见 smbWireInteresting）；READ/WRITE 这类大包只记前
// smbWireCap 字节，避免拷几个 G 的文件就把磁盘写满。

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// smbWireCap 是单条报文最多记录的字节数。控制类命令远小于这个值，
// 只有 READ/WRITE 会被截断。
const smbWireCap = 512

var (
	smbWireMu    sync.Mutex
	smbWireReady bool
	smbWireOn    bool
)

// smbWireInit 读环境变量决定是否开启，并把开关缓存下来（每条报文都读环境变量太浪费）。
func smbWireInit() {
	smbWireMu.Lock()
	defer smbWireMu.Unlock()
	if smbWireReady {
		return
	}
	smbWireReady = true
	v := strings.TrimSpace(os.Getenv("SELFNAS_SMB_WIRE"))
	smbWireOn = v == "1" || strings.EqualFold(v, "all") || strings.EqualFold(v, "on")
}

// smbWireInteresting 报告某条命令是否值得记录。
// 目的是把「判定只读」相关的握手全留下，又不被读写大流量淹掉。
func smbWireInteresting(cmd uint16) bool {
	switch cmd {
	case 0x00, // NEGOTIATE
		0x01, // SESSION_SETUP
		0x02, // LOGOFF
		0x03, // TREE_CONNECT
		0x04, // TREE_DISCONNECT
		0x05, // CREATE
		0x06, // CLOSE
		0x07, // FLUSH
		0x09, // WRITE
		0x0b, // IOCTL
		0x0e, // QUERY_DIRECTORY
		0x0f, // CHANGE_NOTIFY
		0x10, // QUERY_INFO
		0x11: // SET_INFO
		return true
	}
	return false
}

// smbWireDump 把一条 PDU 追加到当天的 wire 文件。dir 是 ">>"（客户端→服务端）
// 或 "<<"（服务端→客户端）。
func smbWireDump(remote, dir string, raw []byte) {	smbWireInit()
	if !smbWireOn || len(raw) < 64 || logDir == "" {
		return
	}
	cmd := uint16(raw[12]) | uint16(raw[13])<<8
	if !smbWireInteresting(cmd) {
		return
	}
	// 头里的 Status 只有响应方向有意义；请求方向那 4 字节固定是 0。
	status := uint32(raw[8]) | uint32(raw[9])<<8 | uint32(raw[10])<<16 | uint32(raw[11])<<24
	mid := uint64(raw[24]) | uint64(raw[25])<<8 | uint64(raw[26])<<16 | uint64(raw[27])<<24 |
		uint64(raw[28])<<32 | uint64(raw[29])<<40 | uint64(raw[30])<<48 | uint64(raw[31])<<56
	tid := uint32(raw[36]) | uint32(raw[37])<<8 | uint32(raw[38])<<16 | uint32(raw[39])<<24
	flags := uint32(raw[16]) | uint32(raw[17])<<8 | uint32(raw[18])<<16 | uint32(raw[19])<<24

	n := len(raw)
	cut := ""
	if n > smbWireCap {
		n = smbWireCap
		cut = fmt.Sprintf(" …(+%d)", len(raw)-smbWireCap)
	}
	line := fmt.Sprintf("%s %s %-15s cmd=0x%02x %-16s status=0x%08x flags=0x%08x mid=%d tid=%d len=%d%s %s\n",
		time.Now().Format("2006-01-02 15:04:05.000000"), dir, remote,
		cmd, smbCmdName(cmd), status, flags, mid, tid, len(raw), cut,
		hex.EncodeToString(raw[:n]))

	smbWireMu.Lock()
	defer smbWireMu.Unlock()
	path := filepath.Join(logDir, "smb-wire-"+time.Now().Format("2006-01-02")+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line)
	_ = f.Close()
}

// describeCreateContexts 把 CREATE 请求里的 create context 列出来。
//
// 为什么单独做：Apple 客户端谈「我支持哪些 Apple 扩展」除了 TREE_CONNECT 的
// AAPL 上下文，还会在 CREATE 里带上下文（读目录属性等）。我们目前对不认识的
// 上下文一律忽略——这符合协议，但如果客户端正是靠它判断「能不能安全写」，
// 忽略就等于沉默，所以至少要先能看见它到底问了什么。
func describeCreateContexts(buf []byte) string {
	var sb strings.Builder
	for pos := 0; pos+16 <= len(buf); {
		next := int(uint32(buf[pos]) | uint32(buf[pos+1])<<8 | uint32(buf[pos+2])<<16 | uint32(buf[pos+3])<<24)
		nameOff := int(uint16(buf[pos+4]) | uint16(buf[pos+5])<<8)
		nameLen := int(uint16(buf[pos+6]) | uint16(buf[pos+7])<<8)
		dataOff := int(uint16(buf[pos+10]) | uint16(buf[pos+11])<<8)
		dataLen := int(uint32(buf[pos+12]) | uint32(buf[pos+13])<<8 | uint32(buf[pos+14])<<16 | uint32(buf[pos+15])<<24)

		name := "?"
		if nameOff+nameLen <= len(buf)-pos {
			name = string(buf[pos+nameOff : pos+nameOff+nameLen])
		}
		data := ""
		if dataLen > 0 && dataOff+dataLen <= len(buf)-pos {
			n := dataLen
			if n > 24 {
				n = 24
			}
			data = " data=" + hex.EncodeToString(buf[pos+dataOff:pos+dataOff+n])
			if dataLen > n {
				data += fmt.Sprintf("…(%dB)", dataLen)
			}
		}
		fmt.Fprintf(&sb, "%s(%dB)%s ", name, dataLen, data)

		if next <= 0 || pos+next > len(buf) {
			break
		}
		pos += next
	}
	return strings.TrimSpace(sb.String())
}
