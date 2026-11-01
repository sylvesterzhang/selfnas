package main

// 受限账号：给 SMB / WebDAV 的「只看白名单文件夹」访问方式。
//
// 背景与需求（2026-10-30 与用户对齐）：
//   - admin 永远全盘（资料/软件/文档）；
//   - 后台可以建受限账号，每个账号单独配一份「允许访问的文件夹」白名单；
//   - 受限账号连 SMB 看到且只看到一个「共享」入口，点进去是白名单文件夹
//     的虚拟合并视图；连 WebDAV（/dav/）同理；
//   - 浏览器管理台不受限（还是 admin 登录那套）。
//
// 密码存储：受限账号不需要网页登录，所以只存 NT hash
// （MD4(UTF-16LE(pwd))，hex）。SMB 认证直接用它；WebDAV 的 Basic
// 认证拿到的是明文密码，现算 NT hash 比对即可，不需要 PBKDF2。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// ScopedFolder 白名单里的一个文件夹。
// Readonly 是这个文件夹单独的写权限：开了就是只读（网页端/SMB/WebDAV 全都只给看，
// 上传、新建、改名、删除、移动一律拒绝）。
type ScopedFolder struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

// UnmarshalJSON 兼容老配置：folders 元素原来就是裸字符串（"D:\\视频"）。
// 升级后不改用户已有白名单，老条目一律按「读写」读进来（跟升级前行为一致）。
func (f *ScopedFolder) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = ScopedFolder{Path: s}
		return nil
	}
	type plain ScopedFolder
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*f = ScopedFolder(p)
	return nil
}

// ScopedAccount 一个受限账号。User 不区分大小写（SMB/DAV 都按小写比对）。
type ScopedAccount struct {
	User    string         `json:"user"`
	NTHash  string         `json:"ntHash"`  // hex(MD4(UTF-16LE(pass)))，见 ntHashOf
	Folders []ScopedFolder `json:"folders"` // 允许访问的文件夹 + 各自的读写权限，展示名取路径基名
}

var scopedMu sync.RWMutex

// scopedAccountsFromCfg 返回配置里的受限账号列表（浅拷贝引用，读侧不改内容）。
func scopedAccountsFromCfg() []ScopedAccount {
	return cfg.ScopedAccounts
}

// scopedAfterChange 账号/白名单变更后的公共收尾：
// 刷新 SMB 认证器账号表（增删账号要立即生效）+ 清内层 FS 缓存（保守）。
// 白名单的展示是每请求现算的，不需要重建共享。
func scopedAfterChange() {
	smbRefreshAuthAccounts()
	scopedInvalidate()
}

// scopedSave 把账号列表写回配置并落盘。调用方已持有/新建 scopedMu 写锁。
func scopedSave() error {
	return saveConfig()
}

// scopedNormalizeUser 统一用户名大小写。
func scopedNormalizeUser(u string) string {
	return strings.ToLower(strings.TrimSpace(u))
}

// scopedFind 按用户名找账号（大小写不敏感）。
func scopedFind(user string) (ScopedAccount, bool) {
	key := scopedNormalizeUser(user)
	scopedMu.RLock()
	defer scopedMu.RUnlock()
	for _, a := range cfg.ScopedAccounts {
		if scopedNormalizeUser(a.User) == key {
			return a, true
		}
	}
	return ScopedAccount{}, false
}

// scopedUpsert 新建或更新受限账号（密码留空表示只改文件夹不改密码）。
func scopedUpsert(user, pass string) error {
	user = strings.TrimSpace(user)
	key := scopedNormalizeUser(user)
	if key == "" {
		return errScoped("用户名不能为空")
	}
	if key == scopedNormalizeUser(cfg.User) {
		return errScoped("不能和管理员账号重名")
	}
	if strings.ContainsAny(user, `/\:"<>|?*`) {
		return errScoped("用户名含非法字符")
	}
	scopedMu.Lock()
	defer scopedMu.Unlock()
	for i := range cfg.ScopedAccounts {
		if scopedNormalizeUser(cfg.ScopedAccounts[i].User) == key {
			if pass != "" {
				cfg.ScopedAccounts[i].NTHash = ntHashOf(pass)
			}
			_ = user
			if err := scopedSave(); err != nil {
				return err
			}
			scopedAfterChange()
			return nil
		}
	}
	if pass == "" {
		return errScoped("新账号必须设置密码")
	}
	cfg.ScopedAccounts = append(cfg.ScopedAccounts, ScopedAccount{
		User:   user,
		NTHash: ntHashOf(pass),
	})
	if err := scopedSave(); err != nil {
		return err
	}
	scopedAfterChange()
	return nil
}

// scopedDelete 删除账号。
func scopedDelete(user string) error {
	key := scopedNormalizeUser(user)
	scopedMu.Lock()
	defer scopedMu.Unlock()
	out := cfg.ScopedAccounts[:0]
	found := false
	for _, a := range cfg.ScopedAccounts {
		if scopedNormalizeUser(a.User) == key {
			found = true
			continue
		}
		out = append(out, a)
	}
	if !found {
		return errScoped("账号不存在")
	}
	cfg.ScopedAccounts = out
	if err := scopedSave(); err != nil {
		return err
	}
	scopedAfterChange()
	return nil
}

