package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	mssrvs "github.com/jfjallid/go-smb/dcerpc/mssrvs"
	mssrvsserver "github.com/jfjallid/go-smb/dcerpc/mssrvs/server"
	dcerpcserver "github.com/jfjallid/go-smb/dcerpc/server"
	"github.com/jfjallid/go-smb/ntlmssp"
	"github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"
	"github.com/jfjallid/go-smb/smb/server/filevfs"
	"github.com/jfjallid/golog"
)

// SmbConfig 是 config.json 里的 "smb" 段。
//
// 为什么单独存一个 NT hash：SMB 的 NTLM 认证要求服务器持有
// MD4(UTF-16LE(密码))，而网页登录只存 PBKDF2，算不出 MD4。
// 所以启用 SMB 时让用户重输一次密码，校验通过后把 NT hash 存下来；
// 改密码时同步重算。
type SmbConfig struct {
	Enabled bool   `json:"enabled"`
	Port    int    `json:"port"`   // 默认 445；iOS 只认 445，别改
	NTHash  string `json:"ntHash"` // hex(MD4(UTF-16LE(密码)))
	// TookOver 记「系统自带的 SMB 是不是被我们关掉了」。
	// 有这个标记才能在关闭 SMB 时准确地把 Windows 的共享还回去，
	// 也才能在还回去失败时说清「系统共享现在是关的，点一下恢复」。
	TookOver bool `json:"tookOver"`
	// GUID 是服务端标识（NegotiateRes 里的 ServerGuid）。
	// **必须持久化**：客户端会把它当成「这台机器」的身份证，
	// 每次重启都换一个的话，iOS 那边的旧挂载会认为「服务器换了个人」而报错。
	GUID string `json:"guid"`
}

const smbDefaultPort = 445

var (
	smbMu        sync.Mutex
	smbRunning   bool
	smbSrv       *server.Server
	smbErr       string
	smbPort      int
	smbReloading bool // 正在重建共享：这期间状态里仍报 running，别让管理台闪一下「没跑起来」

	// 根目录 → 可开关的 VFS 外壳。必须跨多次重建复用（见 smb_gate.go）
	smbGateMu sync.Mutex
	smbGates  = map[string]*smbGate{}

	// SMB 认证器（可热刷新，见 smbDynamicAuth）
	smbAuthCred = &smbDynamicAuth{}
)

// ntHashOf 计算 NT hash（MS-NLMP 规定就是 MD4(UTF-16LE(pwd))）
func ntHashOf(pass string) string {
	return hex.EncodeToString(ntlmssp.Ntowfv1(pass))
}

// smbShareName 清掉 SMB 共享名里的非法字符（根目录名是用户随便起的）
func smbShareName(name string) string {
	bad := `\/:*?"<>|`
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if strings.ContainsRune(bad, r) {
			out = append(out, '_')
			continue
		}
		out = append(out, r)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		s = "share"
	}
	if len([]rune(s)) > 60 {
		s = string([]rune(s)[:60])
	}
	return s
}

// smbGateFor 取（或建）某个根目录对应的「可开关 VFS」。
//
// 同一个根目录在所有共享表重建里必须复用同一个 *smbGate 实例：客户端已经挂载的
// tree 里存的是 Share 的值拷贝，但它持有的 VFS 接口指向这个实例——
// 只有实例不变，「关盘 / 开回来」才能对老连接立刻生效（详见 smb_gate.go）。
func smbGateFor(root string) (*smbGate, error) {
	key := strings.ToLower(root)
	smbGateMu.Lock()
	defer smbGateMu.Unlock()
	if g := smbGates[key]; g != nil {
		if g.current() != nil {
			return g, nil // 一直在开着，原样复用
		}
		g.root = root
		return g, nil
	}
	g := newSmbGate(root)
	smbGates[key] = g
	return g, nil
}

// smbFillGates 给还没有 inner 的 gate 装上真实 VFS。
// 关闭状态的门（inner == nil）不在这里恢复 —— 它要么本轮被重新打开，
// 要么由 smbCloseGates 保持关闭。
func smbFillGates(shares []server.Share) error {
	for _, sh := range shares {
		g, ok := sh.VFS.(*smbGate)
		if !ok || g.current() != nil {
			continue
		}
		opts := filevfs.Options{Root: g.root}
		// readonly 是根目录自己的配置，按 root 反查一下
		for _, r := range cfg.Roots {
			if strings.EqualFold(r.Path, g.root) {
				opts.ReadOnly = r.Readonly
				break
			}
		}
		fs, err := filevfs.New(opts)
		if err != nil {
			return err
		}
		g.setInner(fs)
	}
	return nil
}

