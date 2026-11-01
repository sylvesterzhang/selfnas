package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/webdav"
)

// WebDAV：让「现成的 App」来干上传/播放这件事。
//
// 为什么要它：浏览器里传大文件，屏幕一黑 JS 就被系统挂起，传到一半停住
// （wakeLock 只能保「亮屏不动」，熄屏谁也救不了）。WebDAV 把传输交给
// 操作系统——iOS「文件」App、安卓 Solid Explorer、电脑资源管理器都是
// OS 级后台任务，锁屏照传。顺带 Infuse / VLC 也能直接挂这个地址播。
//
// 路径约定：/dav/<根目录名>/<相对路径>，根名就是管理台里配的名字（资料/软件/文档…），
// 同时接受盘符别名（D: / d: / D）。所有路径都过 resolve()，越界直接拒。

// davRootFor 按「根名」或「盘符」找根
func davRootFor(key string) (Root, bool) {
	if key == "" {
		return Root{}, false
	}
	for _, r := range cfg.Roots {
		if !r.Enabled {
			continue
		}
		if strings.EqualFold(r.Name, key) {
			return r, true
		}
	}
	// 盘符别名：有的客户端会硬写 D:，这里认一下
	k := strings.TrimSuffix(strings.ToUpper(key), ":")
	if k == "" {
		return Root{}, false
	}
	for _, r := range cfg.Roots {
		if !r.Enabled {
			continue
		}
		d := strings.TrimSuffix(strings.ToUpper(filepath.VolumeName(r.Path)), ":")
		if d != "" && d == k {
			return r, true
		}
	}
	return Root{}, false
}

// davCleaned 去掉前导 /、收敛 ..、去掉尾随 /
func davCleaned(name string) string {
	c := path.Clean("/" + name)
	if c == "/" {
		return ""
	}
	return strings.TrimPrefix(c, "/")
}

// davAbs WebDAV 路径 → 绝对路径 + 所属根。越界/根不存在都返回错误。
func davAbs(name string) (string, Root, error) {
	c := davCleaned(name)
	if c == "" {
		return "", Root{}, errOutside
	}
	key, rel := c, ""
	if i := strings.IndexByte(c, '/'); i >= 0 {
		key, rel = c[:i], c[i+1:]
	}
	rt, ok := davRootFor(key)
	if !ok {
		return "", Root{}, os.ErrNotExist
	}
	// 复用 resolve：它已经做了「不能逃出根目录」的判定
	return resolve(rt.Path, rel)
}

func davErr(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errOutside) {
		return &os.PathError{Op: "dav", Path: name, Err: os.ErrPermission}
	}
	return &os.PathError{Op: "dav", Path: name, Err: err}
}

func isWriteFlag(flag int) bool {
	return flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
}

// ---------- 虚拟根目录：/dav/ 上列出所有启用中的根 ----------
//
// 客户端（iOS「文件」）连上 /dav/ 第一件事就是 PROPFIND 列目录，
// 所以这里必须给一个「假但规矩」的目录对象，把各个根当子目录抛出去。

type davDirInfo struct{ name string }

func (d davDirInfo) Name() string       { return d.name }
func (d davDirInfo) Size() int64        { return 0 }
func (d davDirInfo) Mode() os.FileMode  { return os.ModeDir | 0o555 }
func (d davDirInfo) ModTime() time.Time { return time.Time{} }
func (d davDirInfo) IsDir() bool        { return true }
func (d davDirInfo) Sys() any           { return nil }

type davRootDir struct {
	names []string
	pos   int
}

func (d *davRootDir) Close() error { return nil }
func (d *davRootDir) Read([]byte) (int, error) {
	return 0, &os.PathError{Op: "read", Path: "/", Err: errors.New("是目录")}
}
func (d *davRootDir) Seek(int64, int) (int64, error) {
	return 0, &os.PathError{Op: "seek", Path: "/", Err: errors.New("是目录")}
}
func (d *davRootDir) Write([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: "/", Err: os.ErrPermission}
}
func (d *davRootDir) Readdir(count int) ([]fs.FileInfo, error) {
	if davDebug {
		log.Printf("[scoped-dav] Readdir count=%d names=%v pos=%d", count, d.names, d.pos)
	}
	var out []fs.FileInfo
	for d.pos < len(d.names) && (count <= 0 || len(out) < count) {
		out = append(out, davDirInfo{name: d.names[d.pos]})
		d.pos++
	}
	if len(out) == 0 {
		return nil, io.EOF
	}
	return out, nil
}
func (d *davRootDir) Stat() (fs.FileInfo, error) { return davDirInfo{name: "/"}, nil }

