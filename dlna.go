package main

import (
	"encoding/hex"
	"encoding/xml"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 「把视频推到电视上」——DLNA/UPnP 方案。
//
// 原理：几乎所有智能电视/安卓盒子都内置 DLNA 渲染器（一个等在局域网里的 UPnP 服务）。
// 流程三步：
//   1. SSDP 发现 —— 组播 M-SEARCH，谁家电视开着就应答「我有 AVTransport 服务」+ 描述文档地址；
//   2. 拉描述文档 —— 从里面拿到 friendlyName（电视名）和 AVTransport 的 controlURL；
//   3. SOAP 调用 —— SetAVTransportURI 把视频直链塞给电视，再 Play 让它开演。
//
// 视频本体不走我们中转：给电视一条签名直链（复用分享那套 /f?p=...，支持 Range 拖进度），
// 电视自己从 selfnas 拉 —— 局域网内带宽直连，播放器是电视自带的（硬解）。

const (
	dlnaST         = "urn:schemas-upnp-org:service:AVTransport:1"
	ssdpAddr       = "239.255.255.250:1900"
	dlnaCacheTTL   = 2 * time.Minute
	dlnaDiscoverMS = 3 * time.Second // SSDP 收响应窗口（别学我：3000 不是 3 秒，是 3 微秒）
)

type dlnaDevice struct {
	UDN  string `json:"udn"`
	Name string `json:"name"`
	// 以下不对外：SOAP 调用要用的地址
	controlURL string // AVTransport 控制地址（绝对）
}

var (
	dlnaMu    sync.Mutex
	dlnaCache = map[string]*dlnaEntry{} // udn → 设备（常驻监听 + 扫描双路汇入）
	dlnaScan  time.Time
)

type dlnaEntry struct {
	dev      *dlnaDevice
	lastSeen time.Time
}

// dlnaSeen 记一笔「这台设备还活着」。首次见到就异步拉描述文档。
func dlnaSeen(usn, location string) {
	key := dlnaUDN(usn, location)
	dlnaMu.Lock()
	if e, ok := dlnaCache[key]; ok {
		e.lastSeen = time.Now()
		dlnaMu.Unlock()
		return
	}
	dlnaMu.Unlock()
	// 描述文档要发 HTTP，别堵住监听循环
	go func() {
		name, ctrl, err := dlnaDescribe(location)
		if err != nil || ctrl == "" {
			return // 不是渲染器（没 AVTransport）或描述残缺，不要
		}
		dlnaMu.Lock()
		dlnaCache[key] = &dlnaEntry{
			dev:      &dlnaDevice{UDN: key, Name: name, controlURL: ctrl},
			lastSeen: time.Now(),
		}
		dlnaMu.Unlock()
		log.Printf("[DLNA] 监听到渲染器上线: %s (%s)", name, location)
	}()
}

// dlnaListenStart 常驻挂在 SSDP 组播组上收 NOTIFY —— 国产盒子大多不应答 M-SEARCH，
// 只在开机/服务启动时广播几声，之后静默几十分钟。靠常驻监听才能在它上线的瞬间逮到。
func dlnaListenStart() {
	go func() {
		gaddr, err := net.ResolveUDPAddr("udp4", ssdpAddr)
		if err != nil {
			return
		}
		for { // 组播 socket 出错就重挂，服务活着监听就得活着
			mpc, err := net.ListenMulticastUDP("udp4", nil, gaddr)
			if err != nil {
				log.Printf("[DLNA] 组播监听失败，10s 后重试: %v", err)
				time.Sleep(10 * time.Second)
				continue
			}
			buf := make([]byte, 4096)
			for {
				n, _, err := mpc.ReadFrom(buf)
				if err != nil {
					mpc.Close()
					break
				}
				if !strings.HasPrefix(strings.ToUpper(string(buf[:min(n, 8)])), "NOTIFY") {
					continue
				}
				var usn, location, nts string
				for _, ln := range strings.Split(string(buf[:n]), "\r\n") {
					i := strings.IndexByte(ln, ':')
					if i < 0 {
						continue
					}
					switch strings.ToUpper(ln[:i]) {
					case "USN":
						usn = strings.TrimSpace(ln[i+1:])
					case "LOCATION":
						location = strings.TrimSpace(ln[i+1:])
					case "NTS":
						nts = strings.TrimSpace(ln[i+1:])
					}
				}
				if usn == "" || location == "" || nts != "ssdp:alive" {
					continue
				}
				dlnaSeen(usn, location)
			}
		}
	}()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// dlnaDevices 返回当前已知的渲染器：常驻监听攒下的 + 现场问一圈（M-SEARCH）的。
// force=false 且缓存还新鲜时直接回缓存，省 3 秒等待。
func dlnaDevices(force bool) []*dlnaDevice {
	dlnaMu.Lock()
	fresh := time.Since(dlnaScan) < dlnaCacheTTL && len(dlnaCache) > 0
	dlnaMu.Unlock()
	if !force && fresh {
		return dlnaList()
	}
	found := dlnaDiscover()
	dlnaMu.Lock()
	dlnaScan = time.Now()
	// 扫描结果并入缓存（常驻监听可能已经有它了，取并集）
	for _, d := range found {
		if e, ok := dlnaCache[d.UDN]; ok {
			e.lastSeen = time.Now()
			continue
		}
		dlnaCache[d.UDN] = &dlnaEntry{dev: d, lastSeen: time.Now()}
	}
	// 太久没露面的（1 小时）当它下线清掉
	for k, e := range dlnaCache {
		if time.Since(e.lastSeen) > time.Hour {
			delete(dlnaCache, k)
		}
	}
	dlnaMu.Unlock()
	return dlnaList()
}

func dlnaList() []*dlnaDevice {
	dlnaMu.Lock()
	defer dlnaMu.Unlock()
	out := make([]*dlnaDevice, 0, len(dlnaCache))
	for _, e := range dlnaCache {
		out = append(out, e.dev)
	}
	return out
}

func dlnaDeviceByUDN(udn string) *dlnaDevice {
	dlnaMu.Lock()
	defer dlnaMu.Unlock()
	if e, ok := dlnaCache[udn]; ok {
		return e.dev
	}
	return nil
}

// dlnaDiscover 找局域网内的 DLNA 渲染器，两条路一起走：
//   1. M-SEARCH 主动问 —— 发三个 ST（AVTransport / MediaRenderer / rootdevice），
//      有些设备对具体 ST 不应答只认 rootdevice，多问几声不亏；
//   2. 同时加入组播组听 NOTIFY —— 很多国产盒子/电视不答 M-SEARCH，只主动广播 alive。
// 拿到候选后逐台拉描述文档，过滤出真有 AVTransport 服务的。
func dlnaDiscover() []*dlnaDevice {
	log.Printf("[DLNA] 开始扫描…")
	gaddr, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		log.Printf("[DLNA] 解析组播地址失败: %v", err)
		return nil
	}
	// 组播监听：收 NOTIFY（设备主动广播）。收不到就当只靠 M-SEARCH。
	mpc, err := net.ListenMulticastUDP("udp4", nil, gaddr)
	if err != nil {
		log.Printf("[DLNA] 加入组播组失败（NOTIFY 收不到了）: %v", err)
	}
	if mpc != nil {
		defer mpc.Close()
	}
	// 单播 socket：发 M-SEARCH、收应答
	pc, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		log.Printf("[DLNA] SSDP 套接字建立失败: %v", err)
		return nil
	}
	defer pc.Close()

	// 三种 ST 各问一遍，间隔 300ms —— 同一 ST 问多遍设备也只答一次
	searches := []string{dlnaST, "urn:schemas-upnp-org:device:MediaRenderer:1", "upnp:rootdevice"}
	go func() {
		for _, st := range searches {
			msg := strings.Join([]string{
				"M-SEARCH * HTTP/1.1",
				"HOST: " + ssdpAddr,
				`MAN: "ssdp:discover"`,
				"MX: 2",
				"ST: " + st,
				"", "",
			}, "\r\n")
			if _, err := pc.WriteTo([]byte(msg), gaddr); err != nil {
				log.Printf("[DLNA] M-SEARCH(%s) 发送失败: %v", st, err)
			}
			time.Sleep(300 * time.Millisecond)
		}
	}()

	// usn/location 候选收集：M-SEARCH 应答 + NOTIFY 主动广播都往里扔
	type loc struct{ usn, loc string }
	locs := map[string]loc{}
	deadline := time.Now().Add(dlnaDiscoverMS)
	buf := make([]byte, 4096)
	replies := 0
	collect := func(n int) {
		replies++
		var stnt, usn, location string
		for _, ln := range strings.Split(string(buf[:n]), "\r\n") {
			i := strings.IndexByte(ln, ':')
			if i < 0 {
				continue
			}
			// M-SEARCH 应答带 ST，NOTIFY 广播带 NT —— 都是「这是什么服务」
			head := strings.ToUpper(ln[:i])
			if head == "ST" || head == "NT" {
				if stnt == "" {
					stnt = strings.TrimSpace(ln[i+1:])
				}
			} else if head == "USN" {
				usn = strings.TrimSpace(ln[i+1:])
			} else if head == "LOCATION" {
				location = strings.TrimSpace(ln[i+1:])
			}
		}
		if location == "" || usn == "" {
			return
		}
		// 只关心渲染器相关或 root（root 的后面拉描述再过滤，路由器/打印机会在这步被淘汰）
		if stnt != "" && !strings.Contains(stnt, "AVTransport") &&
			!strings.Contains(stnt, "MediaRenderer") && !strings.Contains(stnt, "rootdevice") {
			return
		}
		// 一台设备会广播好几条 ST（uuid::MediaRenderer / ::rootdevice / 裸 uuid），
		// key 用 uuid（或 location），不然后面会推成 3 台「同名电视」
		key := dlnaUDN(usn, location)
		if _, dup := locs[key]; !dup {
			locs[key] = loc{usn, location}
			log.Printf("[DLNA] 候选: st=%s usn=%s loc=%s", stnt, usn, location)
		}
	}
	// 两个源轮流读：单播应答 + 组播 NOTIFY，各自的 deadline 都到齐为止
	for time.Now().Before(deadline) {
		pc.SetReadDeadline(deadline)
		if n, _, err := pc.ReadFrom(buf); err == nil {
			collect(n)
		} else if !strings.Contains(err.Error(), "i/o timeout") {
			log.Printf("[DLNA] SSDP 单播收包提前结束: %v", err)
			break
		}
		if mpc != nil {
			mpc.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			if n, _, err := mpc.ReadFrom(buf); err == nil {
				collect(n)
			}
		}
	}

	out := make([]*dlnaDevice, 0, len(locs))
	for usn, l := range locs {
		name, ctrl, err := dlnaDescribe(l.loc)
		if err != nil || ctrl == "" {
			// rootdevice 应答的设备不一定带 AVTransport（比如路由器/打印机），静默跳过
			if strings.Contains(usn, "AVTransport") || strings.Contains(usn, "MediaRenderer") {
				log.Printf("[DLNA] 描述文档拿不全 %s (%s): %v", usn, l.loc, err)
			}
			continue
		}
		log.Printf("[DLNA] 发现渲染器 %s (%s)", name, l.loc)
		out = append(out, &dlnaDevice{
			UDN:        dlnaUDN(usn, l.loc),
			Name:       name,
			controlURL: ctrl,
		})
	}
	log.Printf("[DLNA] 扫描完成: 应答/广播 %d 条, 渲染器 %d 台", replies, len(out))
	return out
}

