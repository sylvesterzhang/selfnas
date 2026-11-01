package main

// 可开关的 VFS 外壳。
//
// 背景：关掉一个根目录之后，「看不到那个盘」必须对**已经挂载它的客户端**也生效；
// 而重新打开之后，又必须能原地恢复。这两件事把可选方案挤得很窄：
//
//	把 tree 从会话里删掉（最早的写法）
//	  → 客户端手里那个 TreeID 永久失效，它拿到 STATUS_NETWORK_NAME_DELETED 之后
//	    **不会自己重新 TREE_CONNECT**，只会一直拿同一个 TreeID 重试。
//	    iOS 上的表现就是：关掉再打开，那个位置永远「内容不可用」。
//	    日志里能看到重新打开之后 CREATE 还在返回 0xC00000C9。
//
//	重启整个服务（更早的写法）
//	  → 把所有连接都踢了，iOS 被踢几次之后干脆不再问服务端，缓存住「不可用」。
//
// 所以正确做法是 tree 和连接都不动，只切换 tree 背后指向的 VFS：
// 关掉时把 inner 摘掉（所有操作一律 ACCESS_DENIED），打开时装回去。
// 同一个 TreeID 全程有效，客户端不需要重新挂载。
//
// 关掉的那一刻要顺手把之前打开的文件句柄关掉，否则会一直占着文件锁；
// 句柄是本外壳发出去的，所以它自己记得，不需要外部帮忙。

import (
	"context"
	"sync"

	"github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"
)

// smbGate 包住一个真实的 VFS，允许在运行中把它「关掉」和「打开」。
// 它实现 server.VFS，所以可以直接塞进 Share.VFS；由于 Share 是按值存进
// tree 的、而 VFS 是接口（内部指向同一个 *smbGate），已存在的 tree 也能感知开关。
type smbGate struct {
	mu    sync.RWMutex
	root  string
	inner server.VFS // nil = 这个盘当前是关闭状态

	hmu     sync.Mutex
	handles map[server.Handle]struct{} // 本外壳发出去的句柄，关盘时要收干净
}

func newSmbGate(root string) *smbGate {
	return &smbGate{root: root, handles: make(map[server.Handle]struct{})}
}

// current 返回当前生效的 VFS（可能是 nil）。调用方拿到之后立刻用，
// 不要持有太久——切换盘的那一刻句柄会被收掉。
func (g *smbGate) current() server.VFS {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.inner
}

// setInner 换掉背后的 VFS。传 nil 表示关盘。
// 关盘时先把已发出的句柄全部关掉（用**旧的** inner 去关），避免文件被一直占着。
func (g *smbGate) setInner(v server.VFS) {
	g.mu.Lock()
	old := g.inner
	g.inner = v
	g.mu.Unlock()

	if old == nil || old == v {
		return
	}
	g.hmu.Lock()
	hs := make([]server.Handle, 0, len(g.handles))
	for h := range g.handles {
		hs = append(hs, h)
	}
	g.handles = make(map[server.Handle]struct{})
	g.hmu.Unlock()
	for _, h := range hs {
		_ = old.Close(context.Background(), h)
	}
}

func (g *smbGate) track(h server.Handle) {
	if h == nil {
		return
	}
	g.hmu.Lock()
	g.handles[h] = struct{}{}
	g.hmu.Unlock()
}

func (g *smbGate) untrack(h server.Handle) (known bool) {
	g.hmu.Lock()
	defer g.hmu.Unlock()
	if _, ok := g.handles[h]; ok {
		delete(g.handles, h)
		return true
	}
	return false
}

// closedStatus 是「这个盘现在关着」时统一返回的状态。
// 用 ACCESS_DENIED 而不是 NETWORK_NAME_DELETED：后者会让客户端以为共享没了、
// 进而把整个挂载判死；前者只是这一次访问失败，客户端保留挂载，盘一打开就能恢复。
const closedStatus = smb.StatusAccessDenied

// ---- server.VFS 的十一个方法，全部转发；关盘时一律拒绝 ----

func (g *smbGate) Create(ctx context.Context, sess *server.Session, req server.CreateRequest) (server.CreateResult, uint32, error) {
	v := g.current()
	if v == nil {
		return server.CreateResult{}, closedStatus, nil
	}
	res, status, err := v.Create(ctx, sess, req)
	if status == 0 && err == nil {
		g.track(res.Handle)
	}
	return res, status, err
}

func (g *smbGate) Close(ctx context.Context, h server.Handle) error {
	g.untrack(h)
	v := g.current()
	if v == nil {
		// 关盘时句柄已经被 setInner 收掉了，这里当成功，让客户端安静地关掉
		return nil
	}
	return v.Close(ctx, h)
}

func (g *smbGate) Read(ctx context.Context, h server.Handle, offset int64, buf []byte) (int, uint32, error) {
	v := g.current()
	if v == nil {
		return 0, closedStatus, nil
	}
	return v.Read(ctx, h, offset, buf)
}

func (g *smbGate) Write(ctx context.Context, h server.Handle, offset int64, data []byte) (int, uint32, error) {
	v := g.current()
	if v == nil {
		return 0, closedStatus, nil
	}
	return v.Write(ctx, h, offset, data)
}

func (g *smbGate) Flush(ctx context.Context, h server.Handle) (uint32, error) {
	v := g.current()
	if v == nil {
		return closedStatus, nil
	}
	return v.Flush(ctx, h)
}

func (g *smbGate) QueryFileInfo(ctx context.Context, h server.Handle, infoClass byte) (any, uint32, error) {
	v := g.current()
	if v == nil {
		return nil, closedStatus, nil
	}
	return v.QueryFileInfo(ctx, h, infoClass)
}

func (g *smbGate) SetFileInfo(ctx context.Context, h server.Handle, infoClass byte, raw []byte) (uint32, error) {
	v := g.current()
	if v == nil {
		return closedStatus, nil
	}
	return v.SetFileInfo(ctx, h, infoClass, raw)
}

func (g *smbGate) QueryDirectory(ctx context.Context, h server.Handle, pattern string, restart bool) ([]server.DirEntry, uint32, error) {
	v := g.current()
	if v == nil {
		return nil, closedStatus, nil
	}
	return v.QueryDirectory(ctx, h, pattern, restart)
}

func (g *smbGate) QueryFSInfo(ctx context.Context, infoClass byte) (any, uint32, error) {
	v := g.current()
	if v == nil {
		return nil, closedStatus, nil
	}
	return v.QueryFSInfo(ctx, infoClass)
}

func (g *smbGate) QuerySecurity(ctx context.Context, h server.Handle, addInfo uint32) ([]byte, uint32, error) {
	v := g.current()
	if v == nil {
		return nil, closedStatus, nil
	}
	return v.QuerySecurity(ctx, h, addInfo)
}

func (g *smbGate) Ioctl(ctx context.Context, h server.Handle, code uint32, in []byte, maxOut uint32) ([]byte, uint32, error) {
	v := g.current()
	if v == nil {
		return nil, closedStatus, nil
	}
	return v.Ioctl(ctx, h, code, in, maxOut)
}