// davRootNames 只在列根目录时用：名字 = 管理台里配的根名
func davRootNames() []string {
	var out []string
	for _, r := range cfg.Roots {
		if r.Enabled {
			out = append(out, r.Name)
		}
	}
	return out
}

// davScopedRootNames 受限账号虚拟根的一级目录名（白名单展示名，排序保证稳定）。
// ⚠️ folders 是「真实路径 → 展示名」，要的是展示名（value）；取 key 会把
// D:\xxx 当成名字，walkFS 对它 Stat 时匹配不上白名单，整个列表就空了。
func davScopedRootNames(folders map[string]string) []string {
	out := make([]string, 0, len(folders))
	for _, name := range folders {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ---------- 文件系统实现 ----------

type davFS struct{}

func (davFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	if folders, scoped := davScopedFoldersOf(ctx); scoped {
		// 受限账号：根目录是虚拟的不能写；只读文件夹里也不能建
		if davCleaned(name) == "" || davScopedReadonly(ctx, name) {
			return &os.PathError{Op: "mkdir", Path: name, Err: os.ErrPermission}
		}
		full, err := davScopedAbs(folders, name)
		if err != nil {
			return davErr(name, err)
		}
		return os.Mkdir(full, perm)
	}
	if davCleaned(name) == "" {
		return &os.PathError{Op: "mkdir", Path: name, Err: os.ErrPermission}
	}
	full, rt, err := davAbs(name)
	if err != nil {
		return davErr(name, err)
	}
	if rt.Readonly {
		return &os.PathError{Op: "mkdir", Path: name, Err: os.ErrPermission}
	}
	return os.Mkdir(full, perm)
}

// davRootOpen 包一层根目录句柄：只改 Stat 报出去的名字。
// x/net/webdav 的 PROPFIND 是先 OpenFile 再对句柄 Stat()，
// 所以只在 Stat() 里改名不够——直接 stat "D:\" 拿到的 Name() 是个反斜杠。
type davRootOpen struct {
	*os.File
	name string
}

func (d *davRootOpen) Stat() (fs.FileInfo, error) { return davDirInfo{name: d.name}, nil }

// davDebug 受限账号 DAV 视图的调试开关（SELFNAS_DAV_DEBUG=1）。
var davDebug = os.Getenv("SELFNAS_DAV_DEBUG") == "1"

func (davFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	if folders, scoped := davScopedFoldersOf(ctx); scoped {
		if davDebug {
			log.Printf("[scoped-dav] OpenFile user=%q name=%q scoped=%v folders=%v", davUserOf(ctx), name, scoped, folders)
		}
		c := davCleaned(name)
		if c == "" {
			// 虚拟根：可列不可写
			if isWriteFlag(flag) {
				return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrPermission}
			}
			return &davRootDir{names: davScopedRootNames(folders)}, nil
		}
		// 这个文件夹被设成只读：任何写意图都拒（PUT/建文件/截断）
		if isWriteFlag(flag) && davScopedReadonly(ctx, name) {
			if davDebug {
				log.Printf("[scoped-dav] OpenFile 拒写：user=%q name=%q 是只读文件夹", davUserOf(ctx), name)
			}
			return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrPermission}
		}
		full, err := davScopedAbs(folders, name)
		if err != nil {
			return nil, davErr(name, err)
		}
		f, err := os.OpenFile(full, flag, perm)
		if err != nil {
			return nil, err
		}
		// 白名单文件夹那一层：名字换成展示名
		if !strings.Contains(c, "/") {
			return &davRootOpen{File: f, name: c}, nil
		}
		return f, nil
	}
	c := davCleaned(name)
	if c == "" {
		// 根目录是只读的虚拟层：可以列，不能写
		if isWriteFlag(flag) {
			return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrPermission}
		}
		return &davRootDir{names: davRootNames()}, nil
	}
	full, rt, err := davAbs(name)
	if err != nil {
		return nil, davErr(name, err)
	}
	// 只读根：任何写意图都拒（客户端会显示「目标不可写」，比静默失败好）
	if rt.Readonly && isWriteFlag(flag) {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrPermission}
	}
	f, err := os.OpenFile(full, flag, perm)
	if err != nil {
		return nil, err
	}
	// 根那一层：把名字换成管理台里配的根名，别让客户端显示成 "\"
	if !strings.Contains(c, "/") {
		return &davRootOpen{File: f, name: c}, nil
	}
	return f, nil
}

