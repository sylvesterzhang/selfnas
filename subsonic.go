package main

// Subsonic API 兼容层（阶段三）。
//
// 目的：让 iOS/Android 的原生 Subsonic App（Substreamer / play:Sub / DSub 等）直接连上来，
// 锁屏播放、耳机线控、车载、离线缓存全部交给 App —— 这是「手机听歌」的主线方案，
// 网页端在 iOS 上的后台播放限制救不了。
//
// 三个关键设计：
//  1. **同端口挂在 /rest/**，不开新端口、不加新进程。安全面与文件站一致。
//  2. **宁返回空列表，不返回报错**：App 对空结果容忍度很高，对 500 很敏感。
//     未实现的端点一律回 status=ok（而不是 error），这是各家实现的通行做法。
//  3. **id 是自描述的不透明串**：`tr-` + base64url(root|path)、`al-` + base64url(artist|album)、
//     `ar-` + base64url(artist)、`pl-` + 歌单行号。不需要维护内存映射表，
//     重启后同一个文件还是同一个 id（文档里原方案「启动时重建映射」会让 App 的缓存全部失效）。
//
// 鉴权：Subsonic 标准 u/t/s/v 或 p（明文 / enc:hex）。token = md5(明文密码 + salt)，
// 而我们的密码只存了 PBKDF2 与 NT hash（都不可逆）—— 所以额外存一份 AES-GCM 密封的
// 明文副本（key = sha256(cfg.Secret)），只在网页登录 / 改密码 / 建受限账号时写入。
// 没有这份副本时，回一条带指引的 40 错误（让用户先用网页登录一次），而不是静默失败。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const sxVersion = "1.16.1"

// ---------- 密码的可逆副本（只服务 Subsonic token 鉴权） ----------

func sxPassKey() []byte {
	cfgMu.Lock()
	sec := cfg.Secret
	cfgMu.Unlock()
	sum := sha256.Sum256([]byte("selfnas-subsonic|" + sec))
	return sum[:]
}

// sealPass 把明文密码封成 "v1:<base64(nonce|密文)>"。
func sealPass(plain string) string {
	if plain == "" {
		return ""
	}
	block, err := aes.NewCipher(sxPassKey())
	if err != nil {
		return ""
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ""
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return ""
	}
	out := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return "v1:" + base64.StdEncoding.EncodeToString(out)
}

func openPass(enc string) (string, bool) {
	if !strings.HasPrefix(enc, "v1:") {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(enc[3:])
	if err != nil {
		return "", false
	}
	block, err := aes.NewCipher(sxPassKey())
	if err != nil {
		return "", false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(raw) < gcm.NonceSize() {
		return "", false
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", false
	}
	return string(plain), true
}

// ⚠️ 下面两个 ensure 都在**函数入口先算密封值**，然后才抢 cfgMu：
// sealPass 内部的 sxPassKey 也要读 cfg.Secret（会短暂拿 cfgMu），
// 如果先 Lock 再 sealPass 就是自己等自己 —— 曾把整台服务的配置锁永久占死（2026-11-03 踩过）。

// sxEnsureAdminPassEnc 网页登录成功时补一份密封副本（老配置升级后第一次登录就会补上）。
func sxEnsureAdminPassEnc(plain string) {
	if plain == "" {
		return
	}
	enc := sealPass(plain)
	if enc == "" {
		return
	}
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if cfg.PassEnc == enc {
		return
	}
	cfg.PassEnc = enc
	_ = saveConfigLocked()
}

func sxEnsureScopedPassEnc(user, plain string) {
	if plain == "" {
		return
	}
	keyName := scopedNormalizeUser(user)
	enc := sealPass(plain)
	if enc == "" {
		return
	}
	cfgMu.Lock()
	defer cfgMu.Unlock()
	for i := range cfg.ScopedAccounts {
		if scopedNormalizeUser(cfg.ScopedAccounts[i].User) != keyName {
			continue
		}
		if cfg.ScopedAccounts[i].PassEnc == enc {
			return
		}
		cfg.ScopedAccounts[i].PassEnc = enc
		_ = saveConfigLocked()
		return
	}
}

// ---------- 鉴权 ----------

type sxAuthUser struct {
	Name  string
	Admin bool
}

type sxAuthErr struct {
	Code int
	Msg  string
}

// sxAuth 按 Subsonic 规则校验。返回用户或错误码（10 缺参数 / 40 密码错 / 50 无权限）。
func sxAuth(r *http.Request) (sxAuthUser, *sxAuthErr) {
	q := r.URL.Query()
	u := strings.TrimSpace(q.Get("u"))
	tok := strings.TrimSpace(q.Get("t"))
	salt := q.Get("s")
	p := q.Get("p")
	if u == "" {
		return sxAuthUser{}, &sxAuthErr{10, "缺少参数 u"}
	}
	cfgMu.Lock()
	adminName, adminEnc, adminHash := cfg.User, cfg.PassEnc, cfg.PassHash
	var scopedAcc *ScopedAccount
	for i := range cfg.ScopedAccounts {
		if scopedNormalizeUser(cfg.ScopedAccounts[i].User) == scopedNormalizeUser(u) {
			a := cfg.ScopedAccounts[i]
			scopedAcc = &a
			break
		}
	}
	cfgMu.Unlock()

	isAdmin := scopedNormalizeUser(u) == scopedNormalizeUser(adminName)
	enc := adminEnc
	if !isAdmin {
		if scopedAcc == nil {
			return sxAuthUser{}, &sxAuthErr{40, "账号或密码错误"}
		}
		enc = scopedAcc.PassEnc
	}

	ok := false
	var plain string
	switch {
	case p != "":
		plain = p
		if strings.HasPrefix(p, "enc:") {
			if b, err := hex.DecodeString(strings.TrimPrefix(p, "enc:")); err == nil {
				plain = string(b)
			}
		}
		if isAdmin {
			ok = verifyPassword(plain, adminHash)
		} else {
			ok = scopedAcc.NTHash != "" && ntHashOf(plain) == scopedAcc.NTHash
		}
	case tok != "" && salt != "":
		if enc == "" {
			return sxAuthUser{}, &sxAuthErr{40,
				"该账号还没启用 App 访问：请先用网页版登录一次（或改一次密码），之后 App 就能连了"}
		}
		dec, okDec := openPass(enc)
		if !okDec {
			return sxAuthUser{}, &sxAuthErr{40, "凭据已失效：请在网页端重新登录一次"}
		}
		sum := md5.Sum([]byte(dec + salt))
		got := hex.EncodeToString(sum[:])
		ok = subtle.ConstantTimeCompare([]byte(strings.ToLower(got)), []byte(strings.ToLower(tok))) == 1
		plain = dec
	default:
		return sxAuthUser{}, &sxAuthErr{10, "缺少鉴权参数（需要 t+s 或 p）"}
	}
	if !ok {
		return sxAuthUser{}, &sxAuthErr{40, "账号或密码错误"}
	}
	// 明文档（p=）走通时顺手把密封副本补上，下次 App 切 token 也能用
	if plain != "" {
		if isAdmin {
			go sxEnsureAdminPassEnc(plain)
		} else {
			go sxEnsureScopedPassEnc(u, plain)
		}
	}
	if isAdmin {
		return sxAuthUser{Name: adminName, Admin: true}, nil
	}
	return sxAuthUser{Name: scopedAcc.User}, nil
}

// ---------- id 编解码 ----------

func sxEnc(parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "\x00")))
}

