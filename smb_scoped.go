package main

// smb_scoped —— 「共享」虚拟合并视图。
//
// 受限账号连 SMB 只看到一个共享（名字就叫「共享」），它的根目录列出的是
// 后台给这个账号配置的白名单文件夹（每个文件夹一个虚拟子目录），点进去
// 就是真实的文件夹内容。这样 iPhone「文件」App 的体验是：
//
//	192.168.50.168 → 共享 → [电影] [工作资料] [照片]
//
// 实现方式：不重写一个完整文件系统，而是给每个白名单文件夹挂一个现成的
// filevfs.FS（和正式共享同一套实现，含只读/流/重命名所有语义），按路径
// 第一段做分发。根目录的枚举和 Stat 由这里合成。
//
// 关键点：VFS.Create 能拿到 *Session，用它查当前用户的白名单；其余方法
// 只拿 Handle，所以内层句柄在 Create 时就被包成 scopedHandle（记住了内层
// VFS），后续直接透传。

import (
	"context"
	"hash/fnv"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"
	"github.com/jfjallid/go-smb/smb/server/filevfs"
)

// scopedAllReadonly 判断这个账号的白名单是不是「一个能写的都没有」。
// 只有全只读才敢对外宣告只读权限：混合权限时根本身必须保持可写，
// 具体能不能写由每个子目录自己的 filevfs 决定。
func scopedAllReadonly(folders []ScopedFolder) bool {
	if len(folders) == 0 {
		return true
	}
	for _, f := range folders {
		if !f.Readonly {
			return false
		}
	}
	return true
}

// MaximalAccess 实现 server.MaximalAccesser：按账号决定「这个客户端最多能拿到多少权限」。
// 全只读 ⇒ 只给读权限位，客户端（资源管理器 / iOS「文件」）就不会把共享挂成可写。
// 返回 0 表示不参与，交给 Share.MaximalAccess 的默认值。
func (scopedVFS) MaximalAccess(sess *server.Session) uint32 {
	user := ""
	if sess != nil {
		user = sess.Username
	}
	folders, ok := scopedFoldersFor(user)
	if !ok || len(folders) == 0 {
		return 0
	}
	if scopedAllReadonly(folders) {
		return scopedReadOnlyAccess
	}
	return 0
}

// scopedShareName 受限账号唯一可见的共享名。
const scopedShareName = "共享"

// 访问掩码里「这个请求想写」的那些位。目录上 0x2/0x4 就是「建文件/建子目录」。
// 只读白名单要拒的就是这些：让客户端打开根时就吃 ACCESS_DENIED，
// 而不是先把共享当可写、传到一半才发现写不进去。
const (
	accFileWriteData   uint32 = 0x00000002 // 目录：FILE_ADD_FILE
	accFileAppendData  uint32 = 0x00000004 // 目录：FILE_ADD_SUBDIRECTORY
	accFileWriteEa     uint32 = 0x00000010
	accFileDeleteChild uint32 = 0x00000040
	accFileWriteAttrs  uint32 = 0x00000100
	accDelete          uint32 = 0x00010000
	accWriteDac        uint32 = 0x00040000
	accWriteOwner      uint32 = 0x00080000
)

const scopedWriteAccessMask = accFileWriteData | accFileAppendData | accFileWriteEa |
	accFileDeleteChild | accFileWriteAttrs | accDelete | accWriteDac | accWriteOwner

// scopedReadOnlyAccess 全只读时对外宣告的最大权限：
// FILE_GENERIC_READ(0x120089) 已经包含 READ_CONTROL(0x20000) + SYNCHRONIZE(0x100000)。
const scopedReadOnlyAccess uint32 = 0x00120089

// scopedRootFileID 虚拟根目录的 persistent FileID。
// 必须非 0：Windows 资源管理器拿 CREATE 响应里的 FileId 当「这个目录的身份证」，
// 0 会让它认为拿到的不是有效目录句柄，后面就不再发 QUERY_DIRECTORY 了
//（实测现象：资源管理器报「参数错误」，服务端日志里 CREATE 之后一条列目录都没有）。
const scopedRootFileID = 0x7363617065440001