func (davFS) RemoveAll(ctx context.Context, name string) error {
	if folders, scoped := davScopedFoldersOf(ctx); scoped {
		if davCleaned(name) == "" || davScopedReadonly(ctx, name) {
			return &os.PathError{Op: "removeall", Path: name, Err: os.ErrPermission}
		}
		full, err := davScopedAbs(folders, name)
		if err != nil {
			return davErr(name, err)
		}
		return os.RemoveAll(full)
	}
	if davCleaned(name) == "" {
		return &os.PathError{Op: "removeall", Path: name, Err: os.ErrPermission}
	}
	full, rt, err := davAbs(name)
	if err != nil {
		return davErr(name, err)
	}
	if rt.Readonly {
		return &os.PathError{Op: "removeall", Path: name, Err: os.ErrPermission}
	}
	return os.RemoveAll(full)
}

func (davFS) Rename(ctx context.Context, oldName, newName string) error {
	if folders, scoped := davScopedFoldersOf(ctx); scoped {
		// 源和目标都要判：从只读文件夹里搬走、搬进只读文件夹，都是写
		if davCleaned(oldName) == "" || davCleaned(newName) == "" ||
			davScopedReadonly(ctx, oldName) || davScopedReadonly(ctx, newName) {
			return &os.PathError{Op: "rename", Path: oldName, Err: os.ErrPermission}
		}
		oldFull, err := davScopedAbs(folders, oldName)
		if err != nil {
			return davErr(oldName, err)
		}
		newFull, err := davScopedAbs(folders, newName)
		if err != nil {
			return davErr(newName, err)
		}
		return os.Rename(oldFull, newFull)
	}
	if davCleaned(oldName) == "" || davCleaned(newName) == "" {
		return &os.PathError{Op: "rename", Path: oldName, Err: os.ErrPermission}
	}
	oldFull, oldRoot, err := davAbs(oldName)
	if err != nil {
		return davErr(oldName, err)
	}
	newFull, newRoot, err := davAbs(newName)
	if err != nil {
		return davErr(newName, err)
	}
	// 跨根移动不接：两块盘之间走 os.Rename 会直接失败，不如在这里说清楚
	if !strings.EqualFold(normPath(oldRoot.Path), normPath(newRoot.Path)) {
		return &os.PathError{Op: "rename", Path: oldName, Err: errors.New("不支持跨根目录移动")}
	}
	if oldRoot.Readonly || newRoot.Readonly {
		return &os.PathError{Op: "rename", Path: oldName, Err: os.ErrPermission}
	}
	return os.Rename(oldFull, newFull)
}

func (davFS) Stat(ctx context.Context, name string) (fs.FileInfo, error) {
	if folders, scoped := davScopedFoldersOf(ctx); scoped {
		c := davCleaned(name)
		if c == "" {
			return davDirInfo{name: "/"}, nil
		}
		full, err := davScopedAbs(folders, name)
		if davDebug {
			log.Printf("[scoped-dav] Stat user=%q name=%q c=%q full=%q err=%v", davUserOf(ctx), name, c, full, err)
		}
		if err != nil {
			return nil, davErr(name, err)
		}
		fi, err := os.Stat(full)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(c, "/") && fi.IsDir() {
			return davDirInfo{name: c}, nil
		}
		return fi, nil
	}
	c := davCleaned(name)
	if c == "" {
		return davDirInfo{name: "/"}, nil
	}
	full, _, err := davAbs(name)
	if err != nil {
		return nil, davErr(name, err)
	}
	fi, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	// 根目录本身：os.Stat("D:\") 的 Name() 是 "\"，客户端会把盘显示成一个反斜杠，
	// 这里换成管理台里配的名字（资料 / 软件 / 文档）
	if !strings.Contains(c, "/") && fi.IsDir() {
		return davDirInfo{name: c}, nil
	}
	return fi, nil
}

// ---------- 处理器：Basic 认证 + 日志 ----------

// davUserCtx 在请求上下文里带「当前 DAV 用户名」。
// 受限账号（scopedAccounts）和 admin 走同一套 davFS，但视角不同：
// admin 看到所有启用的根，受限账号只看到自己白名单文件夹的虚拟根。
type davUserCtx struct{}

func davUserOf(ctx context.Context) string {
	u, _ := ctx.Value(davUserCtx{}).(string)
	return u
}