func sxDec(id string) []string {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	return strings.Split(string(b), "\x00")
}

func sxTrackID(t *musicTrack) string          { return "tr-" + sxEnc(t.Root, t.Path) }
func sxArtistID(name string) string           { return "ar-" + sxEnc(name) }
func sxAlbumIDOf(artist, album string) string { return "al-" + sxEnc(artist, album) }
// 歌单 id 直接吐数据库自增号，不加前缀。
// 曲目/专辑要 "tr-"/"al-" 前缀是因为 id 是「root+path 的 base64」，得带标签区分类型；
// 歌单 id 本身就是整数，再加前缀反而可能被客户端当字符串/整数混用而丢条目
// （Substreamer 一类 App 的模型里 playlist.id 是强类型）。解析侧仍兼容老的 "pl-N"。
func sxPlaylistID(id int64) string            { return strconv.FormatInt(id, 10) }

func sxTrackByID(user, id string) *musicTrack {
	if !strings.HasPrefix(id, "tr-") {
		return nil
	}
	parts := sxDec(strings.TrimPrefix(id, "tr-"))
	if len(parts) != 2 {
		return nil
	}
	tracks := musicVisibleTracks(user)
	for _, t := range tracks {
		if t.Root == parts[0] && t.Path == parts[1] {
			return t
		}
	}
	// 兜底：客户端可能拿着**别的机器签发的旧 id**（同一批文件换了机器/换了根目录，
	// 例如以前连 Linux 那台 id 是 /home/xxx/Music/…，现在连 Windows 这台根是 D:\）。
	// App 的本地库缓存着这些 id，播一个报一个 70，界面上就是 PlaybackFailed。
	// 这里按「文件名」唯一匹配救回来；同名文件多于一个就**不猜**，照旧报错（宁可不播也别播错）。
	base := strings.ToLower(filepath.Base(filepath.FromSlash(parts[1])))
	var hit *musicTrack
	for _, t := range tracks {
		if strings.ToLower(filepath.Base(t.Path)) != base {
			continue
		}
		if hit != nil {
			return nil
		}
		hit = t
	}
	if hit != nil {
		log.Printf("[subsonic] id 兜底命中：%s → %s（旧 id 的路径不属于本机）", parts[1], hit.Path)
	}
	return hit
}

// ---------- 响应信封 ----------

type sxErr struct {
	Code    int    `xml:"code,attr" json:"code"`
	Message string `xml:"message,attr" json:"message"`
}

type sxResponse struct {
	XMLName       xml.Name `xml:"subsonic-response" json:"-"`
	XMLNS         string   `xml:"xmlns,attr,omitempty" json:"-"`
	Status        string   `xml:"status,attr" json:"status"`
	Version       string   `xml:"version,attr" json:"version"`
	Type          string   `xml:"type,attr,omitempty" json:"type,omitempty"`
	ServerVersion string   `xml:"serverVersion,attr,omitempty" json:"serverVersion,omitempty"`

	Error         *sxErr          `xml:"error,omitempty" json:"error,omitempty"`
	License       *sxLicense      `xml:"license,omitempty" json:"license,omitempty"`
	MusicFolders  *sxMusicFolders `xml:"musicFolders,omitempty" json:"musicFolders,omitempty"`
	Indexes       *sxIndexes      `xml:"indexes,omitempty" json:"indexes,omitempty"`
	Artists       *sxArtists      `xml:"artists,omitempty" json:"artists,omitempty"`
	Artist        *sxArtist       `xml:"artist,omitempty" json:"artist,omitempty"`
	Album         *sxAlbum        `xml:"album,omitempty" json:"album,omitempty"`
	AlbumList     *sxAlbumList    `xml:"albumList,omitempty" json:"albumList,omitempty"`
	AlbumList2    *sxAlbumList    `xml:"albumList2,omitempty" json:"albumList2,omitempty"`
	Song          *sxSong         `xml:"song,omitempty" json:"song,omitempty"`
	SearchResult  *sxSearch       `xml:"searchResult,omitempty" json:"searchResult,omitempty"`
	SearchResult2 *sxSearch       `xml:"searchResult2,omitempty" json:"searchResult2,omitempty"`
	SearchResult3 *sxSearch       `xml:"searchResult3,omitempty" json:"searchResult3,omitempty"`
	Playlists     *sxPlaylists    `xml:"playlists,omitempty" json:"playlists,omitempty"`
	Playlist      *sxPlaylist     `xml:"playlist,omitempty" json:"playlist,omitempty"`
	Genres        *sxGenres       `xml:"genres,omitempty" json:"genres,omitempty"`
	Directory     *sxDirectory    `xml:"directory,omitempty" json:"directory,omitempty"`
	RandomSongs   *sxSongs        `xml:"randomSongs,omitempty" json:"randomSongs,omitempty"`
	Lyrics        *sxLyrics       `xml:"lyrics,omitempty" json:"lyrics,omitempty"`
	ScanStatus    *sxScanStatus   `xml:"scanStatus,omitempty" json:"scanStatus,omitempty"`
	User          *sxUserInfo     `xml:"user,omitempty" json:"user,omitempty"`
}

type sxLicense struct {
	Valid bool `xml:"valid,attr" json:"valid"`
}

type sxMusicFolders struct {
	MusicFolder []sxMusicFolder `xml:"musicFolder" json:"musicFolder"`
}
type sxMusicFolder struct {
	ID   string `xml:"id,attr" json:"id"`
	Name string `xml:"name,attr" json:"name"`
}

type sxIndexes struct {
	Index []sxIndex `xml:"index" json:"index"`
}
type sxIndex struct {
	Name   string      `xml:"name,attr" json:"name"`
	Artist []sxArtItem `xml:"artist" json:"artist"`
}

type sxArtists struct {
	IgnoredArticles string    `xml:"ignoredArticles,attr" json:"ignoredArticles"`
	Index           []sxIndex `xml:"index" json:"index"`
}

type sxArtItem struct {
	ID         string `xml:"id,attr" json:"id"`
	Name       string `xml:"name,attr" json:"name"`
	AlbumCount int    `xml:"albumCount,attr,omitempty" json:"albumCount,omitempty"`
}

type sxArtist struct {
	ID         string      `xml:"id,attr" json:"id"`
	Name       string      `xml:"name,attr" json:"name"`
	AlbumCount int         `xml:"albumCount,attr" json:"albumCount"`
	Album      []sxAlbumID `xml:"album" json:"album,omitempty"`
}