// smbCloseGates 把这一轮不在 active 里的根目录全部关上。
// 注意是「关门」而不是「删 tree」：tree 留着，客户端那条挂载才不会废掉。
func smbCloseGates(active map[string]bool) []string {
	smbGateMu.Lock()
	gates := make(map[string]*smbGate, len(smbGates))
	for k, v := range smbGates {
		gates[k] = v
	}
	smbGateMu.Unlock()

	var closed []string
	for k, g := range gates {
		if active[k] || g.current() == nil {
			continue
		}
		g.setInner(nil)
		closed = append(closed, g.root)
	}
	return closed
}

// smbBuildShares 按 config 里的根目录生成共享列表
func smbBuildShares() ([]server.Share, []string, error) {
	shares := make([]server.Share, 0, len(cfg.Roots))
	names := make([]string, 0, len(cfg.Roots))
	seen := map[string]bool{}
	for _, r := range cfg.Roots {
		if !r.Enabled {
			continue
		}
		if _, err := os.Stat(r.Path); err != nil {
			log.Printf("[smb] 跳过根 %q：%v", r.Name, err)
			continue
		}
		name := smbShareName(r.Name)
		for i := 2; seen[strings.ToLower(name)]; i++ {
			name = fmt.Sprintf("%s_%d", smbShareName(r.Name), i)
		}
		seen[strings.ToLower(name)] = true
		g, err := smbGateFor(r.Path)
		if err != nil {
			log.Printf("[smb] 根 %q 挂载失败：%v", r.Name, err)
			continue
		}
		shares = append(shares, server.Share{
			Name:          name,
			Type:          smb.ShareTypeDisk,
			Remark:        "selfnas",
			VFS:           g,
			MaximalAccess: 0x001f01ff,
		})
		names = append(names, name)
	}
	// 「共享」：受限账号唯一的入口，根目录是各账号白名单文件夹的虚拟合并视图。
	// admin 不走它（TreeConnect 钩子里拒掉），共享枚举也按用户过滤。
	// 注意即使一个根目录都没有（全部禁用），「共享」也要注册——
	// 受限账号可能配了别的盘上的白名单文件夹。
	shares = append(shares, server.Share{
		Name:          scopedShareName,
		Type:          smb.ShareTypeDisk,
		Remark:        "selfnas",
		VFS:           scopedVFS{},
		MaximalAccess: 0x001f01ff,
	})
	names = append(names, scopedShareName)
	if err := smbFillGates(shares); err != nil {
		return nil, nil, err
	}
	return shares, names, nil
}

// smbAccountsMap 组装 SMB 认证账号表：admin + 所有受限账号。
func smbAccountsMap(adminHash []byte) map[string]*server.Account {
	m := map[string]*server.Account{
		strings.ToLower(cfg.User): {NTHash: adminHash},
	}
	for _, a := range scopedAccountsFromCfg() {
		h, err := hex.DecodeString(a.NTHash)
		if err != nil || len(h) != 16 {
			continue
		}
		m[scopedNormalizeUser(a.User)] = &server.Account{NTHash: h}
	}
	return m
}

// smbDynamicAuth 可热刷新的认证器：后台增删受限账号后换掉内层账号表即可，
// 不用重启 SMB 服务（重启会踢掉所有已建立的连接，那是大忌）。
type smbDynamicAuth struct {
	mu    sync.RWMutex
	inner *server.MapAuthenticator
}

func (d *smbDynamicAuth) Verify(c *server.Conn, auth *ntlmssp.Authenticate, ch [8]byte) ([]byte, uint32) {
	d.mu.RLock()
	inner := d.inner
	d.mu.RUnlock()
	if inner == nil {
		return nil, smb.StatusLogonFailure
	}
	return inner.Verify(c, auth, ch)
}

// smbRefreshAuthAccounts 按当前配置重建账号表，立即生效。
func smbRefreshAuthAccounts() {
	hash, err := hex.DecodeString(cfg.SMB.NTHash)
	if err != nil || len(hash) != 16 {
		return // admin 凭据没就绪时维持现状
	}
	smbAuthCred.mu.Lock()
	smbAuthCred.inner = &server.MapAuthenticator{Accounts: smbAccountsMap(hash)}
	smbAuthCred.mu.Unlock()
}