// davScopedFoldersOf 取当前请求用户的白名单（真实路径 → 展示名，scopedNameFor 的口径）。
// 不是受限账号返回 false。
func davScopedFoldersOf(ctx context.Context) (map[string]string, bool) {
	folders, ok := scopedFoldersFor(davUserOf(ctx))
	if !ok {
		return nil, false
	}
	return scopedNameFor(folders), true
}

// davScopedReadonly 判断受限账号这次访问落在哪个白名单文件夹上、那个文件夹是不是只读。
// 虚拟根（第一段为空）恒不可写。名字匹配走展示名（和 davScopedAbs 同一套口径）。
func davScopedReadonly(ctx context.Context, name string) bool {
	folders, ok := scopedFoldersFor(davUserOf(ctx))
	if !ok {
		return false
	}
	c := davCleaned(name)
	if c == "" {
		return true // 虚拟根：可列不可写
	}
	first := c
	if i := strings.IndexByte(c, '/'); i >= 0 {
		first = c[:i]
	}
	for real, disp := range scopedNameFor(folders) {
		if strings.EqualFold(disp, first) {
			return scopedReadonlyFor(folders, real)
		}
	}
	return false
}

// davScopedAbs 把受限账号的 dav 路径（"电影/x/y"）解析成绝对路径。
// 第一段必须是白名单展示名（大小写不敏感），余下部分拼在真实路径上，
// 再用 scopedWithin 防越界——不能借 resolve()：它先 findRoot 查根目录，
// 白名单子目录不是根，会直接 errOutside（踩过：列表永远是空的）。
func davScopedAbs(folders map[string]string, name string) (string, error) {
	c := davCleaned(name)
	if c == "" {
		return "", errOutside
	}
	key, rel := c, ""
	if i := strings.IndexByte(c, '/'); i >= 0 {
		key, rel = c[:i], c[i+1:]
	}
	for real, disp := range folders {
		// ⚠️ folders 是「真实路径 → 展示名」：real 是 key、disp 是 value。
		// 之前写成 for disp, real，把 D:\xxx 当展示名比，永远匹配不上。
		if strings.EqualFold(disp, key) {
			full := filepath.Clean(filepath.Join(real, filepath.FromSlash(rel)))
			if !scopedWithin(full, real) {
				return "", os.ErrNotExist
			}
			return full, nil
		}
	}
	return "", os.ErrNotExist
}