// scopedAddFolder 给账号加一个白名单文件夹。要求：
//   - 路径真实存在且是目录；
//   - 必须落在某个启用的根目录之内（和「允许的盘」口径一致）。
//
// readonly 是新加的文件夹默认权限：勾了就是只读，客户端只能看不能改。
func scopedAddFolder(user, path string, readonly bool) error {
	abs, err := scopedValidatePath(path)
	if err != nil {
		return err
	}
	scopedMu.Lock()
	defer scopedMu.Unlock()
	for i := range cfg.ScopedAccounts {
		if scopedNormalizeUser(cfg.ScopedAccounts[i].User) != scopedNormalizeUser(user) {
			continue
		}
		for _, have := range cfg.ScopedAccounts[i].Folders {
			if strings.EqualFold(have.Path, abs) {
				return errScoped("该文件夹已在白名单里")
			}
		}
		cfg.ScopedAccounts[i].Folders = append(cfg.ScopedAccounts[i].Folders, ScopedFolder{Path: abs, Readonly: readonly})
		if err := scopedSave(); err != nil {
			return err
		}
		scopedAfterChange()
		return nil
	}
	return errScoped("账号不存在")
}

// scopedSetFolderReadonly 改某个白名单文件夹的读写权限。
// 改完必须清内层 FS 缓存（SMB 的只读是建 FS 时就定死的，见 scopedInnerFor）。
func scopedSetFolderReadonly(user, path string, readonly bool) error {
	// 客户端传回来的路径形态不一定标准：手输、粘贴、或者经过 shell 的路径转换，
	// 正斜杠和重复分隔符都可能出现。先用 filepath.Clean 归一成 Windows 形态再比，
	// 不然后端「白名单里没有这个文件夹」的报错会让人摸不着头脑。
	path = filepath.Clean(path)
	scopedMu.Lock()
	defer scopedMu.Unlock()
	for i := range cfg.ScopedAccounts {
		if scopedNormalizeUser(cfg.ScopedAccounts[i].User) != scopedNormalizeUser(user) {
			continue
		}
		for j := range cfg.ScopedAccounts[i].Folders {
			if !strings.EqualFold(cfg.ScopedAccounts[i].Folders[j].Path, path) {
				continue
			}
			if cfg.ScopedAccounts[i].Folders[j].Readonly == readonly {
				return nil // 没变化，别白写盘
			}
			cfg.ScopedAccounts[i].Folders[j].Readonly = readonly
			if err := scopedSave(); err != nil {
				return err
			}
			scopedAfterChange()
			return nil
		}
		return errScoped("白名单里没有这个文件夹")
	}
	return errScoped("账号不存在")
}

// scopedRemoveFolder 移除白名单文件夹（大小写不敏感比对）。
func scopedRemoveFolder(user, path string) error {
	path = filepath.Clean(path)   // 同 scopedSetFolderReadonly：先归一形态再比
	scopedMu.Lock()
	defer scopedMu.Unlock()
	for i := range cfg.ScopedAccounts {
		if scopedNormalizeUser(cfg.ScopedAccounts[i].User) != scopedNormalizeUser(user) {
			continue
		}
		out := cfg.ScopedAccounts[i].Folders[:0]
		found := false
		for _, f := range cfg.ScopedAccounts[i].Folders {
			if strings.EqualFold(f.Path, path) {
				found = true
				continue
			}
			out = append(out, f)
		}
		if !found {
			return errScoped("白名单里没有这个文件夹")
		}
		cfg.ScopedAccounts[i].Folders = out
		if err := scopedSave(); err != nil {
			return err
		}
		scopedAfterChange()
		return nil
	}
	return errScoped("账号不存在")
}

// scopedValidatePath 校验白名单路径：绝对、存在、目录、落在启用的根之内。
func scopedValidatePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errScoped("路径不能为空")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", errScoped("路径非法")
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", errScoped("路径不存在或不可读：" + err.Error())
	}
	if !st.IsDir() {
		return "", errScoped("必须是一个文件夹")
	}
	for _, r := range cfg.Roots {
		if !r.Enabled {
			continue
		}
		if scopedWithin(abs, r.Path) {
			return abs, nil
		}
	}
	return "", errScoped("必须在已启用的盘（资料/软件/文档）之内")
}