// smbStart 启动内置 SMB 服务（幂等）
func smbStart() error {
	smbMu.Lock()
	defer smbMu.Unlock()
	if smbRunning {
		return nil
	}
	port := cfg.SMB.Port
	if port <= 0 {
		port = smbDefaultPort
	}
	if strings.TrimSpace(cfg.SMB.NTHash) == "" {
		return fmt.Errorf("还没有 SMB 的密码凭据，请重新输入一次登录密码")
	}
	hash, err := hex.DecodeString(cfg.SMB.NTHash)
	if err != nil || len(hash) != 16 {
		return fmt.Errorf("SMB 凭据已损坏，请重新输入一次登录密码")
	}
	// 账号表（admin + 受限账号）灌进热刷新认证器
	smbRefreshAuthAccounts()
	// 先探一下端口，抢不到时给出人话
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return smbListenHint(port, err)
	}
	ln.Close()

	shares, names, err := smbBuildShares()
	if err != nil {
		return err
	}

	scfg := &server.ServerConfig{
		ServerGUID:    smbServerGUID(),
		NetBIOSName:   "SELFNAS",
		NetBIOSDomain: "WORKGROUP",
		// 关签名：库对「复合请求」的签名校验是按单条报文算的，而规范要求覆盖整条链，
		// iOS 会打包 CREATE+QUERY_INFO，必然校验失败被踢。关掉签名后 iOS 正常；
		// 代价是局域网明文（跟 WebDAV 走 http 同级），外网必须套 HTTPS。
		SigningRequired: false,
		Authenticator:   smbAuthCred,
	}
	// 诊断钩子。手机连不上时，能拿到的唯一线索就是「哪条命令 / 返回什么状态码」，
	// 光靠客户端那句「无效的自变量」完全没法定位（EINVAL 可能对应服务端任何一个 NTSTATUS）。
	// 平时只记失败（正常请求太多），要看全过程就设 SELFNAS_SMB_DEBUG=1。
	dbg := os.Getenv("SELFNAS_SMB_DEBUG") != ""
	if dbg {
		// 库自己用 golog 输出（默认写 stdout），接进我们的日志文件才看得到
		golog.SetAll(golog.LevelDebug, log.LstdFlags, log.Writer(), log.Writer())
	}
	scfg.OnConnect = func(c *server.Conn) error {
		if dbg {
			log.Printf("[smb] 连入 %v", c.RemoteAddr)
		}
		return nil
	}
	scfg.OnTreeConnect = func(c *server.Conn, s *server.Session, share string, req *smb.TreeConnectReq, res *smb.TreeConnectRes) (*server.Status, error) {
		// 按账号门禁：受限账号只许开「共享」；admin 不许开「共享」
		//（那是受限账号的视图，admin 有全盘的根共享）。
		// IPC$ 必须放行：手机连上来第一步就是开 IPC$ 管道枚举共享列表
		//（srvsvc），把它拦了客户端直接报「连不上」；枚举结果本身在
		// OnShareEnumAll 里已按账号过滤，这里不用管。
		user := ""
		if s != nil {
			user = s.Username
		}
		if share == "IPC$" {
			return nil, nil
		}
		if scopedUserIsScoped(user) {
			if share != scopedShareName {
				log.Printf("[smb] %v 受限账号 %q 试图打开共享 %q → 拒绝", c.RemoteAddr, user, share)
				return &server.Status{Code: smb.StatusAccessDenied}, nil
			}
		} else if share == scopedShareName {
			return &server.Status{Code: smb.StatusAccessDenied}, nil
		}
		// 「客户端把共享判成只读」这类问题只能靠这几个字段自证清白：
		// ShareType 必须是 1(disk)、ShareFlags 不能带奇怪位、
		// MaximalAccess 里必须有写位（受限账号全只读时这里会是 0x00120089，那是故意的）。
		// reqCtx/回 ctx 是 SMB3.1.1 的 create context：非 0 说明客户端在问
		// Apple 扩展能力（AAPL）——那是 iOS 判「能不能写」的入口。
		// 这行不进 dbg 开关：每次挂载只一行，排「权限为什么不对」第一眼看的就是它。
		log.Printf("[smb] %v 打开共享 %q（type=%d flags=0x%08x caps=0x%08x maxAccess=0x%08x reqCtx=%d → ctx=%d）",
			c.RemoteAddr, share, res.ShareType, res.ShareFlags, res.Capabilities, res.MaximalAccess,
			len(req.Context), len(res.Context))
		return nil, nil
	}
	scfg.OnCredentialCaptured = func(c *server.Conn, cred *server.Credential) {
		if dbg {
			log.Printf("[smb] %v 认证尝试 user=%q domain=%q 格式=%s", c.RemoteAddr, cred.Username, cred.Domain, cred.Format)
		}
	}
	// CREATE 的路径名要从原始报文里自己挖（库没暴露钩子）：
	// 「点进某个文件夹就内容不可用」这种问题，不看名字根本不知道是哪个目录被拒
	scfg.OnRawRequest = func(c *server.Conn, raw []byte) (bool, error) {
		// 线级转储（SELFNAS_SMB_WIRE=1）。必须在 dbg 判断之前，
		// 否则「没开 debug 但要看报文」的场景就拿不到数据。
		smbWireDump(fmt.Sprint(c.RemoteAddr), ">>", raw)
		if !dbg || len(raw) < 64+56 {
			return false, nil
		}
		// 复合请求里每一段都有自己的头，逐段找 READ/WRITE：
		// 「视频打不开黑屏」这类问题必须知道它到底在对哪个句柄、什么偏移读多长。
		smbWalkCompound(raw, func(cmd uint16, body []byte) {
			switch cmd {
			case 0x08: // READ
				if len(body) >= 32 {
					log.Printf("[smb] %v READ 请求 fid=0x%04x off=%d len=%d",
						c.RemoteAddr,
						binary.LittleEndian.Uint16(body[16:18]),
						binary.LittleEndian.Uint64(body[8:16]),
						binary.LittleEndian.Uint32(body[4:8]))
				}
			case 0x09: // WRITE
				if len(body) >= 32 {
					log.Printf("[smb] %v WRITE 请求 fid=0x%04x off=%d len=%d",
						c.RemoteAddr,
						binary.LittleEndian.Uint16(body[16:18]),
						binary.LittleEndian.Uint64(body[8:16]),
						binary.LittleEndian.Uint32(body[4:8]))
				}
			}
		})
		if binary.LittleEndian.Uint16(raw[12:14]) != 0x05 { // SMB2 CREATE
			return false, nil
		}
		body := raw[64:]
		off := int(binary.LittleEndian.Uint16(body[44:46]))
		n := int(binary.LittleEndian.Uint16(body[46:48]))
		// create context：Apple 客户端靠它声明扩展能力（AAPL 等）。
		// 我们目前对不认识的上下文一律忽略，所以要先把「它问了什么」记下来。
		ctxOff := int(binary.LittleEndian.Uint32(body[48:52]))
		ctxLen := int(binary.LittleEndian.Uint32(body[52:56]))
		ctxDesc := ""
		if ctxLen > 0 && ctxOff > 0 && ctxOff+ctxLen <= len(raw) {
			ctxDesc = " context[" + describeCreateContexts(raw[ctxOff:ctxOff+ctxLen]) + "]"
		}
		if n == 0 || off <= 0 || off+n > len(raw) {
			log.Printf("[smb] %v CREATE <根目录>%s", c.RemoteAddr, ctxDesc)
			return false, nil
		}
		name := smbUTF16(raw[off : off+n])
		disp := binary.LittleEndian.Uint32(body[36:40])
		log.Printf("[smb] %v CREATE %q（disposition=%d）%s", c.RemoteAddr, name, disp, ctxDesc)
		return false, nil
	}
	scfg.OnRawResponse = func(c *server.Conn, raw []byte) ([]byte, error) {
		smbWireDump(fmt.Sprint(c.RemoteAddr), "<<", raw)
		if len(raw) < 64 {
			return raw, nil
		}
		st := binary.LittleEndian.Uint32(raw[8:12])
		cmd := binary.LittleEndian.Uint16(raw[12:14])
		// READ 应答补上实际读到的字节数：播放器卡住/黑屏时，
		// 「读到 0 字节/比要的少」一眼就能看出来。
		if dbg && cmd == 0x08 && len(raw) >= 64+8 {
			n := binary.LittleEndian.Uint32(raw[64+4 : 64+8])
			log.Printf("[smb] %v READ 应答 n=%d status=0x%08x", c.RemoteAddr, n, st)
		}
		// 0xC0000016 = NTLM 握手的中间态，不是错；0x80000006/0x8000001A
		// 是「列完了」，warning 级。都别当失败刷日志。
		if st != 0 && st != 0xc0000016 && smbStatusIsError(st) {
			log.Printf("[smb] %v 失败 %s status=0x%08x（%s）",
				c.RemoteAddr, smbCmdName(cmd), st, smbStatusName(st))
		} else if dbg {
			log.Printf("[smb] %v %s -> 0x%08x", c.RemoteAddr, smbCmdName(cmd), st)
		}
		return raw, nil
	}
	srv := &server.Server{Config: scfg}
	for _, sh := range shares {
		srv.RegisterShare(sh.Name, sh)
	}
	// 共享枚举（srvsvc）：iOS「文件」App 靠它列出共享列表。
	// 按账号过滤：受限账号只看到「共享」；admin 看到根共享、看不到「共享」。
	// 枚举句柄按会话建（PipeOpener 拿得到 Session），正好在这层分叉。
	scfg.PipeOpener = &server.MapPipeOpener{Pipes: map[string]func(*server.Session) (server.PipeBackend, error){
		"srvsvc": func(s *server.Session) (server.PipeBackend, error) {
			user := ""
			if s != nil {
				user = s.Username
			}
			svc := &mssrvsserver.Service{}
			svc.OnShareEnumAll = func(_ *mssrvs.NetShareEnumAllRequest, _ []mssrvsserver.ShareEntry) ([]mssrvsserver.ShareEntry, uint32) {
				all := mssrvsserver.FromServer(srv)
				scoped := scopedUserIsScoped(user)
				out := make([]mssrvsserver.ShareEntry, 0, len(all))
				for _, e := range all {
					keep := e.Name != scopedShareName // admin：藏掉「共享」
					if scoped {
						keep = e.Name == scopedShareName || e.Name == "IPC$"
					}
					if keep {
						out = append(out, e)
					}
				}
				return out, 0
			}
			return &pipeAdapter{h: dcerpcserver.NewPipeHandler("srvsvc", svc)}, nil
		},
	}}

	go func() {
		log.Printf("[smb] 监听 :%d，共享 %v", port, names)
		if err := srv.ListenAndServe(fmt.Sprintf(":%d", port)); err != nil && err != server.ErrServerClosed {
			smbMu.Lock()
			smbRunning = false
			smbErr = err.Error()
			smbMu.Unlock()
			log.Printf("[smb] 服务退出：%v", err)
		}
	}()

	smbRunning = true
	smbSrv = srv
	smbErr = ""
	smbPort = port
	return nil
}

