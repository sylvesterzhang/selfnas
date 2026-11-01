package server

// SMB2 CREATE context "MxAc" —— SMB2_CREATE_QUERY_MAXIMAL_ACCESS_REQUEST。
//
// 为什么非回不可：
// 这是客户端在问「我在这条句柄上究竟能拿到多少权限」
// （请求见 MS-SMB2 §2.2.13.2.2，应答见 §2.2.13.2.3）。Windows 服务器一律回答，
// 而 Apple 的客户端会**在打开共享根时带上它**——实测一次挂载 173 次。
//
// 我们不回，客户端能得到的唯一解释就是「服务端没给任何访问权限」。
// iOS「文件」App 里那个「只读」标记、以及「新建文件夹」变灰，来源就在这里：
// 协议层其余字段（TREE_CONNECT 的 MaximalAccess、FILE_READ_ONLY_VOLUME、
// 目录属性位）我们全是对的，唯独这个「询问」被当空气。
//
// 应答体是 8 字节：
//
//	QueryStatus   (4)  访问检查的 NTSTATUS，成功时为 0
//	MaximalAccess (4)  授予的访问掩码
//
// 注意 CreateDisposition/DesiredAccess 读文件时也可能带这个上下文，
// 所以按句柄所在共享的 MaximalAccess 统一回答即可（我们不区分文件级 ACL）。

import "encoding/binary"

// createTagMxAc 是 MS-SMB2 §2.2.13.2.2 定义的上下文名，大小写敏感。
const createTagMxAc = "MxAc"

// maximalAccessReply 在请求带 "MxAc" 上下文时构造对应的应答上下文。
// 返回 nil 表示客户端没问，不要凭空塞一个。
func maximalAccessReply(reqCtxs []createContext, maximalAccess uint32) *createContext {
	for _, cc := range reqCtxs {
		if cc.Name != createTagMxAc {
			continue
		}
		data := make([]byte, 8)
		binary.LittleEndian.PutUint32(data[0:], 0) // QueryStatus = STATUS_SUCCESS
		binary.LittleEndian.PutUint32(data[4:], maximalAccess)
		return &createContext{Name: createTagMxAc, Data: data}
	}
	return nil
}

// MaximalAccesser 是可选的 VFS 能力：让「最多能拿到多少权限」按会话动态决定，
// 而不是整个共享写死一个值。返回 0 表示不参与（按 Share.MaximalAccess 走）。
//
// PATCH(selfnas): 「共享」是一个按账号合成的虚拟根，同一个共享对 zh 是只读、
// 对另一个人可能是读写，静态的 Share.MaximalAccess 表达不了。不实现这个接口时
// 服务端一律宣告 0x001F01FF（全权限），客户端会先把共享挂成可写、传到一半才发现
// 写不进去 —— 这正是要避免的「假可写」。
type MaximalAccesser interface {
	MaximalAccess(sess *Session) uint32
}

// shareMaximalAccess 返回应答里应该用的访问掩码。
// Share.MaximalAccess 为 0 时按 TREE_CONNECT 的默认值走，保持两处口径一致。
func shareMaximalAccess(share Share, sess *Session) uint32 {
	if ar, ok := share.VFS.(MaximalAccesser); ok && sess != nil {
		if mx := ar.MaximalAccess(sess); mx != 0 {
			return mx
		}
	}
	if share.MaximalAccess == 0 {
		return defaultMaximalAccess
	}
	return share.MaximalAccess
}