// dlnaUDN 设备唯一键：USN 里的 uuid 最稳；拿不到就用 location。
func dlnaUDN(usn, location string) string {
	for _, seg := range strings.Split(usn, "::") {
		if strings.HasPrefix(strings.ToLower(seg), "uuid:") {
			return seg
		}
	}
	return location
}

type dlnaDescXML struct {
	Device struct {
		FriendlyName string `xml:"friendlyName"`
		Services     []struct {
			ServiceType string `xml:"serviceType"`
			ControlURL  string `xml:"controlURL"`
		} `xml:"serviceList>service"`
	} `xml:"device"`
	// 嵌套设备（<device><deviceList><device>…）里也可能藏着 AVTransport
	Children struct {
		Devices []dlnaDescChild `xml:"device"`
	} `xml:"deviceList"`
}

type dlnaDescChild struct {
	FriendlyName string `xml:"friendlyName"`
	Services     []struct {
		ServiceType string `xml:"serviceType"`
		ControlURL  string `xml:"controlURL"`
	} `xml:"serviceList>service"`
}

// dlnaDescribe 拉设备描述文档：拿到电视名 + AVTransport 的控制地址（转成绝对地址）。
func dlnaDescribe(location string) (name, controlURL string, err error) {
	cli := &http.Client{Timeout: 4 * time.Second}
	resp, err := cli.Get(location)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	var desc dlnaDescXML
	if err := xml.Unmarshal(body, &desc); err != nil {
		return "", "", err
	}
	if desc.Device.FriendlyName != "" {
		name = desc.Device.FriendlyName
	}
	grab := func(s struct {
		ServiceType string `xml:"serviceType"`
		ControlURL  string `xml:"controlURL"`
	}) (string, bool) {
		if strings.Contains(s.ServiceType, "AVTransport") && s.ControlURL != "" {
			return s.ControlURL, true
		}
		return "", false
	}
	for _, s := range desc.Device.Services {
		if u, ok := grab(s); ok {
			return name, dlnaAbsURL(location, u), nil
		}
	}
	for _, ch := range desc.Children.Devices {
		if name == "" && ch.FriendlyName != "" {
			name = ch.FriendlyName
		}
		for _, s := range ch.Services {
			if u, ok := grab(s); ok {
				return name, dlnaAbsURL(location, u), nil
			}
		}
	}
	if name == "" {
		name = "未知设备"
	}
	return name, "", nil
}