// scopedRootResult 造「共享」根目录的 CreateResult。
//
// ⚠️ Info 必须填全，不能留零值：create.go 组装响应时直接用 result.Info，
// 不会拿 Handle.Stat() 兜底。Attributes=0 意味着「不是目录」——Windows 收到
// 「打开的是目录、返回的属性却不是目录」就直接放弃这个句柄；Apple 客户端
// 对时间戳为 0 也会当成异常。之前这里只给了 Handle，Windows 一律列不出内容。
func scopedRootResult(user string, action uint32) server.CreateResult {
	now := time.Now()
	return server.CreateResult{
		Handle:       &scopedHandle{root: true, user: user},
		CreateAction: action,
		Info: server.FileInfo{
			Name:           scopedShareName,
			FileID:         scopedRootFileID,
			Attributes:     smb.FileAttrDirectory,
			CreationTime:   now,
			LastAccessTime: now,
			LastWriteTime:  now,
			ChangeTime:     now,
		},
	}
}

// scopedInnerFS 按文件夹真实路径缓存内层 filevfs（New 要 os.Stat，别每次建）。
var (
	scopedFSMu    sync.Mutex
	scopedInnerFS = map[string]*filevfs.FS{}
)

// scopedInnerFor 取（或建）某个真实文件夹的内层 FS。
// readonly 是白名单里给这个文件夹配的写权限：filevfs 的只读是建 FS 时定的
// （写/建/改名/删除全部 STATUS_ACCESS_DENIED），所以缓存键必须带上权限位——
// 不然先建了可写的那份，后面改成只读还是能写（反之亦然）。
func scopedInnerFor(realPath string, readonly bool) (*filevfs.FS, error) {
	scopedFSMu.Lock()
	defer scopedFSMu.Unlock()
	key := strings.ToLower(realPath)
	if readonly {
		key += "|ro"
	}
	if fs, ok := scopedInnerFS[key]; ok {
		return fs, nil
	}
	fs, err := filevfs.New(filevfs.Options{Root: realPath, ReadOnly: readonly})
	if err != nil {
		return nil, err
	}
	scopedInnerFS[key] = fs
	return fs, nil
}

// scopedInvalidate 清掉内层 FS 缓存（白名单变更/盘重挂后调用；保守起见全清）。
func scopedInvalidate() {
	scopedFSMu.Lock()
	scopedInnerFS = map[string]*filevfs.FS{}
	scopedFSMu.Unlock()
}

// scopedVFS 实现 server.VFS。无状态：所有按用户的解析从 Session / Handle 里来。
type scopedVFS struct{}