func smbStop() {
	smbMu.Lock()
	srv := smbSrv
	smbSrv = nil
	smbRunning = false
	smbMu.Unlock()
	if srv == nil {
		return
	}
	// 只有在用户**明确关掉 SMB** 时才走到这里（改根目录走 smbReload，不重启）。
	// Shutdown 只关监听、然后等现有连接自己走，而手机的长连接不会自己走——
	// 不补一刀的话，界面上写着「已关闭」，445 上却还在正常服务。
	// 所以优雅等待只给 800ms 收尾，然后 Close() 把连接一起断掉。
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
	}
	log.Printf("[smb] 已停止")
}

// smbRunningNow 报告 SMB 服务当前是否在跑
func smbRunningNow() bool {
	smbMu.Lock()
	defer smbMu.Unlock()
	return smbRunning
}

// smbReload 让「根目录的增删改」立刻反映到 SMB 上。
//
// 早先这里是 stop + start 重建整个服务，代价太大：重启会把**所有**已建立的
// 连接踢掉。用户的 iPhone 被踢几次之后就再也不去问服务端了，点进共享一直显示
// 「内容不可用」——而服务端这边一切正常（新连接能正常列目录）。
//
// 现在改成只改共享表，监听和连接都不动：
//   - 新出现的共享：RegisterShare，新连接立刻能打开
//   - 消失的共享：UnregisterShare（新连接报 BAD_NETWORK_NAME）
//   - DropShare（把已经打开的 tree 关掉，客户端收到 NETWORK_NAME_DELETED）
//   - srvsvc 的枚举列表跟着换一份，手机刷新共享列表就能看到最新状态
//
// 这就是真文件服务器在管理员删共享时的行为，客户端本来就处理得了。
func smbReload() {
	smbMu.Lock()
	srv, running := smbSrv, smbRunning
	smbMu.Unlock()
	if !running || srv == nil {
		return // 没在跑，下次启动自然会用新配置
	}
	go func() {
		smbMu.Lock()
		smbReloading = true
		smbMu.Unlock()
		defer func() {
			smbMu.Lock()
			smbReloading = false
			smbMu.Unlock()
		}()

		shares, names, err := smbBuildShares()
		if err != nil {
			// 一个根都不剩：共享全部摘掉，门全部关上，但服务本身继续跑
			log.Printf("[smb] 根目录变了，没有可用共享：%v", err)
			shares, names = nil, nil
		}

		// 哪些根目录这一轮还开着（key 与 smbGates 一致，都是小写路径）
		active := make(map[string]bool, len(shares))
		for _, sh := range shares {
			if g, ok := sh.VFS.(*smbGate); ok {
				active[strings.ToLower(g.root)] = true
			}
		}
		if closed := smbCloseGates(active); len(closed) > 0 {
			log.Printf("[smb] 已关闭根目录：%v", closed)
		}

		want := make(map[string]bool, len(shares))
		for _, sh := range shares {
			want[strings.ToLower(sh.Name)] = true
		}
		// 摘掉不该存在的共享名。**不删 tree**：客户端手里的挂载要留着，
		// 它下一句 CREATE 会打到刚关上的门（ACCESS_DENIED），
		// 盘再打开时同一个 tree 立刻恢复（见 smb_gate.go）。
		for _, have := range srv.ShareNames() {
			if !want[have] {
				srv.UnregisterShare(have)
			}
		}
		for _, sh := range shares {
			srv.RegisterShare(sh.Name, sh)
		}
		log.Printf("[smb] 根目录变了，共享已更新：%v", names)
	}()
}