// dlnaAbsURL 描述文档里的 controlURL 常是相对路径，按 LOCATION 拼成绝对地址。
// ⚠️ 有些电视（「客厅」那台）的 controlURL 是 "_urn:schemas-upnp-org:service:AVTransport_control"
// 这种以名字开头带冒号的怪相对地址，url.Parse 会直接报
// "first path segment in URL cannot contain colon" —— 不能因此把坏的 ctrl 原样放行，
// 要手工按 LOCATION 所在目录把它拼成 http://host/_urn:..._control。
func dlnaAbsURL(location, ctrl string) string {
	if strings.HasPrefix(ctrl, "http://") || strings.HasPrefix(ctrl, "https://") {
		return ctrl
	}
	u, err := url.Parse(location)
	if err != nil {
		return ctrl
	}
	base := u.Scheme + "://" + u.Host
	if strings.HasPrefix(ctrl, "/") {
		return base + ctrl
	}
	idx := strings.LastIndex(u.Path, "/")
	dir := "/"
	if idx >= 0 {
		dir = u.Path[:idx+1]
	}
	return base + dir + ctrl
}

// dlnaSOAP 发一条 AVTransport 动作。UPnP 规定 body 里 & < > 都得转义。
func dlnaSOAP(dev *dlnaDevice, action, argsXML string) error {
	body := `<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body><u:` + action + ` xmlns:u="` + dlnaST + `">` +
		`<InstanceID>0</InstanceID>` + argsXML +
		`</u:` + action + `></s:Body></s:Envelope>`
	req, err := http.NewRequest(http.MethodPost, dev.controlURL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPACTION", `"`+dlnaST+`#`+action+`"`)
	cli := &http.Client{Timeout: 6 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode >= 300 {
		// UPnP 错误也在 body 里，截一段进日志方便真机排障
		return dlnaErr{code: resp.StatusCode, body: string(b)}
	}
	return nil
}

type dlnaErr struct {
	code int
	body string
}

func (e dlnaErr) Error() string {
	msg := e.body
	if i := strings.Index(msg, "<faultstring>"); i >= 0 {
		if j := strings.Index(msg[i:], "</faultstring>"); j > 0 {
			msg = msg[i+13 : i+j]
		}
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return "电视没接住（HTTP " + itoa(e.code) + "）: " + msg
}

// dlnaEscape XML 元素文本转义（URL 里的 &、标题里的 < 都会碰上）。
func dlnaEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// dlnaPush 推一条视频并让电视开演。
// linkURL 是签名直链（电视自己来拉）；title 给电视端显示。
func dlnaPush(dev *dlnaDevice, linkURL, title, mime string) error {
	didl := `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/"` +
		` xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">` +
		`<item id="0" parentID="0" restricted="0">` +
		`<dc:title>` + dlnaEscape(title) + `</dc:title>` +
		`<upnp:class>object.item.videoItem</upnp:class>` +
		`<res protocolInfo="http-get:*:` + dlnaEscape(mime) + `:*">` + dlnaEscape(linkURL) + `</res>` +
		`</item></DIDL-Lite>`
	// CurrentURIMetaData 是字符串参数，DIDL 整体要再转义一层
	args := `<CurrentURI>` + dlnaEscape(linkURL) + `</CurrentURI>` +
		`<CurrentURIMetaData>` + dlnaEscape(didl) + `</CurrentURIMetaData>`
	if err := dlnaSOAP(dev, "SetAVTransportURI", args); err != nil {
		return err
	}
	return dlnaSOAP(dev, "Play", `<Speed>1</Speed>`)
}

func dlnaStop(dev *dlnaDevice) error {
	return dlnaSOAP(dev, "Stop", "")
}

// dlnaMime 按扩展名给电视报 MIME —— 有些渲染器看见 application/octet-stream 直接拒播。
func dlnaMime(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".ts", ".m2ts":
		return "video/mp2t"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	case ".flv":
		return "video/x-flv"
	case ".wmv":
		return "video/x-ms-wmv"
	case ".mpg", ".mpeg":
		return "video/mpeg"
	case ".3gp":
		return "video/3gpp"
	default:
		return "video/mp4" // 兜底：多数渲染器对 mp4 的接受度最高
	}
}

// ---------- HTTP 接口 ----------

// GET /api/dlna/devices?refresh=1 → 局域网内的电视/盒子列表
func handleDlnaDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, "方法不对", nil)
		return
	}
	devs := dlnaDevices(r.URL.Query().Get("refresh") == "1")
	out := make([]map[string]any, 0, len(devs))
	for _, d := range devs {
		out = append(out, map[string]any{"udn": d.UDN, "name": d.Name})
	}
	writeJSON(w, 0, "", out)
}