// scopedWithin 判断 child（含相等）是否在 root 目录树内。都走绝对路径比较，
// 大小写不敏感（Windows 盘符大小写随手写）。
func scopedWithin(child, root string) bool {
	child = filepath.Clean(child)
	root = filepath.Clean(root)
	if strings.EqualFold(child, root) {
		return true
	}
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// scopedFoldersFor 取某用户的白名单；不是受限账号返回 false。
func scopedFoldersFor(user string) ([]ScopedFolder, bool) {
	a, ok := scopedFind(user)
	if !ok {
		return nil, false
	}
	return a.Folders, true
}

// scopedReadonlyFor 查白名单里某个真实路径是不是只读。
// 不在白名单里返回 false（调用方本来就会先做白名单匹配）。
func scopedReadonlyFor(folders []ScopedFolder, real string) bool {
	for _, f := range folders {
		if strings.EqualFold(filepath.Clean(f.Path), filepath.Clean(real)) {
			return f.Readonly
		}
	}
	return false
}

// scopedVerify 校验受限账号的密码（WebDAV Basic 用：现算 NT hash 比对）。
func scopedVerify(user, pass string) bool {
	a, ok := scopedFind(user)
	if !ok {
		return false
	}
	return a.NTHash != "" && ntHashOf(pass) == a.NTHash
}

// scopedNameFor 给白名单文件夹取展示名：路径基名；重名时加后缀保证唯一。
// 展示名会直接变成「共享」里的文件夹名 / WebDAV 的一级目录名。
func scopedNameFor(folders []ScopedFolder) map[string]string {
	used := map[string]bool{}
	out := make(map[string]string, len(folders))
	for _, f := range folders {
		p := f.Path
		name := filepath.Base(filepath.Clean(p))
		if name == "" || name == "." || name == string(filepath.Separator) {
			name = "文件夹"
		}
		base := name
		for i := 2; used[strings.ToLower(name)]; i++ {
			name = base + " (" + strconv.Itoa(i) + ")"
		}
		used[strings.ToLower(name)] = true
		out[p] = name
	}
	return out
}

// errScoped 一个轻量错误类型，API 层直接把消息透给前端。
type errScoped string

func (e errScoped) Error() string { return string(e) }

// ---------- 管理台 API ----------

// handleScopedAccounts /api/scoped：受限账号的增删查。
//   GET            列出（不带 hash）
//   POST {user,pass}          新建 / 改密码（pass 留空=只更新已有账号的存在性不行，新建必须带密码）
//   DELETE ?user=xxx          删除
func handleScopedAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		scopedMu.RLock()
		type folderOut struct {
			Path     string `json:"path"`
			Readonly bool   `json:"readonly"`
		}
		type acct struct {
			User    string      `json:"user"`
			Folders []folderOut `json:"folders"`
		}
		out := make([]acct, 0, len(cfg.ScopedAccounts))
		for _, a := range cfg.ScopedAccounts {
			fs := make([]folderOut, 0, len(a.Folders))
			for _, f := range a.Folders {
				fs = append(fs, folderOut{Path: f.Path, Readonly: f.Readonly})
			}
			out = append(out, acct{User: a.User, Folders: fs})
		}
		scopedMu.RUnlock()
		writeJSON(w, 0, "ok", map[string]any{"accounts": out})

	case http.MethodPost:
		var body struct {
			User string `json:"user"`
			Pass string `json:"pass"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, 400, "请求格式错误", nil)
			return
		}
		if err := scopedUpsert(body.User, body.Pass); err != nil {
			writeJSON(w, 400, err.Error(), nil)
			return
		}
		writeJSON(w, 0, "已保存", nil)

	case http.MethodDelete:
		user := r.URL.Query().Get("user")
		if err := scopedDelete(user); err != nil {
			writeJSON(w, 400, err.Error(), nil)
			return
		}
		writeJSON(w, 0, "已删除", nil)

	default:
		writeJSON(w, 405, "只支持 GET/POST/DELETE", nil)
	}
}

// handleScopedFolder /api/scoped/folder：白名单文件夹的增删。
//   POST   {user, path, readonly}   加（路径必须存在、在启用的盘之内；readonly 省略=读写）
//   DELETE ?user=&path=             删
func handleScopedFolder(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var body struct {
			User     string `json:"user"`
			Path     string `json:"path"`
			Readonly bool   `json:"readonly"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, 400, "请求格式错误", nil)
			return
		}
		if err := scopedAddFolder(body.User, body.Path, body.Readonly); err != nil {
			writeJSON(w, 400, err.Error(), nil)
			return
		}
		writeJSON(w, 0, "已加入白名单", nil)

	case http.MethodDelete:
		user := r.URL.Query().Get("user")
		path := r.URL.Query().Get("path")
		if err := scopedRemoveFolder(user, path); err != nil {
			writeJSON(w, 400, err.Error(), nil)
			return
		}
		writeJSON(w, 0, "已移除", nil)

	default:
		writeJSON(w, 405, "只支持 POST/DELETE", nil)
	}
}

// handleScopedPerm /api/scoped/perm：改某个白名单文件夹的读写权限。
//
//	POST {user, path, readonly}
//
// 改了立即生效：网页端下一次请求的 readonly、SMB 内层 FS（缓存按权限区分）、
// WebDAV 的写判定都读同一份配置。
func handleScopedPerm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "只支持 POST", nil)
		return
	}
	var body struct {
		User     string `json:"user"`
		Path     string `json:"path"`
		Readonly bool   `json:"readonly"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	if err := scopedSetFolderReadonly(body.User, body.Path, body.Readonly); err != nil {
		writeJSON(w, 400, err.Error(), nil)
		return
	}
	msg := "已设为可读写"
	if body.Readonly {
		msg = "已设为只读"
	}
	writeJSON(w, 0, msg, nil)
}