type sxAlbumID struct {
	ID        string `xml:"id,attr" json:"id"`
	Name      string `xml:"name,attr" json:"name"`
	Artist    string `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	ArtistID  string `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	CoverArt  string `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	SongCount int    `xml:"songCount,attr" json:"songCount"`
	Duration  int    `xml:"duration,attr" json:"duration"`
	Created   string `xml:"created,attr" json:"created"`
	Year      int    `xml:"year,attr,omitempty" json:"year,omitempty"`
}

type sxAlbum struct {
	sxAlbumID
	Song []sxSong `xml:"song" json:"song,omitempty"`
}

type sxAlbumList struct {
	Album []sxAlbumID `xml:"album" json:"album"`
}

type sxSong struct {
	ID          string `xml:"id,attr" json:"id"`
	Parent      string `xml:"parent,attr,omitempty" json:"parent,omitempty"`
	IsDir       bool   `xml:"isDir,attr" json:"isDir"`
	Title       string `xml:"title,attr" json:"title"`
	Album       string `xml:"album,attr,omitempty" json:"album,omitempty"`
	Artist      string `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	Track       int    `xml:"track,attr,omitempty" json:"track,omitempty"`
	Year        int    `xml:"year,attr,omitempty" json:"year,omitempty"`
	CoverArt    string `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	Size        int64  `xml:"size,attr" json:"size"`
	ContentType string `xml:"contentType,attr" json:"contentType"`
	Suffix      string `xml:"suffix,attr" json:"suffix"`
	Duration    int    `xml:"duration,attr" json:"duration"`
	Path        string `xml:"path,attr,omitempty" json:"path,omitempty"`
	AlbumID     string `xml:"albumId,attr,omitempty" json:"albumId,omitempty"`
	ArtistID    string `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	Type        string `xml:"type,attr" json:"type"`
	DiscNumber  int    `xml:"discNumber,attr,omitempty" json:"discNumber,omitempty"`
}

type sxSongs struct {
	Song []sxSong `xml:"song" json:"song"`
}

type sxSearch struct {
	Artist []sxArtItem `xml:"artist" json:"artist,omitempty"`
	Album  []sxAlbumID `xml:"album" json:"album,omitempty"`
	Song   []sxSong    `xml:"song" json:"song,omitempty"`
}

type sxPlaylists struct {
	Playlist []sxPlaylist `xml:"playlist" json:"playlist"`
}

// 歌单对象。字段按 Subsonic 1.16.1 规范补齐 —— 其中 songCount / duration / created /
// changed 是**必填**，缺了会让严格实现的客户端整条丢掉（App 里表现成「播放列表为空」）。
// 我们不做公开歌单，public 恒 false 但必须显式吐出来（规范要求：支持该字段就必须带默认值）。
type sxPlaylist struct {
	ID        string   `xml:"id,attr" json:"id"`
	Name      string   `xml:"name,attr" json:"name"`
	Comment   string   `xml:"comment,attr,omitempty" json:"comment,omitempty"`
	Owner     string   `xml:"owner,attr,omitempty" json:"owner,omitempty"`
	Public    bool     `xml:"public,attr" json:"public"`
	SongCount int      `xml:"songCount,attr" json:"songCount"`
	Duration  int      `xml:"duration,attr" json:"duration"`
	Created   string   `xml:"created,attr" json:"created"`
	Changed   string   `xml:"changed,attr" json:"changed"`
	CoverArt  string   `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	Entry     []sxSong `xml:"entry" json:"entry,omitempty"`
}

type sxGenres struct {
	Genre []sxGenre `xml:"genre" json:"genre"`
}
type sxGenre struct {
	Value      string `xml:",chardata" json:"value"`
	SongCount  int    `xml:"songCount,attr" json:"songCount"`
	AlbumCount int    `xml:"albumCount,attr" json:"albumCount"`
}

type sxDirectory struct {
	ID    string    `xml:"id,attr" json:"id"`
	Name  string    `xml:"name,attr" json:"name"`
	Child []sxChild `xml:"child" json:"child"`
}
type sxChild struct {
	ID       string `xml:"id,attr" json:"id"`
	Parent   string `xml:"parent,attr,omitempty" json:"parent,omitempty"`
	IsDir    bool   `xml:"isDir,attr" json:"isDir"`
	Title    string `xml:"title,attr" json:"title"`
	Album    string `xml:"album,attr,omitempty" json:"album,omitempty"`
	Artist   string `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	CoverArt string `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	Duration int    `xml:"duration,attr,omitempty" json:"duration,omitempty"`
	Year     int    `xml:"year,attr,omitempty" json:"year,omitempty"`
	Track    int    `xml:"track,attr,omitempty" json:"track,omitempty"`
	Type     string `xml:"type,attr,omitempty" json:"type,omitempty"`
}

type sxLyrics struct {
	Artist string `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	Title  string `xml:"title,attr,omitempty" json:"title,omitempty"`
	Value  string `xml:",chardata" json:"value"`
}

type sxScanStatus struct {
	Scanning bool   `xml:"scanning,attr" json:"scanning"`
	Count    int64  `xml:"count,attr" json:"count"`
	LastScan string `xml:"lastScan,attr,omitempty" json:"lastScan,omitempty"`
}

type sxUserInfo struct {
	Username          string `xml:"username,attr" json:"username"`
	Email             string `xml:"email,attr,omitempty" json:"email,omitempty"`
	ScrobblingEnabled bool   `xml:"scrobblingEnabled,attr" json:"scrobblingEnabled"`
	AdminRole         bool   `xml:"adminRole,attr" json:"adminRole"`
	PlaylistRole      bool   `xml:"playlistRole,attr" json:"playlistRole"`
	DownloadRole      bool   `xml:"downloadRole,attr" json:"downloadRole"`
	UploadRole        bool   `xml:"uploadRole,attr" json:"uploadRole"`
	CoverArtRole      bool   `xml:"coverArtRole,attr" json:"coverArtRole"`
	CommentRole       bool   `xml:"commentRole,attr" json:"commentRole"`
	PodcastRole       bool   `xml:"podcastRole,attr" json:"podcastRole"`
	StreamRole        bool   `xml:"streamRole,attr" json:"streamRole"`
	JukeboxRole       bool   `xml:"jukeboxRole,attr" json:"jukeboxRole"`
	ShareRole         bool   `xml:"shareRole,attr" json:"shareRole"`
}

func sxWrite(w http.ResponseWriter, r *http.Request, resp *sxResponse) {
	resp.XMLNS = "http://subsonic.org/restapi"
	resp.Version = sxVersion
	resp.Type = "selfnas"
	resp.ServerVersion = version
	if resp.Status == "" {
		resp.Status = "ok"
	}
	if strings.EqualFold(r.URL.Query().Get("f"), "json") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"subsonic-response": resp})
		return
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	out, err := xml.Marshal(resp)
	if err != nil {
		http.Error(w, "xml encode error", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(out)
}

func sxFail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	sxWrite(w, r, &sxResponse{Status: "failed", Error: &sxErr{Code: code, Message: msg}})
}

// ---------- 转换与聚合 ----------

func sxSuffix(path string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if ext == "" {
		return "mp3"
	}
	return ext
}

// ---------- 真实音频格式（不信扩展名）----------
//
// 本机曲库 360/362 个文件是「.mp3 后缀装 FLAC 数据」（某网盘下载回来的老货）。
// 浏览器播放时会嗅探文件头，所以网页里一切正常 —— 但 Substreamer / ExoPlayer 这类
// **按 Content-Type 选解码器**的播放器，看到 audio/mpeg 就交给 MP3 解析器，
// 解 FLAC 直接报错，表现为「点了播放没反应 / 报错」（2026-11-06 用户实测）。
//
// 所以这里按文件头真实格式吐 suffix / contentType，播放器才会挑对解码器。
// 结果按「大小 + 修改时间」缓存：换文件自然失效，不用手工清。
type sxFmtEntry struct {
	size, mtime   int64
	suffix, ctype string
}

var sxFmtCache sync.Map // key: root\x00path