// POST /api/dlna/play {udn, root, path, title} → 推给电视并开演
func handleDlnaPlay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "方法不对", nil)
		return
	}
	var b struct {
		UDN   string `json:"udn"`
		Root  string `json:"root"`
		Path  string `json:"path"`
		Title string `json:"title"`
	}
	if !decodeBody(r, &b) {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	// 投屏只做视频：DLNA 的 DIDL class 和 MIME 全按视频写死，
	// 音频文件（mp3/flac…）推过去会被当成 video/mp4，电视解不了。
	// 放最前面尽早拒绝，省掉一次没必要的设备扫描。
	if fileKind(b.Path, false) != "video" {
		writeJSON(w, 400, "只有视频能推到电视", nil)
		return
	}
	dev := dlnaDeviceByUDN(b.UDN)
	if dev == nil {
		// 缓存里没有（重启过 / TTL 刚刷新掉）就现扫一遍再找
		for _, d := range dlnaDevices(true) {
			if d.UDN == b.UDN {
				dev = d
				break
			}
		}
		if dev == nil {
			writeJSON(w, 404, "没找到这台电视，重新扫描看看", nil)
			return
		}
	}
	// 权限跟下载同口径：这个用户看得见这个文件才准推
	full, _, err := resolveFor(webUserOf(r), b.Root, b.Path)
	if err != nil {
		writeJSON(w, 403, "无权访问该路径", nil)
		return
	}
	// 生成签名直链（handleFSLink 同款逻辑，直接拼 URL 不经 HTTP 自调）
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		writeJSON(w, 404, "文件不存在", nil)
		return
	}
	exp := time.Now().Add(linkTTL).Unix()
	sig := signPath(full, st.Size(), exp)
	// 电视和这台设备在同一个局域网，host 用请求方看到的那个（跟直链下载一个道理）
	host := r.Host
	linkURL := "http://" + host + "/f?p=" + hex.EncodeToString([]byte(full)) +
		"&s=" + strconv.FormatInt(st.Size(), 10) + "&e=" + strconv.FormatInt(exp, 10) + "&g=" + sig
	title := strings.TrimSpace(b.Title)
	if title == "" {
		title = path.Base(strings.ReplaceAll(full, "\\", "/"))
	}
	if err := dlnaPush(dev, linkURL, title, dlnaMime(full)); err != nil {
		writeJSON(w, 502, "推送到「"+dev.Name+"」失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已推送到「"+dev.Name+"」，电视上开始播放", map[string]string{"name": dev.Name})
}

// POST /api/dlna/stop {udn} → 让电视停
func handleDlnaStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, "方法不对", nil)
		return
	}
	var b struct {
		UDN string `json:"udn"`
	}
	if !decodeBody(r, &b) {
		writeJSON(w, 400, "请求格式错误", nil)
		return
	}
	dev := dlnaDeviceByUDN(b.UDN)
	if dev == nil {
		writeJSON(w, 404, "设备不在线", nil)
		return
	}
	if err := dlnaStop(dev); err != nil {
		writeJSON(w, 502, "停止失败: "+err.Error(), nil)
		return
	}
	writeJSON(w, 0, "已停止", nil)
}
