package server

// SMB2 CREATE context "AAPL" —— Apple 的扩展能力协商（MS-AAPL）。
//
// 为什么非回不可（这是「iOS 文件 App 显示只读」的最后一环）：
//
// Apple 客户端在 K 挂载后对共享根的第一次 CREATE 会带一个 24 字节的
// "AAPL" 上下文，问「你支持哪些 Apple 扩展」。Samba 里对应的是
// vfs_fruit 的 check_aapl()（source3/modules/vfs_fruit.c）。
//
// 不回，客户端只能认为「这是一台非 Apple-aware 的服务器」，于是防御性地
// 把挂载降为只读 —— 症状是共享名下面写着「只读」、新建文件夹变灰，
// 但协议层每个响应都是 STATUS_SUCCESS，日志里找不到任何错误。
//
// 社区里 samba 的定论修复是 `vfs objects = catia fruit streams_xattr`：
//   - streams_xattr 让 fs_capabilities 带上 FILE_NAMED_STREAMS
//   - fruit 在收到本上下文时，只有在 fs_capabilities 含 FILE_NAMED_STREAMS
//     的前提下才会把 SUPPORTS_READ_DIR_ATTR 置位回给客户端
// 两半缺一不可，这正好解释了为什么只加 FILE_NAMED_STREAMS（queryinfo.go
// 里已经加了）而没有回本上下文时，症状依然存在。
//
// 请求布局（固定 24 字节，vfs_fruit.c 用 IVAL/BVAL 按偏移读）：
//
//	off 0  (4)  CommandCode       1 = SERVER_QUERY，2 = RESOLVE_ID
//	off 4  (4)  保留，未使用
//	off 8  (8)  req_bitmap        1 = 要 ServerCaps，2 = 要 VolumeCaps，4 = 要 ModelInfo
//	off 16 (8)  client_caps       客户端自己的能力位
//
// 应答布局（16 字节定长头 + 按 req_bitmap 追加的可选段）：
//
//	off 0  (4)  CommandCode = 1
//	off 4  (4)  0
//	off 8  (8)  req_bitmap 原样回显
//	然后：
//	  req_bitmap & 1 -> 8 字节 server_caps
//	  req_bitmap & 2 -> 8 字节 volume_caps
//	  req_bitmap & 4 -> 8 字节 {u32 保留=0, u32 modelLen} + model 的 UTF-16LE
//
// 注意：Samba 在长度不等于 24 或 CommandCode 不是 1 时直接返回
// STATUS_INVALID_PARAMETER。我们选择「日志记录 + 忽略」，因为把整个 CREATE
// 打回会让该客户端连浏览都做不到，比不答更糟。

import (
	"encoding/binary"
	"os"
	"strconv"

	"github.com/jfjallid/go-smb/smb/unicode"
)

const createTagAAPL = "AAPL"

// "AAPL" 上下文命令码。
const (
	aaplCmdServerQuery = 1
	aaplCmdResolveID   = 2
)

// Server Query 的请求/应答位图。
const (
	aaplReqServerCaps = 1
	aaplReqVolumeCaps = 2
	aaplReqModelInfo  = 4
)

// 客户端/服务端能力位（vfs_fruit.c 与 libcli/smb/smb2_create_ctx.h 同源）。
const (
	aaplCapSupportsReadDirAttr = 1
	aaplCapSupportsOSXCopyFile = 2
	aaplCapUnixBased           = 4
	aaplCapSupportsNFSAce      = 8
)

// Volume 能力位。
const (
	aaplVolSupportResolveID = 1
	aaplVolCaseSensitive    = 2
	aaplVolFullSync         = 4
)

// aaplModel 是 ModelInfo 段里回给客户端的产品名。Samba 的对应配置项是
// fruit:model，默认值 "MacSamba"。可以用环境变量覆盖，纯诊断用。
func aaplModel() string {
	if v := os.Getenv("SELFNAS_SMB_AAPL_MODEL"); v != "" {
		return v
	}
	return "selfnas"
}