func sxAudioFmt(t *musicTrack) (string, string) {
	key := t.Root + "\x00" + t.Path
	if v, ok := sxFmtCache.Load(key); ok {
		if e := v.(sxFmtEntry); e.size == t.Size && e.mtime == t.Mtime {
			return e.suffix, e.ctype
		}
	}
	suf, ct := sxProbeAudio(t.full(), t.Path)
	sxFmtCache.Store(key, sxFmtEntry{t.Size, t.Mtime, suf, ct})
	return suf, ct
}

// sxProbeAudio 读前 12 字节认容器；不认识就退回扩展名。
func sxProbeAudio(full, path string) (string, string) {
	if f, err := os.Open(full); err == nil {
		var h [12]byte
		n, _ := f.Read(h[:])
		f.Close()
		if n >= 8 {
			switch {
			case h[0] == 'f' && h[1] == 'L' && h[2] == 'a' && h[3] == 'C':
				return "flac", "audio/flac"
			case h[0] == 'R' && h[1] == 'I' && h[2] == 'F' && h[3] == 'F' && h[8] == 'W':
				return "wav", "audio/wav"
			case h[4] == 'f' && h[5] == 't' && h[6] == 'y' && h[7] == 'p':
				return "m4a", "audio/mp4"
			case h[0] == 'O' && h[1] == 'g' && h[2] == 'g' && h[3] == 'S':
				return "ogg", "audio/ogg"
			case h[0] == 'M' && h[1] == 'A' && h[2] == 'C' && h[3] == ' ':
				return "ape", "audio/x-ape"
			case h[0] == 'D' && h[1] == 'S' && h[2] == 'D' && h[3] == ' ':
				return "dsf", "audio/x-dsf"
			}
		}
	}
	ext := sxSuffix(path)
	return ext, sxContentType(ext)
}

// sxContentType 收「后缀」而不是路径：调用方常常已经知道真实后缀（探过文件头），
// 再传路径进来会被 sxSuffix 的 Ext() 判成空 → 全变 audio/mpeg。
func sxContentType(suf string) string {
	switch strings.ToLower(strings.TrimPrefix(suf, ".")) {
	case "mp3":
		return "audio/mpeg"
	case "flac":
		return "audio/flac"
	case "m4a", "mp4", "aac":
		return "audio/mp4"
	case "ogg", "oga", "opus":
		return "audio/ogg"
	case "wav":
		return "audio/wav"
	case "wma":
		return "audio/x-ms-wma"
	case "ape":
		return "audio/x-ape"
	case "dsf", "dff":
		return "audio/x-dsf"
	case "aiff", "aif":
		return "audio/aiff"
	default:
		return "audio/mpeg"
	}
}

func sxSongOf(t *musicTrack) sxSong {
	albumID := ""
	if t.showAlbum() != "" {
		albumID = sxAlbumIDOf(t.showArtist(), t.showAlbum())
	}
	// 封面统一用专辑 id：专辑内任一首有图（或目录里有 cover.jpg）都能出图，
	// 比逐首判断 hasCover 稳（同专辑里常有一两首没内嵌图）
	cover := albumID
	// 磁盘号缺省按 1 报：单碟专辑本来就该是 disc 1，报 0 会让部分 App 把它归到
	// 「未知碟」里，多碟专辑的排序也会乱。音轨号在 sxNumberSongs 里统一补。
	disc := t.DiscNo
	if disc <= 0 {
		disc = 1
	}
	// suffix / contentType 按**文件头真实格式**报（不是扩展名），否则 FLAC 装成 .mp3
	// 的文件会让 App 挑错解码器，点播放直接报错。见 sxAudioFmt 注释。
	suf, ct := sxAudioFmt(t)
	return sxSong{
		ID: sxTrackID(t), Parent: albumID, IsDir: false,
		Title: t.Title, Album: t.showAlbum(), Artist: t.showArtist(),
		Track: t.TrackNo, Year: t.Year, CoverArt: cover,
		Size: t.Size, ContentType: ct, Suffix: suf,
		Duration: int(t.Duration + 0.5), Path: t.Path,
		AlbumID: albumID, ArtistID: sxArtistID(t.showArtist()),
		Type: "music", DiscNumber: disc,
	}
}

// sxDropFirst 切掉前 n 个元素，实现 Subsonic 的 offset 语义。n<=0 原样返回。
func sxDropFirst[T any](list []T, n int) []T {
	if n <= 0 {
		return list
	}
	if n >= len(list) {
		return list[:0]
	}
	return list[n:]
}

// sxNumberSongs 给没有音轨号的曲目补一个「专辑内序号」。
//
// 为什么必须补：很多 MP3 没有 ID3 音轨号，music_track.track_no 就是 0，而 sxSong 上
// Track 带 omitempty，等于整个字段不返回。Substreamer 这类 App 把 (专辑, 音轨号)
// 当本地 songs 表里的排序/唯一键 —— 两首都是 0 时后一首会覆盖前一首，
// 表现就是「专辑显示 2 首，点进去只有 1 首」。实测撞到过（两首同名不同版本的未打标 mp3）。
//
// 两趟：先把每组已有的最大音轨号记下来，再给 0 的往上接，避免和真实编号撞车。
// 同一响应内序号稳定（顺序由调用方的确定性排序决定），跨响应可能不同 ——
// 但音轨号只用于展示与排序，不承担身份，不影响缓存。
func sxNumberSongs(list []sxSong) []sxSong {
	if len(list) == 0 {
		return list
	}
	key := func(s sxSong) string {
		if s.AlbumID != "" {
			return s.AlbumID
		}
		return "album\x00" + s.Album + "\x00" + s.Artist
	}
	max := map[string]int{}
	for _, s := range list {
		if s.Track > max[key(s)] {
			max[key(s)] = s.Track
		}
	}
	for i := range list {
		if list[i].Track == 0 {
			k := key(list[i])
			max[k]++
			list[i].Track = max[k]
		}
	}
	return list
}

// sxGroupAlbums 按 (showArtist, showAlbum) 聚合，排序：歌手 → 最早年份 → 专辑名。
func sxGroupAlbums(tracks []*musicTrack) []musicAlbum {
	byKey := map[string]*musicAlbum{}
	for _, t := range tracks {
		key := t.showArtist() + "\x00" + t.showAlbum()
		a := byKey[key]
		if a == nil {
			a = &musicAlbum{Artist: t.showArtist(), Album: t.showAlbum(), Root: t.Root, Path: t.Path}
			byKey[key] = a
		}
		a.Tracks++
		a.Duration += t.Duration
		a.HasCover = a.HasCover || t.HasCover
		if t.Year > 0 && (a.Year == 0 || t.Year < a.Year) {
			a.Year = t.Year
		}
	}
	out := make([]musicAlbum, 0, len(byKey))
	for _, a := range byKey {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Artist != out[j].Artist {
			return out[i].Artist < out[j].Artist
		}
		if out[i].Year != out[j].Year {
			return out[i].Year < out[j].Year
		}
		return out[i].Album < out[j].Album
	})
	return out
}