// scopedSplit 把共享相对路径拆成（白名单展示名, 余下相对路径）。
// "电影\某片.mkv" → ("电影", "某片.mkv")；"" → ("", "")；"电影" → ("电影", "")。
func scopedSplit(p string) (first, rest string) {
	p = strings.Trim(p, `\`)
	if p == "" {
		return "", ""
	}
	if i := strings.Index(p, `\`); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

// scopedHandle 包内层句柄；virtual=true 表示这是「共享」根目录本身的句柄。
type scopedHandle struct {
	inner server.Handle
	vfs   *filevfs.FS // inner 对应的内层 FS（透传用）；virtual 时为 nil
	root  bool        // 是否「共享」根目录句柄
	user  string
}

func (h *scopedHandle) Stat() (server.FileInfo, error) {
	if h.root {
		now := time.Now()
		return server.FileInfo{
			Name:         scopedShareName,
			FileID:       scopedRootFileID,
			Attributes:   smb.FileAttrDirectory,
			CreationTime: now, LastAccessTime: now, LastWriteTime: now, ChangeTime: now,
		}, nil
	}
	return h.inner.Stat()
}
func (h *scopedHandle) Path() string {
	if h.root {
		return ""
	}
	return h.inner.Path()
}
func (h *scopedHandle) IsDir() bool {
	if h.root {
		return true
	}
	return h.inner.IsDir()
}

// scopedDeny 统一的拒绝返回。
func scopedDeny() (server.CreateResult, uint32, error) {
	return server.CreateResult{}, smb.StatusAccessDenied, nil
}

// Create 实现 server.VFS。根目录下的白名单文件夹是唯一入口：
//   - ""            → 根目录句柄（虚拟）
//   - "电影"        → 内层 FS 的根目录
//   - "电影\x\y"    → 内层 FS 的 "x\y"
//   - 其他（根下直接建文件、未知名字）→ 拒绝
func (scopedVFS) Create(_ context.Context, sess *server.Session, req server.CreateRequest) (server.CreateResult, uint32, error) {
	user := ""
	if sess != nil {
		user = sess.Username
	}
	folders, ok := scopedFoldersFor(user)
	if !ok || len(folders) == 0 {
		// 没配白名单的受限账号：什么都看不到，但根目录还是要给开出来，
		// 否则客户端连枚举都做不了、表现为「连不上」。
		if strings.Trim(req.Path, `\`) == "" {
			return scopedRootResult(user, smb.FileOpened), smb.StatusOk, nil
		}
		return scopedDeny()
	}

	name, rest := scopedSplit(req.Path)
	if name == "" {
		// 根目录本身。全只读时，带写意图的打开一律拒绝 —— 让客户端在
		// 「打开共享」这一步就知道不能写，而不是挂载成功、上传到一半才报错。
		// 读打开照旧放行：客户端要用它来枚举白名单。
		if scopedAllReadonly(folders) && req.DesiredAccess&scopedWriteAccessMask != 0 {
			log.Printf("[smb] %q 以写权限打开「共享」根目录被拒（白名单全只读，access=0x%08x）",
				user, req.DesiredAccess)
			return scopedDeny()
		}
		return scopedRootResult(user, smb.FileOpened), smb.StatusOk, nil
	}

	names := scopedNameFor(folders)
	realPath := ""
	for p, n := range names {
		if strings.EqualFold(n, name) {
			realPath = p
			break
		}
	}
	if realPath == "" {
		// 根下只允许白名单文件夹；直接建文件/未知名字一律拒绝
		return scopedDeny()
	}

	inner, err := scopedInnerFor(realPath, scopedReadonlyFor(folders, realPath))
	if err != nil {
		// 文件夹可能被移走/盘没挂：当路径不存在报，客户端好理解
		return server.CreateResult{}, smb.StatusObjectNameNotFound, nil
	}
	res, st, err := inner.Create(context.Background(), sess, server.CreateRequest{
		Path:              rest,
		DesiredAccess:     req.DesiredAccess,
		FileAttributes:    req.FileAttributes,
		ShareAccess:       req.ShareAccess,
		CreateDisposition: req.CreateDisposition,
		CreateOptions:     req.CreateOptions,
	})
	if err != nil || st != smb.StatusOk {
		return res, st, err
	}
	res.Handle = &scopedHandle{inner: res.Handle, vfs: inner, user: user}
	return res, st, nil
}

// delegRun 把调用转给内层 filevfs。返回 false 表示「这个句柄不归内层管」
// （虚拟根句柄，或不是我们造的句柄），调用方应回 STATUS_NOT_SUPPORTED
// 让服务端走自己的默认实现。
//
// ⚠️ 这里返回 bool 而不是 STATUS，是有血的教训：
// 早先写成 `st = deleg(...)`，deleg 内部返回 STATUS_OK，把闭包里刚写进去的
// 真实状态码**覆盖**掉了 —— filevfs 说「这个 info class 我不支持」「读失败了」，
// 到上层全变成「成功 + 空数据」。表现就是 Windows 资源管理器列共享时报
// 「参数错误」：内层回 NOT_SUPPORTED，上层却按成功去序列化 nil，
// 于是 `serializeOpaqueOrInfo: VFS returned unrecognized info type <nil>`。
func delegRun(h server.Handle, fn func(*filevfs.FS, server.Handle)) bool {
	sh, ok := h.(*scopedHandle)
	if !ok || sh.root || sh.vfs == nil {
		return false
	}
	fn(sh.vfs, sh.inner)
	return true
}

func (v scopedVFS) Close(_ context.Context, h server.Handle) error {
	if sh, ok := h.(*scopedHandle); ok && !sh.root && sh.vfs != nil {
		return sh.vfs.Close(context.Background(), sh.inner)
	}
	return nil
}

func (v scopedVFS) Read(_ context.Context, h server.Handle, offset int64, buf []byte) (int, uint32, error) {
	var (
		n   int
		st  uint32
		err error
	)
	if !delegRun(h, func(fs *filevfs.FS, ih server.Handle) {
		n, st, err = fs.Read(context.Background(), ih, offset, buf)
	}) {
		return 0, smb.StatusNotSupported, nil
	}
	return n, st, err
}

func (v scopedVFS) Write(_ context.Context, h server.Handle, offset int64, data []byte) (int, uint32, error) {
	var (
		n   int
		st  uint32
		err error
	)
	if !delegRun(h, func(fs *filevfs.FS, ih server.Handle) {
		n, st, err = fs.Write(context.Background(), ih, offset, data)
	}) {
		return 0, smb.StatusNotSupported, nil
	}
	return n, st, err
}

func (v scopedVFS) Flush(_ context.Context, h server.Handle) (uint32, error) {
	var st uint32
	var err error
	if !delegRun(h, func(fs *filevfs.FS, ih server.Handle) {
		st, err = fs.Flush(context.Background(), ih)
	}) {
		return smb.StatusNotSupported, nil
	}
	return st, err
}

func (v scopedVFS) QueryFileInfo(_ context.Context, h server.Handle, infoClass byte) (any, uint32, error) {
	var (
		out any
		st  uint32
		err error
	)
	if !delegRun(h, func(fs *filevfs.FS, ih server.Handle) {
		out, st, err = fs.QueryFileInfo(context.Background(), ih, infoClass)
	}) {
		return nil, smb.StatusNotSupported, nil
	}
	return out, st, err
}

func (v scopedVFS) SetFileInfo(_ context.Context, h server.Handle, infoClass byte, raw []byte) (uint32, error) {
	var st uint32
	var err error
	if !delegRun(h, func(fs *filevfs.FS, ih server.Handle) {
		st, err = fs.SetFileInfo(context.Background(), ih, infoClass, raw)
	}) {
		return smb.StatusNotSupported, nil
	}
	return st, err
}

// QueryDirectory：根句柄 → 合成白名单目录项；内层句柄 → 透传。
func (v scopedVFS) QueryDirectory(_ context.Context, h server.Handle, pattern string, restart bool) ([]server.DirEntry, uint32, error) {
	sh, ok := h.(*scopedHandle)
	if !ok {
		return nil, smb.StatusNotSupported, nil
	}
	if !sh.root {
		if sh.vfs == nil {
			return nil, smb.StatusNotSupported, nil
		}
		return sh.vfs.QueryDirectory(context.Background(), sh.inner, pattern, restart)
	}

	// 根目录：列出白名单文件夹。分页游标懒得做精确语义——iOS/Windows
	// 都是拿完为止（65536 的 outBuf 一次装得下几百个目录项），这里一次给全。
	folders, ok := scopedFoldersFor(sh.user)
	if !ok || len(folders) == 0 {
		return nil, smb.StatusOk, nil
	}
	var out []server.DirEntry
	for p, name := range scopedNameFor(folders) {
		st, err := os.Stat(p)
		if err != nil {
			continue // 文件夹没了就不列，别让整个目录挂掉
		}
		out = append(out, server.DirEntry{
			FileInfo: fileInfoFromOS(name, st),
		})
	}
	return out, smb.StatusOk, nil
}

// QueryFSInfo 交给服务端默认实现（卷信息对虚拟根没有意义）。
func (scopedVFS) QueryFSInfo(_ context.Context, _ byte) (any, uint32, error) {
	return nil, smb.StatusNotSupported, nil
}

func (scopedVFS) QuerySecurity(_ context.Context, _ server.Handle, _ uint32) ([]byte, uint32, error) {
	return nil, smb.StatusNotSupported, nil
}

func (scopedVFS) Ioctl(_ context.Context, _ server.Handle, _ uint32, _ []byte, _ uint32) ([]byte, uint32, error) {
	return nil, smb.StatusNotSupported, nil
}

// fileInfoFromOS 把 os.FileInfo 转成 VFS 的 FileInfo（只用于根目录的虚拟项）。
func fileInfoFromOS(name string, st os.FileInfo) server.FileInfo {
	return server.FileInfo{
		Name:           name,
		FileID:         scopedStableFileID(name),
		Size:           0,
		AllocationSize: 0,
		Attributes:     smb.FileAttrDirectory,
		CreationTime:   st.ModTime(),
		LastAccessTime: st.ModTime(),
		LastWriteTime:  st.ModTime(),
		ChangeTime:     st.ModTime(),
	}
}

// scopedStableFileID 给虚拟目录项一个稳定的非 0 FileID（FNV-1a）。
// 同一文件夹每次枚举都要拿到同一个值：Windows 用 FileId 判「是不是同一个对象」，
// 全 0 会让它把几个不同文件夹当成一个。
func scopedStableFileID(name string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	id := h.Sum64()
	if id == 0 {
		id = 1
	}
	return id
}

// scopedUserIsScoped 判断某用户是不是受限账号（admin 永远不是）。
func scopedUserIsScoped(user string) bool {
	_, ok := scopedFoldersFor(user)
	return ok
}