// smbStatus 给管理台的状态
func smbStatus() map[string]any {
	smbMu.Lock()
	running, errMsg, port := smbRunning || smbReloading, smbErr, smbPort
	smbMu.Unlock()
	usePort := cfg.SMB.Port
	if port > 0 && running {
		usePort = port
	}
	if usePort <= 0 {
		usePort = smbDefaultPort
	}
	st := map[string]any{
		"supported": runtime.GOOS == "windows" || runtime.GOOS == "linux" || runtime.GOOS == "darwin",
		"os":        runtime.GOOS,
		"enabled":   cfg.SMB.Enabled,
		"running":   running,
		"port":      usePort,
		"user":      cfg.User,
		"error":     errMsg,
		"hasCred":   strings.TrimSpace(cfg.SMB.NTHash) != "",
		"tookOver":  cfg.SMB.TookOver,
	}
	shares := []string{}
	for _, r := range cfg.Roots {
		if r.Enabled {
			shares = append(shares, smbShareName(r.Name))
		}
	}
	st["shares"] = shares
	return st
}

// smbServerGUID 取出（或首次生成并落盘）服务端 GUID。
//
// 这个值是客户端的「服务器身份」：Apple 的 SMB 客户端会把它跟自己记住的
// 那台服务器对起来。库的默认行为是「配零就随便生成一个」，也就是**每次重启都换身份**，
// 客户端那边还挂着旧身份，重连就会给一个莫名其妙的基础错误（EINVAL / 无效的自变量）。
// 所以这里持久化到 config.json。
func smbServerGUID() [16]byte {
	var g [16]byte
	if s := strings.TrimSpace(cfg.SMB.GUID); s != "" {
		if b, err := hex.DecodeString(s); err == nil && len(b) == 16 {
			copy(g[:], b)
			return g
		}
	}
	if _, err := rand.Read(g[:]); err != nil {
		return [16]byte{} // 生成失败就交给库自己随机，至少能起来
	}
	cfgMu.Lock()
	cfg.SMB.GUID = hex.EncodeToString(g[:])
	err := saveConfigLocked()
	cfgMu.Unlock()
	if err != nil {
		log.Printf("[smb] 保存服务端 GUID 失败：%v", err)
	}
	return g
}