func sxGroupArtists(tracks []*musicTrack) []musicArtist {
	byName := map[string]*musicArtist{}
	seen := map[string]bool{}
	for _, t := range tracks {
		n := t.showArtist()
		a := byName[n]
		if a == nil {
			a = &musicArtist{Name: n}
			byName[n] = a
		}
		a.Tracks++
		key := n + "\x00" + t.showAlbum()
		if !seen[key] {
			seen[key] = true
			a.Albums++
		}
	}
	out := make([]musicArtist, 0, len(byName))
	for _, a := range byName {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sxAlbumItem(a musicAlbum) sxAlbumID {
	return sxAlbumID{
		ID: sxAlbumIDOf(a.Artist, a.Album), Name: a.Album,
		Artist: a.Artist, ArtistID: sxArtistID(a.Artist),
		CoverArt:  sxAlbumIDOf(a.Artist, a.Album),
		SongCount: a.Tracks, Duration: int(a.Duration + 0.5),
		Created: time.Now().Format(time.RFC3339), Year: a.Year,
	}
}

func sxTracksOfAlbum(tracks []*musicTrack, artist, album string) []*musicTrack {
	var out []*musicTrack
	for _, t := range tracks {
		if t.showArtist() == artist && t.showAlbum() == album {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DiscNo != out[j].DiscNo {
			return out[i].DiscNo < out[j].DiscNo
		}
		if out[i].TrackNo != out[j].TrackNo {
			return out[i].TrackNo < out[j].TrackNo
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// ---------- 入口 ----------

func handleSubsonic(w http.ResponseWriter, r *http.Request) {
	ep := strings.TrimPrefix(r.URL.Path, "/rest/")
	// 客户端五花八门：多数 App 让你填「服务器地址」然后自己拼 /rest，少数要你填全路径。
	// 用户在 App 里把 http://host:port/rest 整条填进去，就会变成 /rest/rest/ping ——
	// 这时候 endpoints 全都对不上，会一路落到 default 的「回 ok」，App 里表现成
	// ping 通过但列表全空（很像「登录不上」）。这里把重复的 rest/ 前缀吃掉，两种填法都认。
	for strings.HasPrefix(ep, "rest/") {
		ep = strings.TrimPrefix(ep, "rest/")
	}
	ep = strings.TrimSuffix(ep, ".view")
	ep = strings.TrimSuffix(ep, "/")

	// 两个二进制端点自己管鉴权与 Content-Type
	switch ep {
	case "stream", "download":
		sxStream(w, r)
		return
	case "getCoverArt":
		sxCoverArt(w, r)
		return
	}

	user, aerr := sxAuth(r)
	if aerr != nil {
		sxFail(w, r, aerr.Code, aerr.Msg)
		return
	}
	tracks := musicVisibleTracks(user.Name)
	q := r.URL.Query()

	switch ep {
	case "ping":
		sxWrite(w, r, &sxResponse{})

	case "getLicense":
		sxWrite(w, r, &sxResponse{License: &sxLicense{Valid: true}})

	case "getMusicFolders":
		sxWrite(w, r, &sxResponse{MusicFolders: &sxMusicFolders{
			MusicFolder: []sxMusicFolder{{ID: "1", Name: "音乐库"}},
		}})

	case "getIndexes", "getArtists":
		arts := sxGroupArtists(tracks)
		// 按首字符分组：中文归到「中文」，数字/符号归到 #
		groups := map[string][]sxArtItem{}
		var order []string
		for _, a := range arts {
			ch := "#"
			if r := []rune(a.Name); len(r) > 0 {
				c := r[0]
				if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
					ch = strings.ToUpper(string(c))
				} else if c > 127 {
					ch = "中文"
				}
			}
			if _, ok := groups[ch]; !ok {
				order = append(order, ch)
			}
			groups[ch] = append(groups[ch], sxArtItem{
				ID: sxArtistID(a.Name), Name: a.Name, AlbumCount: a.Albums,
			})
		}
		sort.Strings(order)
		idx := make([]sxIndex, 0, len(order))
		for _, ch := range order {
			idx = append(idx, sxIndex{Name: ch, Artist: groups[ch]})
		}
		if ep == "getIndexes" {
			sxWrite(w, r, &sxResponse{Indexes: &sxIndexes{Index: idx}})
		} else {
			sxWrite(w, r, &sxResponse{Artists: &sxArtists{IgnoredArticles: "The El La Los Las Le Les", Index: idx}})
		}

	case "getArtist":
		id := q.Get("id")
		name := ""
		if parts := sxDec(strings.TrimPrefix(id, "ar-")); len(parts) == 1 {
			name = parts[0]
		}
		if name == "" {
			sxFail(w, r, 10, "缺少参数 id")
			return
		}
		var mine []*musicTrack
		for _, t := range tracks {
			if t.showArtist() == name {
				mine = append(mine, t)
			}
		}
		albums := sxGroupAlbums(mine)
		detail := &sxArtist{ID: id, Name: name, AlbumCount: len(albums)}
		for _, a := range albums {
			detail.Album = append(detail.Album, sxAlbumItem(a))
		}
		sxWrite(w, r, &sxResponse{Artist: detail})

	case "getAlbum":
		id := q.Get("id")
		parts := sxDec(strings.TrimPrefix(id, "al-"))
		if len(parts) != 2 {
			sxFail(w, r, 10, "缺少参数 id")
			return
		}
		list := sxTracksOfAlbum(tracks, parts[0], parts[1])
		if len(list) == 0 {
			sxFail(w, r, 70, "专辑不存在")
			return
		}
		albums := sxGroupAlbums(list)
		out := &sxAlbum{}
		if len(albums) == 1 {
			out.sxAlbumID = sxAlbumItem(albums[0])
		} else {
			out.ID, out.Name, out.Artist, out.ArtistID = id, parts[1], parts[0], sxArtistID(parts[0])
			out.CoverArt = id
			out.Created = time.Now().Format(time.RFC3339)
		}
		for _, t := range list {
			out.Song = append(out.Song, sxSongOf(t))
		}
		out.Song = sxNumberSongs(out.Song)
		sxWrite(w, r, &sxResponse{Album: out})

	case "getAlbumList", "getAlbumList2":
		typ := q.Get("type")
		size := atoiDefault(q.Get("size"), 50)
		offset := atoiDefault(q.Get("offset"), 0)
		albums := sxGroupAlbums(tracks)
		switch typ {
		case "newest", "recent", "frequent", "highest":
			// 没有播放统计：用「最近修改」近似 newest，其余退化成同一份顺序，别报错
			sort.SliceStable(albums, func(i, j int) bool { return albums[i].Album > albums[j].Album })
		case "random":
			for i := len(albums) - 1; i > 0; i-- {
				j := sxRand(i + 1)
				albums[i], albums[j] = albums[j], albums[i]
			}
		case "alphabeticalByArtist":
			// sxGroupAlbums 已经按歌手排好了
		default: // alphabeticalByName / starred / 未知 → 按专辑名
			sort.SliceStable(albums, func(i, j int) bool { return albums[i].Album < albums[j].Album })
		}
		if offset < 0 {
			offset = 0
		}
		if offset > len(albums) {
			offset = len(albums)
		}
		end := offset + size
		if size <= 0 || end > len(albums) {
			end = len(albums)
		}
		out := &sxAlbumList{Album: []sxAlbumID{}}
		for _, a := range albums[offset:end] {
			out.Album = append(out.Album, sxAlbumItem(a))
		}
		if ep == "getAlbumList" {
			sxWrite(w, r, &sxResponse{AlbumList: out})
		} else {
			sxWrite(w, r, &sxResponse{AlbumList2: out})
		}

	case "getSong":
		t := sxTrackByID(user.Name, q.Get("id"))
		if t == nil {
			sxFail(w, r, 70, "歌曲不存在")
			return
		}
		s := sxSongOf(t)
		sxWrite(w, r, &sxResponse{Song: &s})

	case "getRandomSongs":
		size := atoiDefault(q.Get("size"), 20)
		list := append([]*musicTrack(nil), tracks...)
		for i := len(list) - 1; i > 0; i-- {
			j := sxRand(i + 1)
			list[i], list[j] = list[j], list[i]
		}
		if size <= 0 || size > len(list) {
			size = len(list)
		}
		out := &sxSongs{Song: []sxSong{}}
		for _, t := range list[:size] {
			out.Song = append(out.Song, sxSongOf(t))
		}
		out.Song = sxNumberSongs(out.Song)
		sxWrite(w, r, &sxResponse{RandomSongs: out})

	case "search", "search2", "search3":
		term := strings.ToLower(strings.TrimSpace(q.Get("query")))
		ac := atoiDefault(q.Get("artistCount"), 20)
		alc := atoiDefault(q.Get("albumCount"), 20)
		sc := atoiDefault(q.Get("songCount"), 20)
		// 分页：Subsonic 是 count + offset。只认 count 不认 offset 时，
		// Substreamer 的 search3 通道（按 1000 条翻页）会把第一页原样再拿一遍，
		// 库大时会一直重复，最后只能靠它「首条 id 与上页相同」的兜底降级成逐专辑 walk。
		ao := atoiDefault(q.Get("artistOffset"), 0)
		alo := atoiDefault(q.Get("albumOffset"), 0)
		so := atoiDefault(q.Get("songOffset"), 0)
		if ao < 0 {
			ao = 0
		}
		if alo < 0 {
			alo = 0
		}
		if so < 0 {
			so = 0
		}
		res := &sxSearch{}
		// 空 query 的语义是「匹配全部」，不是「什么都不返回」。
		// Substreamer 拿 search3.view?query=&albumCount=10&songCount=0 做能力探测，
		// 也用它在快通道里翻页同步 —— 早先这里 if term != "" 直接把整块跳过，
		// 探测一律判「不支持」，App 就退到 getAlbumList2 + 逐专辑 getAlbum 的慢通道。
		all := term == ""
		// 取够 offset+count 就停，最后把前 offset 个切掉（列表本身很小，够用了）
		for _, a := range sxGroupArtists(tracks) {
			if !(all || strings.Contains(strings.ToLower(a.Name), term)) {
				continue
			}
			if len(res.Artist) >= ao+ac {
				break
			}
			res.Artist = append(res.Artist, sxArtItem{ID: sxArtistID(a.Name), Name: a.Name, AlbumCount: a.Albums})
		}
		res.Artist = sxDropFirst(res.Artist, ao)
		{
			for _, a := range sxGroupAlbums(tracks) {
				if !(all || strings.Contains(strings.ToLower(a.Album), term) ||
					strings.Contains(strings.ToLower(a.Artist), term)) {
					continue
				}
				if len(res.Album) >= alo+alc {
					break
				}
				res.Album = append(res.Album, sxAlbumItem(a))
			}
			res.Album = sxDropFirst(res.Album, alo)
		}
		for _, t := range tracks {
			if !musicMatch(t, term) {
				continue
			}
			if len(res.Song) >= so+sc {
				break
			}
			res.Song = append(res.Song, sxSongOf(t))
		}
		res.Song = sxNumberSongs(sxDropFirst(res.Song, so))
		switch ep {
		case "search":
			sxWrite(w, r, &sxResponse{SearchResult: res})
		case "search2":
			sxWrite(w, r, &sxResponse{SearchResult2: res})
		default:
			sxWrite(w, r, &sxResponse{SearchResult3: res})
		}

	case "getGenres":
		sxWrite(w, r, &sxResponse{Genres: &sxGenres{Genre: []sxGenre{}}})

	case "getMusicDirectory":
		id := q.Get("id")
		dir := &sxDirectory{ID: id, Name: "音乐库", Child: []sxChild{}}
		if strings.HasPrefix(id, "al-") {
			parts := sxDec(strings.TrimPrefix(id, "al-"))
			if len(parts) == 2 {
				dir.Name = parts[1]
				songs := []sxSong{}
				for _, t := range sxTracksOfAlbum(tracks, parts[0], parts[1]) {
					songs = append(songs, sxSongOf(t))
				}
				for _, s := range sxNumberSongs(songs) {
					dir.Child = append(dir.Child, sxChild{
						ID: s.ID, Parent: s.Parent, IsDir: false, Title: s.Title,
						Album: s.Album, Artist: s.Artist, CoverArt: s.CoverArt,
						Duration: s.Duration, Year: s.Year, Track: s.Track, Type: "music",
					})
				}
			}
		} else {
			// 根 / 歌手：都按专辑列出，够老 App 浏览用
			for _, a := range sxGroupAlbums(tracks) {
				dir.Child = append(dir.Child, sxChild{
					ID: sxAlbumIDOf(a.Artist, a.Album), IsDir: true, Title: a.Album,
					Artist: a.Artist, CoverArt: sxAlbumIDOf(a.Artist, a.Album),
					Year: a.Year, Type: "album",
				})
			}
		}
		sxWrite(w, r, &sxResponse{Directory: dir})

	case "getPlaylists":
		// duration / changed 是规范必填项：时长走 music_track 的扫描结果求和，
		// changed 取「歌单本身 ts 与最后一次改条目 ts 的较大值」，客户端靠它判断要不要刷新。
		rows, err := db.Query(`SELECT p.id, p.name, p.ts, COUNT(i.id),
			COALESCE(SUM(t.duration), 0), COALESCE(MAX(i.ts), p.ts)
			FROM playlist p
			LEFT JOIN playlist_item i ON i.playlist_id = p.id
			LEFT JOIN music_track t ON t.root = i.root AND t.path = i.path
			WHERE p.username = ? GROUP BY p.id ORDER BY p.ts DESC`, user.Name)
		out := &sxPlaylists{Playlist: []sxPlaylist{}}
		if err == nil {
			for rows.Next() {
				var id, ts, chg int64
				var name string
				var cnt int
				var dur float64
				if rows.Scan(&id, &name, &ts, &cnt, &dur, &chg) == nil {
					out.Playlist = append(out.Playlist, sxPlaylist{
						ID: sxPlaylistID(id), Name: name, Owner: user.Name,
						SongCount: cnt, Duration: int(dur + 0.5),
						Created: time.Unix(ts, 0).Format(time.RFC3339),
						Changed: time.Unix(chg, 0).Format(time.RFC3339),
					})
				}
			}
			rows.Close()
		}
		sxWrite(w, r, &sxResponse{Playlists: out})

	case "getPlaylist":
		id := sxPlaylistDBID(q.Get("id"))
		if id <= 0 || !sxOwnPlaylist(user.Name, id) {
			sxFail(w, r, 70, "歌单不存在")
			return
		}
		out := &sxPlaylist{ID: sxPlaylistID(id), Owner: user.Name}
		var pTs, pChg int64
		_ = db.QueryRow(`SELECT p.name, p.ts,
			COALESCE((SELECT MAX(ts) FROM playlist_item WHERE playlist_id = p.id), p.ts)
			FROM playlist p WHERE p.id = ?`, id).Scan(&out.Name, &pTs, &pChg)
		out.Created = time.Unix(pTs, 0).Format(time.RFC3339)
		out.Changed = time.Unix(pChg, 0).Format(time.RFC3339)
		type ref struct{ root, path string }
		var refs []ref
		rows, err := db.Query(`SELECT root, path FROM playlist_item WHERE playlist_id = ? ORDER BY pos`, id)
		if err == nil {
			for rows.Next() {
				var r ref
				if rows.Scan(&r.root, &r.path) == nil {
					refs = append(refs, r)
				}
			}
			rows.Close()
		}
		byKey := map[string]*musicTrack{}
		for _, t := range tracks {
			byKey[t.Root+"\x00"+t.Path] = t
		}
		for _, r := range refs {
			t := byKey[r.root+"\x00"+r.path]
			if t == nil {
				continue // 白名单外/已删：歌单里静默跳过（App 不认「失效条目」这个概念）
			}
			out.Entry = append(out.Entry, sxSongOf(t))
		}
		out.Entry = sxNumberSongs(out.Entry)
		out.SongCount = len(out.Entry)
		// 时长按「实际能播的条目」求和：白名单外/已删的文件不计，跟 songCount 口径一致
		sumDur := 0.0
		for _, e := range out.Entry {
			sumDur += float64(e.Duration)
		}
		out.Duration = int(sumDur + 0.5)
		sxWrite(w, r, &sxResponse{Playlist: out})

	case "createPlaylist":
		pid := sxPlaylistDBID(q.Get("playlistId"))
		name := strings.TrimSpace(q.Get("name"))
		ids := q["songId"]
		var id int64
		if pid > 0 && sxOwnPlaylist(user.Name, pid) {
			id = pid
			if name != "" {
				_, _ = db.Exec(`UPDATE playlist SET name = ? WHERE id = ?`, name, pid)
			}
			_, _ = db.Exec(`DELETE FROM playlist_item WHERE playlist_id = ?`, pid)
		} else {
			if name == "" {
				name = "未命名歌单"
			}
			res, err := db.Exec(`INSERT INTO playlist (username, name, ts) VALUES (?,?,?)`,
				user.Name, name, time.Now().Unix())
			if err != nil {
				sxFail(w, r, 0, "创建歌单失败")
				return
			}
			id, _ = res.LastInsertId()
		}
		for i, sid := range ids {
			t := sxTrackByID(user.Name, sid)
			if t == nil {
				continue
			}
			_, _ = db.Exec(`INSERT INTO playlist_item (playlist_id, root, path, pos, ts) VALUES (?,?,?,?,?)`,
				id, t.Root, t.Path, i, time.Now().Unix())
		}
		flush()
		sxWrite(w, r, &sxResponse{})

	case "updatePlaylist":
		id := sxPlaylistDBID(q.Get("playlistId"))
		if id <= 0 || !sxOwnPlaylist(user.Name, id) {
			sxFail(w, r, 70, "歌单不存在")
			return
		}
		if name := strings.TrimSpace(q.Get("name")); name != "" {
			_, _ = db.Exec(`UPDATE playlist SET name = ? WHERE id = ?`, name, id)
		}
		for _, sid := range q["songIdToAdd"] {
			t := sxTrackByID(user.Name, sid)
			if t == nil {
				continue
			}
			var max int
			_ = db.QueryRow(`SELECT COALESCE(MAX(pos), -1) FROM playlist_item WHERE playlist_id = ?`, id).Scan(&max)
			_, _ = db.Exec(`INSERT INTO playlist_item (playlist_id, root, path, pos, ts) VALUES (?,?,?,?,?)`,
				id, t.Root, t.Path, max+1, time.Now().Unix())
		}
		// 删除序号要从大到小删，否则删一个后面全部前移、越删越偏
		rm := []int{}
		for _, s := range q["songIndexToRemove"] {
			if n, err := strconv.Atoi(s); err == nil {
				rm = append(rm, n)
			}
		}
		sort.Sort(sort.Reverse(sort.IntSlice(rm)))
		for _, idx := range rm {
			var itemID int64
			err := db.QueryRow(`SELECT id FROM playlist_item WHERE playlist_id = ? ORDER BY pos LIMIT 1 OFFSET ?`,
				id, idx).Scan(&itemID)
			if err == nil {
				_, _ = db.Exec(`DELETE FROM playlist_item WHERE id = ?`, itemID)
			}
		}
		// 重排 pos，保持 0..n-1 连续
		sxRenumberPlaylist(id)
		flush()
		sxWrite(w, r, &sxResponse{})

	case "deletePlaylist":
		id := sxPlaylistDBID(q.Get("id"))
		if id <= 0 || !sxOwnPlaylist(user.Name, id) {
			sxFail(w, r, 70, "歌单不存在")
			return
		}
		_, _ = db.Exec(`DELETE FROM playlist WHERE id = ?`, id)
		_, _ = db.Exec(`DELETE FROM playlist_item WHERE playlist_id = ?`, id)
		flush()
		sxWrite(w, r, &sxResponse{})

	case "getScanStatus":
		musicScan.mu.Lock()
		st := &sxScanStatus{Scanning: musicScan.running, Count: int64(len(tracks))}
		if musicScan.lastScan > 0 {
			st.LastScan = time.Unix(musicScan.lastScan, 0).Format(time.RFC3339)
		}
		musicScan.mu.Unlock()
		sxWrite(w, r, &sxResponse{ScanStatus: st})

	case "startScan":
		musicStartScan()
		musicScan.mu.Lock()
		st := &sxScanStatus{Scanning: musicScan.running, Count: int64(len(tracks))}
		if musicScan.lastScan > 0 {
			st.LastScan = time.Unix(musicScan.lastScan, 0).Format(time.RFC3339)
		}
		musicScan.mu.Unlock()
		sxWrite(w, r, &sxResponse{ScanStatus: st})

	case "getUser":
		u := strings.TrimSpace(q.Get("username"))
		if u == "" {
			u = user.Name
		}
		isAdmin := scopedNormalizeUser(u) == scopedNormalizeUser(user.Name) && user.Admin
		sxWrite(w, r, &sxResponse{User: &sxUserInfo{
			Username: u, ScrobblingEnabled: false, AdminRole: isAdmin,
			PlaylistRole: true, DownloadRole: true, CoverArtRole: true, StreamRole: true,
		}})

	case "getLyrics":
		// 歌词按 songId 取；老 App 用 artist/title，拿不到就回空歌词，不报错
		ly := &sxLyrics{}
		if t := sxTrackByID(user.Name, q.Get("id")); t != nil {
			ly.Artist, ly.Title = t.showArtist(), t.Title
			if txt := readSidecarLRC(t.full()); txt != "" {
				ly.Value = txt
			} else if txt := readTagLyrics(t.full()); txt != "" {
				ly.Value = txt
			}
		}
		sxWrite(w, r, &sxResponse{Lyrics: ly})

	default:
		// 未实现的端点（star/unstar/scrobble/setRating/getStarred2/savePlayQueue…）：
		// 一律 ok。App 收到 error 会弹「服务器不支持」，收到 ok 就当成功——收藏/评分
		// 这些非核心功能静默丢弃，比让整个 App 报错划算。
		sxWrite(w, r, &sxResponse{})
	}
}

// ---------- 二进制端点 ----------

func sxStream(w http.ResponseWriter, r *http.Request) {
	user, aerr := sxAuth(r)
	if aerr != nil {
		sxFail(w, r, aerr.Code, aerr.Msg)
		return
	}
	t := sxTrackByID(user.Name, r.URL.Query().Get("id"))
	if t == nil {
		// 播放失败排查全靠这几行：App 只报「无法播放」，服务端得留下是 id 对不上、
		// 还是文件打不开。id 太长只打前 24 字符，够定位是哪个用户/哪种 id 形态。
		id := r.URL.Query().Get("id")
		if len(id) > 24 {
			id = id[:24] + "…"
		}
		log.Printf("[subsonic] 播放失败 %s %s：id=%q 不在可播范围（账号 %s）",
			clientIP(r), uaTag(r.UserAgent()), id, user.Name)
		sxFail(w, r, 70, "歌曲不存在")
		return
	}
	full := t.full()
	f, err := os.Open(full)
	if err != nil {
		log.Printf("[subsonic] 播放失败 %s：打开 %s 出错 %v", uaTag(r.UserAgent()), full, err)
		sxFail(w, r, 70, "文件打不开")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		sxFail(w, r, 70, "文件不可读")
		return
	}
	suf, ct := sxAudioFmt(t)
	log.Printf("[subsonic] 播放 %s %s → %s/%s %.1fMB",
		uaTag(r.UserAgent()), t.Path, suf, ct, float64(t.Size)/(1<<20))
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Accept-Ranges", "bytes")
	// App 名字 + 文件名：安卓下载管理器会拿它当显示名。
	// 后缀回**真实容器**（flac 不是 mp3），否则存到本地还是打不开。
	name := t.Title + "." + suf
	// 下载端点给 attachment（安卓下载管理器会拿它当显示名）；
	// 播放端点必须 inline —— 少数客户端/Safari 把 attachment 当「另存为」，
	// 播放器拿到的是一个下载任务而不是媒体流，表现成「点了没反应」。
	disp := "inline"
	if strings.Contains(r.URL.Path, "download") {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", disp+"; filename*=UTF-8''"+urlEncodePath(name))
	// ServeContent 自带 Range / If-Modified-Since，断点续传与拖动进度条都靠它
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func sxCoverArt(w http.ResponseWriter, r *http.Request) {
	user, aerr := sxAuth(r)
	if aerr != nil {
		sxFail(w, r, aerr.Code, aerr.Msg)
		return
	}
	id := r.URL.Query().Get("id")
	var full string
	switch {
	case strings.HasPrefix(id, "tr-"):
		if t := sxTrackByID(user.Name, id); t != nil {
			full = t.full()
		}
	case strings.HasPrefix(id, "al-"):
		parts := sxDec(strings.TrimPrefix(id, "al-"))
		if len(parts) == 2 {
			list := sxTracksOfAlbum(musicVisibleTracks(user.Name), parts[0], parts[1])
			if len(list) > 0 {
				full = list[0].full()
			}
		}
	case strings.HasPrefix(id, "ar-"):
		parts := sxDec(strings.TrimPrefix(id, "ar-"))
		if len(parts) == 1 {
			for _, t := range musicVisibleTracks(user.Name) {
				if t.showArtist() == parts[0] {
					full = t.full()
					break
				}
			}
		}
	}
	if full == "" {
		http.Error(w, "没有封面", http.StatusNotFound)
		return
	}
	st, err := os.Stat(full)
	if err != nil || st.IsDir() || cacheDir == "" {
		http.Error(w, "没有封面", http.StatusNotFound)
		return
	}
	key := cacheKey(full, st, "cover640")
	target := coverPath(key)
	if _, err := os.Stat(target); err != nil || st.Size() == 0 {
		data := readCoverBytes(full)
		if data == nil {
			data = readDirCover(filepath.Dir(full))
		}
		if data == nil {
			http.Error(w, "没有封面", http.StatusNotFound)
			return
		}
		_ = os.MkdirAll(filepath.Dir(target), 0o755)
		if err := writeCoverJPEG(data, target, coverMaxSide); err != nil {
			_ = os.WriteFile(target, data, 0o644)
		}
	}
	serveThumb(w, r, target)
}

// ---------- 小工具 ----------

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func sxPlaylistDBID(s string) int64 {
	s = strings.TrimPrefix(strings.TrimSpace(s), "pl-")
	id, _ := strconv.ParseInt(s, 10, 64)
	return id
}

func sxOwnPlaylist(user string, id int64) bool {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM playlist WHERE id = ? AND username = ?`, id, user).Scan(&n)
	return n > 0
}

func sxRenumberPlaylist(id int64) {
	rows, err := db.Query(`SELECT id FROM playlist_item WHERE playlist_id = ? ORDER BY pos`, id)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var x int64
		if rows.Scan(&x) == nil {
			ids = append(ids, x)
		}
	}
	rows.Close()
	// 顺便把 ts 刷新成「现在」：changed 字段取的是 MAX(item.ts)，
	// 只改 pos 不改 ts 的话，App 里删一首歌后 changed 不变，客户端按缓存不会刷新。
	now := time.Now().Unix()
	for i, x := range ids {
		_, _ = db.Exec(`UPDATE playlist_item SET pos = ?, ts = ? WHERE id = ?`, i, now, x)
	}
}

// sxRand 给 random 类型用：不引 math/rand 的全局源，避免并发偏好问题（对随机性没要求）
func sxRand(n int) int {
	if n <= 1 {
		return 0
	}
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return int(time.Now().UnixNano()) % n
	}
	return int(uint16(b[0])<<8|uint16(b[1])) % n
}

func urlEncodePath(s string) string {
	// RFC 5987 的 filename* 只保证 ASCII 以外的字符被百分号编码
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}

// handleSubsonicInfo /api/music/appinfo：管理台「手机 App 连接」卡片的信息源。
//
// 只给地址和状态提示，不返回任何凭据/密码副本。地址沿用二维码那套 host 推断
// （局域网 IP 优先 → Tailscale → 本机），手机和电脑在同一个网里就能直连。
//
// adminReady 表示 admin 的密码副本（passEnc）在不在：Subsonic 的 token 鉴权是
// md5(明文+salt)，而库里只存 PBKDF2/NT hash，算不出来 —— 缺副本时 App 会报
// 「账号或密码错误」，得先在网页上登录一次把副本补出来。
//
// 实际上 admin 进这个页面时必定已经登录过（登录那一刻就补了副本），所以更值得
// 提示的是 pending 个受限账号：它们建好以后如果没人用网页登录过，App 里连不上。
func handleSubsonicInfo(w http.ResponseWriter, r *http.Request) {
	host := qrHost()
	cfgMu.Lock()
	port := effPort
	ready := cfg.PassEnc != ""
	scopedN := len(cfg.ScopedAccounts)
	pending := 0
	for _, a := range cfg.ScopedAccounts {
		if a.PassEnc == "" {
			pending++
		}
	}
	cfgMu.Unlock()
	// 给的是**基础地址**（不带 /rest）：多数 App（Substreamer、play:Sub、Amperfe…）
	// 自己会拼 /rest，整条带进去就变成 /rest/rest。两种填法服务端都兼容，
	// 但默认给不带的那种，少一层困惑。
	base := hostURL(host, "")
	writeJSON(w, 0, "ok", map[string]any{
		"url":        base,
		"restURL":    base + "/rest",
		"host":       host,
		"port":       port,
		"lan":        lanIPv4(),
		"adminReady": ready,
		"scoped":     scopedN,
		"pending":    pending,
	})
}