// davAuth WebDAV 客户端不会带 cookie，所以这里以 Basic 认证为主；
// 浏览器里已经登过的（带着 sn_token）也放行，方便调试。
// 认证通过后把用户名塞进 context：受限账号在 davFS 里走白名单视图。
func davAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cookie 会话：网页端登录过的浏览器。用户可能是 admin 也可能是
		// 受限账号（它们现在能登文件站网页端）——按令牌里的用户分发视角，
		// 绝不能见到合法 Cookie 就给 admin 全盘。authedRenew 顺带续签。
		authed := authedRenew(w, r)
		if u := webUserOf(r); authed && u != "" {
			if u != cfg.User && !scopedUserIsScoped(u) {
				// 用户已被删除：令牌理论上过不了 tokenUser，这里再兜一层
				http.Error(w, "需要登录（账号同管理台）", http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), davUserCtx{}, u)))
			return
		}
		u, p, ok := r.BasicAuth()
		switch {
		case ok && strings.EqualFold(u, cfg.User) && verifyPassword(p, cfg.PassHash):
			if davDebug {
				log.Printf("[dav-auth] Basic admin user=%q", u)
			}
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), davUserCtx{}, cfg.User)))
		case ok && scopedVerify(u, p):
			if davDebug {
				log.Printf("[dav-auth] Basic 受限 user=%q", u)
			}
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), davUserCtx{}, scopedNormalizeUser(u))))
		default:
			// iOS「文件」/安卓文件管理器都靠这个头判断要不要弹账号密码框
			if davDebug {
				log.Printf("[dav-auth] Basic 拒绝 user=%q ok=%v", u, ok)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="selfnas", charset="UTF-8"`)
			http.Error(w, "需要登录（账号同管理台）", http.StatusUnauthorized)
		}
	})
}

// davLog 记一笔成功/失败，出问题好查（PROPFIND 太吵，不记）
type davLogWriter struct {
	http.ResponseWriter
	code int
}

func (w *davLogWriter) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}

func davLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lw := &davLogWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(lw, r)
		if r.Method == "PROPFIND" || r.Method == "OPTIONS" {
			return // 列目录太吵，只记真正动了文件的操作
		}
		log.Printf("[dav] %s %s %s %s -> %d", clientIP(r), uaTag(r.UserAgent()), r.Method, r.URL.Path, lw.code)
	})
}

// davReadonly 判断一次请求是否要写只读根。
// 为什么要前置拦：x/net/webdav 的 handlePut 把所有非「不存在」的错误一律回 404，
// 只读根在客户端会显示成「文件找不到」而不是「没有权限」，看着像 bug。
var davWriteMethods = map[string]bool{
	"PUT": true, "DELETE": true, "MKCOL": true, "MOVE": true, "COPY": true,
	"PROPPATCH": true, "LOCK": true,
}

func davRootOfPath(p string) (Root, bool) {
	c := davCleaned(p)
	if c == "" {
		return Root{}, false
	}
	key := c
	if i := strings.IndexByte(c, '/'); i >= 0 {
		key = c[:i]
	}
	return davRootFor(key)
}

// davRejectWrite 回绝一个写请求，并保证客户端真的能收到这个「拒绝」。
//
// 为什么不能 http.Error 了事：客户端 PUT 大文件时是「边传 body 边等响应」。
// 服务端一个字节都不读就关连接，TCP 会发 RST，**接收缓冲区里刚发出的 403
// 会跟着一起被丢掉**，客户端只能看到「连接被重置」——于是既不提示权限不足，
// 也不说为什么失败，表现就是「传不上去但也不报错」。
//
// 实测（裸 socket 模拟客户端，对只读文件夹 PUT）：
//
//	5 MB  → 只发出 0.3 MB 就 ConnectionAborted，403 读不到
//	200 KB → 全部发出，稳稳收到 403
//
// 分界线正好是 Go 的 maxPostHandlerReadBytes(256KB)：body 超过它，Go 判定
// 「不值得读完再复用连接」，直接关 socket。nginx 的 lingering_close 就是为
// 这件事存在的。
//
// 所以顺序必须是：先把响应推出去 → 再把请求体读干净 → 最后才让连接关闭。
func davRejectWrite(w http.ResponseWriter, r *http.Request, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// 明确告诉客户端这条连接到此为止，别指望复用
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, msg+"\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush() // 先送达，再慢慢收尾
	}
	// 读完请求体（上限 15s）。局域网 15s 够传 600MB 左右，再大就断开——
	// 那时客户端多半已经在等响应了，代价比「静默失败」小得多。
	if r.Body != nil {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = io.Copy(io.Discard, r.Body)
		}()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}
		_ = r.Body.Close()
	}
}

func davGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if davWriteMethods[r.Method] {
			// MOVE/COPY 还要看目标
			targets := []string{r.URL.Path}
			if d := r.Header.Get("Destination"); d != "" {
				if u, err := url.Parse(d); err == nil {
					targets = append(targets, u.Path)
				}
			}
			for _, t := range targets {
				p := strings.TrimPrefix(t, "/dav")
				if rt, ok := davRootOfPath(p); ok && rt.Readonly {
					davRejectWrite(w, r, "「"+rt.Name+"」是只读根目录，不能写入（到管理台把只读关掉再试）")
					return
				}
				// 受限账号：白名单文件夹可能单独设了只读。
				// 这里必须前置拦——x/net/webdav 的 handlePut 把 OpenFile 的
				// permission denied 也映射成 404（见上面注释），客户端会显示「文件找不到」
				if scopedUserIsScoped(davUserOf(r.Context())) && davScopedReadonly(r.Context(), p) {
					davRejectWrite(w, r, "这个文件夹是只读的，不能写入（到管理台把权限改成可读写再试）")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// mountWebDAV 注册 /dav/ 路由。handler 自带 PROPFIND/PUT/MOVE/LOCK 全套语义。
func mountWebDAV(mux *http.ServeMux) {
	h := &webdav.Handler{
		Prefix:     "/dav",
		FileSystem: davFS{},
		LockSystem: webdav.NewMemLS(), // 备忘录锁：Windows 资源管理器/Office 要用
		Logger: func(r *http.Request, err error) {
			if err != nil {
				log.Printf("[dav] %s %s 出错：%v", clientIP(r), r.URL.Path, err)
			}
		},
	}
	mux.HandleFunc("/dav", func(w http.ResponseWriter, r *http.Request) {
		// 不带斜杠的 /dav 会被 webdav.Handler 判成非法路径，直接带回斜杠
		http.Redirect(w, r, "/dav/", http.StatusMovedPermanently)
	})
	mux.Handle("/dav/", davLog(davAuth(davGuard(h))))
}