// smbUTF16 把 SMB 里的 UTF-16LE 字符串转成 Go 字符串（文件名都是这么编码的）
func smbUTF16(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, binary.LittleEndian.Uint16(b[i:i+2]))
	}
	return string(utf16.Decode(u))
}

// smbWalkCompound 遍历复合 PDU 里的每个 SMB2 头+体。nextCommand 链上
// 每段头都带自己的命令码，raw 钩子只看到第一段的场景就是被这个坑过的。
func smbWalkCompound(raw []byte, fn func(cmd uint16, body []byte)) {
	off := 0
	for off+64 <= len(raw) {
		h := raw[off:]
		cmd := binary.LittleEndian.Uint16(h[12:14])
		next := int(binary.LittleEndian.Uint32(h[20:24]))
		bodyEnd := len(raw)
		if next > 0 && off+next <= len(raw) {
			bodyEnd = off + next
		}
		if off+64 <= bodyEnd {
			fn(cmd, raw[off+64:bodyEnd])
		}
		if next == 0 {
			break
		}
		// NextCommand 是「从头到下一段头」的绝对偏移，发送方已保证 8 字节对齐，
		// 这里不能再补一次对齐（补了会把后面每段的 body 都错位 8 字节）
		off += next
	}
}

// smbCmdName 把 SMB2 命令码翻成人话，日志里看得懂才有意义
func smbCmdName(cmd uint16) string {
	switch cmd {
	case 0x00:
		return "NEGOTIATE"
	case 0x01:
		return "SESSION_SETUP"
	case 0x02:
		return "LOGOFF"
	case 0x03:
		return "TREE_CONNECT"
	case 0x04:
		return "TREE_DISCONNECT"
	case 0x05:
		return "CREATE"
	case 0x06:
		return "CLOSE"
	case 0x07:
		return "FLUSH"
	case 0x08:
		return "READ"
	case 0x09:
		return "WRITE"
	case 0x0b:
		return "IOCTL"
	case 0x0c:
		return "CANCEL"
	case 0x0d:
		return "ECHO"
	case 0x0e:
		return "QUERY_DIRECTORY"
	case 0x0f:
		return "CHANGE_NOTIFY"
	case 0x10:
		return "QUERY_INFO"
	case 0x11:
		return "SET_INFO"
	case 0x12:
		return "OPLOCK_BREAK"
	}
	return fmt.Sprintf("cmd#0x%02x", cmd)
}