// aaplServerCaps 计算应答的 server_caps。默认口径对齐 Samba：
// servicaps 初始为 UNIX_BASED，只有「客户端要了 且 本文件系统声明了
// FILE_NAMED_STREAMS」才加上 SUPPORTS_READ_DIR_ATTR。
//
// 不声明 SUPPORTS_OSX_COPYFILE：那要求服务端实现 FSCTL_SRV_COPYCHUNK 的
// 服务端拷贝语义，我们没做，虚报会让客户端的复制走一条不存在的能力。
// 不声明 SUPPORTS_NFS_ACE：我们没有 POSIX/NFS ACE。
//
// SELFNAS_SMB_AAPL_SERVER_CAPS 可以把结果整个覆盖掉，用来在不重编译的
// 情况下逐位试（诊断只读问题时很有用）。
func aaplServerCaps(clientCaps uint64, fsAttrs uint32) uint64 {
	if v := os.Getenv("SELFNAS_SMB_AAPL_SERVER_CAPS"); v != "" {
		if n, err := strconv.ParseUint(v, 0, 64); err == nil {
			return n
		}
	}
	caps := uint64(aaplCapUnixBased)
	if clientCaps&aaplCapSupportsReadDirAttr != 0 && fsAttrs&FsAttrNamedStreams != 0 {
		caps |= aaplCapSupportsReadDirAttr
	}
	return caps
}

// aaplVolumeCaps 计算应答的 volume_caps。
// 我们的宿主是 Windows，路径大小写不敏感，所以不置 CASE_SENSITIVE；
// 不打算做 Time Machine，所以不置 FULL_SYNC。留 0 是最诚实的回答。
func aaplVolumeCaps() uint64 {
	if v := os.Getenv("SELFNAS_SMB_AAPL_VOLUME_CAPS"); v != "" {
		if n, err := strconv.ParseUint(v, 0, 64); err == nil {
			return n
		}
	}
	return 0
}

// aaplReply 在请求带 "AAPL" 上下文时构造应答上下文。
// 返回 nil 表示客户端没问（或命令码不认识），不要凭空塞一个。
// 第二个返回值是宣告的 serverCaps，调用方据此更新会话的 Apple 扩展状态。
func (c *Conn) aaplReply(reqCtxs []createContext) (*createContext, uint64) {
	logger := c.logger()
	for _, cc := range reqCtxs {
		if cc.Name != createTagAAPL {
			continue
		}
		if len(cc.Data) != 24 {
			logger.Debugf("Create: AAPL context has unexpected length %d (want 24) -> ignored", len(cc.Data))
			continue
		}
		cmd := binary.LittleEndian.Uint32(cc.Data[0:])
		if cmd != aaplCmdServerQuery {
			// RESOLVE_ID 只在 MxAc 之后用，我们已经有 MxAc 应答了；不答即可。
			logger.Debugf("Create: AAPL context cmd=%d (not SERVER_QUERY) -> ignored", cmd)
			continue
		}
		reqBitmap := binary.LittleEndian.Uint64(cc.Data[8:])
		clientCaps := binary.LittleEndian.Uint64(cc.Data[16:])

		serverCaps := aaplServerCaps(clientCaps, fsAttrBits())
		volumeCaps := aaplVolumeCaps()
		model := unicode.ToUnicode(aaplModel())

		data := make([]byte, 16)
		binary.LittleEndian.PutUint32(data[0:], aaplCmdServerQuery)
		binary.LittleEndian.PutUint32(data[4:], 0)
		binary.LittleEndian.PutUint64(data[8:], reqBitmap)

		if reqBitmap&aaplReqServerCaps != 0 {
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], serverCaps)
			data = append(data, b[:]...)
		}
		if reqBitmap&aaplReqVolumeCaps != 0 {
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], volumeCaps)
			data = append(data, b[:]...)
		}
		if reqBitmap&aaplReqModelInfo != 0 {
			var b [8]byte
			binary.LittleEndian.PutUint32(b[0:], 0)
			binary.LittleEndian.PutUint32(b[4:], uint32(len(model)))
			data = append(data, b[:]...)
			data = append(data, model...)
		}

		logger.Debugf("Create: AAPL SERVER_QUERY reqBitmap=0x%x clientCaps=0x%x -> serverCaps=0x%x volumeCaps=0x%x model=%q (%d bytes)",
			reqBitmap, clientCaps, serverCaps, volumeCaps, aaplModel(), len(data))
		return &createContext{Name: createTagAAPL, Data: data}, serverCaps
	}
	return nil, 0
}