// smbStatusName 只翻几个「连不上」时真会遇到的码，其余留十六进制
func smbStatusName(st uint32) string {
	switch st {
	case 0xc000000d:
		return "STATUS_INVALID_PARAMETER，客户端一般显示「无效的自变量」"
	case 0xc000000f:
		return "STATUS_NO_SUCH_FILE"
	case 0xc0000022:
		return "STATUS_ACCESS_DENIED"
	case 0xc0000034:
		return "STATUS_OBJECT_NAME_NOT_FOUND"
	case 0xc00000bb:
		return "STATUS_NOT_SUPPORTED"
	case 0xc00000cc:
		return "STATUS_BAD_NETWORK_NAME（共享不存在）"
	case 0xc0000016:
		return "STATUS_MORE_PROCESSING_REQUIRED（认证还没走完）"
	case 0xc000006d:
		return "STATUS_LOGON_FAILURE（用户名或密码不对）"
	case 0xc0000203:
		return "STATUS_USER_SESSION_DELETED（会话被删了，不是路径问题）"
	case 0xc0000128:
		return "STATUS_FILE_CLOSED（复合请求里前一条已失败，后面的句柄操作自然失效）"
	case 0x80000005:
		return "STATUS_BUFFER_OVERFLOW（缓冲区不够，客户端会自己再来一次）"
	case 0x80000006:
		return "STATUS_NO_MORE_FILES（目录列完了，正常结束）"
	case 0x8000001a:
		return "STATUS_NO_MORE_ENTRIES（枚举完了，正常结束）"
	}
	return "未知状态码"
}

// smbStatusIsError 区分「真失败」和「warning 级状态」。
// NTSTATUS 的高两位是 severity：0b11 = error，0b10 = warning，
// 0b00 = success。目录列完（NO_MORE_FILES）、缓冲区不够（BUFFER_OVERFLOW）
// 都是 warning 甚至正常收尾，记成「失败」只会让日志误导人。
func smbStatusIsError(st uint32) bool {
	return st&0xc0000000 == 0xc0000000
}

// smbPortFree 报告端口当前是否可用（可用 = 我们能绑上）
func smbPortFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}
