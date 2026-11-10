package main

// 后端 EQ：把音效预设放在服务端用 ffmpeg 实时算好，前端只用一个普普通通的 <audio>
// 元素播放（也就是「加 EQ 之前」那个能正常后台/锁屏播放的元素）。
//
// 为什么放后端：前端 Web Audio 的 createMediaElementSource 把元素不可逆地改道到
// Web Audio 图，iOS 一最小化/锁屏/切后台就把 AudioContext 置成 interrupted 并暂停媒体
// 元素，且 resume() 要求用户手势 —— 后台事件里补不回来，这就是「加 EQ 之后最小化停播」
// 的根。后端做 EQ 后前端彻底没有 Web Audio，iOS 对普通 <audio> 没有任何限制。
//
// 实现：边转边发（streaming transcode）。请求进来就按 eqFilterChain 拼好滤波链，
// 把 ffmpeg 的 stdout 直接写进 HTTP 响应；同时后台跑一遍把整首落盘成成品，
// 下次同参数命中磁盘就直接 ServeFile。
//
// 为什么不是「先整首转码再返回」：那样得先给浏览器原文件、转好再换源，
// 换源瞬间必然卡一下 + 音量/音色突变（用户 2026-11-06 实测反馈）。
// 现在第一个字节就是处理后的声音，从头到尾只有一路流。
//
// 边流边 seek 的前提是 320k CBR MP3 字节率恒定 40000 B/s（LAME 用 padding 帧钉死码率），
// 所以 Content-Length = 时长 × 40000，Range: bytes=a-b 直接换算成 -ss (a/40000) -t ((b-a+1)/40000)。
// 该换算误差上限是一个帧（26ms）。流式输出必须带 -write_xing 0 -id3v2_version 0，
// 否则会多出 44 字节 ID3 头，字节↔时间映射整体偏移。
//
// ffmpeg 缺失或转码失败时优雅降级为原文件，播放绝不中断。

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EQPreset 对应前端 MP_FX 的预设；增益（dB）与整体增益(0~1) 由服务端应用。
// 顺序固定：低架80 / 中低峰300 / 中高频峰2500 / 高架8000。
// 响度均衡不在这里 —— 它是独立开关（?loud=1），可与任意 EQ 预设叠加。
type EQPreset struct {
	Key   string
	Name  string
	Gains [4]float64
	Pre   float64 // 链尾整体增益，防削波（越猛的预设压得越多）
}

// EQ_PRESETS 与前端 MP_FX 的 k/name 保持一致；g/pre 是服务端真正生效的值。
var EQ_PRESETS = map[string]EQPreset{
	"off":    {"off", "原声", [4]float64{0, 0, 0, 0}, 1.00},
	"warm":   {"warm", "温和低音", [4]float64{3, -0.5, 0, 0}, 0.95},
	"bass":   {"bass", "重低音", [4]float64{7, 2, -2, -3}, 0.70},
	"vocal":  {"vocal", "人声清晰", [4]float64{-4, -5, 4, 1}, 0.85},
	"bright": {"bright", "明亮细节", [4]float64{0, -1, 0, 6}, 0.80},
}

// 响度均衡：**只把偏小的提上来，绝不把任何歌压小声**。
//
// 2026-11-06 六改：最初学 Apple Music 归一化到 -16 LUFS，结果这个库里的歌普遍是
// -8.7 ~ -11 LUFS 的现代母带，等于每首都被压 6~7 dB，一开开关「声音突然小一大截」。
// 现在改成单向：目标 -10.5 LUFS，高于目标的歌**原样不动**（连 0.1 dB 都不压），
// 只把明显偏小的老录音往上提；提多少受真峰值余量约束，不提爆。
//
// ⚠️ 代价要记清楚：正因为在响的歌一个 dB 都不压，全库「绝对一致」做不到 ——
// 想真一致就必须压响的那批，那是用户明确否掉的。要改回双向必须先问。
const (
	loudTargetI    = -10.5 // LUFS：这个库的自然响度，偏小的才够得着
	loudTargetTP   = -0.1  // dBTP：只用来限制「往上提」的幅度，绝不拿它往下压
	loudTargetLRA  = 11.0  // 只给「分析失败」的动态兜底模式用
	loudMaxBoostDb = 12.0  // 最多提这么多，防极端素材被拉爆
	loudAlgoVer    = "v3"  // 策略版本：进缓存 key，改目标/策略必须同步改它
)

// loudGainDb 用分析遍量到的指标算一个**恒定**增益（dB）。
// 用静态 volume 而不是 loudnorm linear=true：后者为了保证 TP 会自己再让一步，
// 实际增益和算出来的对不上，还会把信号升采样到 192kHz 再拉回来（白多一遍重采样）。
// 静态增益 = 所见即所得。
func loudGainDb(m map[string]float64) float64 {
	g := loudTargetI - m["input_i"]
	if g <= 0 {
		return 0 // 已经够响：不动。这是「打开开关声音不许变小」的唯一保证
	}
	// 往上提时看真峰值还剩多少余量（母带顶在 0 dBFS 附近的歌，余量≈0）
	if room := loudTargetTP - m["input_tp"]; g > room {
		g = room
	}
	if g < 0 {
		g = 0
	}
	if g > loudMaxBoostDb {
		g = loudMaxBoostDb
	}
	return g
}

// buildLoudFilter 返回链尾的恒定增益；差值小到听不出来时返回空串（等于不加这一环）。
func buildLoudFilter(m map[string]float64) string {
	g := loudGainDb(m)
	if math.Abs(g) < 0.15 {
		return ""
	}
	return fmt.Sprintf("volume=volume=%.2fdB", g)
}

// buildDynamicLoudFilter 分析失败时的兜底：单遍动态 loudnorm（有轻微喘息感，
// 但至少不会忽大忽小）。loudnorm 固定升采样到 192kHz，后面必须拉回 44.1k。
func buildDynamicLoudFilter() string {
	return fmt.Sprintf("loudnorm=I=%g:TP=%g:LRA=%g,aresample=44100", loudTargetI, loudTargetTP, loudTargetLRA)
}

// loudMeasure 分析遍：纯解码（不编码）量出响度指标。
// pre 是会在正式转码时先跑的滤波链（EQ 预设）—— 必须一起套上！线性响度归一的
// 增益 = 目标 - 量到的响度，量的是「原文件」而增益作用在「EQ 之后」的信号上，
// 就会偏（实测 vocal+loud 会落在 -17.7 而不是 -16）。所以分析遍与转码遍滤波必须一致。
//
// 2026-11-06 九改：量测主体换成 ebur128，loudnorm 只留作兜底。
// loudnorm 算门控积分前会把信号升采样到 192kHz，纯烧 CPU 不产出一个采样：
// 同一首 Hi-Res（6:33）实测 loudnorm 6.01s / ebur128 2.19s，而两者都是 EBU R128，
// 抽三首对比 I 与真峰值差只有 0.02dB（-17.59 vs -17.6 / -3.32 vs -3.3）。
// 分析遍是「开音量一致」时用户唯一要等的那几秒，所以这一换是白赚的 2.7 倍。
func loudMeasure(ff, full, pre string) (map[string]float64, error) {
	if m, err := loudMeasureEBUR128(ff, full, pre); err == nil {
		return m, nil
	} else {
		// 老 ffmpeg 没把 ebur128 编进来、或输出格式变了：退回 loudnorm，慢但稳
		log.Printf("[EQ] ebur128 量测失败（%v），退回 loudnorm 重测", err)
	}
	return loudMeasureLoudnorm(ff, full, pre)
}

// loudMeasureEBUR128 一遍扫描同时拿到 I（整体响度）与真峰值。周期日志每 100ms 一行，
// 一首 6 分钟的歌约 4000 行，所以只用 tailBuffer 留尾巴 —— Summary 在最后。
func loudMeasureEBUR128(ff, full, pre string) (map[string]float64, error) {
	af := "ebur128=peak=true"
	if pre != "" {
		af = pre + "," + af
	}
	cmd := exec.Command(ff,
		"-hide_banner", "-nostats", "-v", "info",
		"-i", full, "-vn", "-sn", "-dn",
		"-af", af,
		"-f", "null", "-")
	tb := &tailBuffer{limit: 64 << 10}
	cmd.Stderr = tb
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return parseEBUR128(tb.String())
}

var (
	reEBURI   = regexp.MustCompile(`\bI:\s+(-?\d+(?:\.\d+)?)\s+LUFS`)
	reEBURTP  = regexp.MustCompile(`Peak:\s+(-?\d+(?:\.\d+)?)\s+dBFS`)
	reEBURLRA = regexp.MustCompile(`\bLRA:\s+(-?\d+(?:\.\d+)?)\s+LU\b`)
	reEBURTHR = regexp.MustCompile(`Threshold:\s+(-?\d+(?:\.\d+)?)\s+LUFS`)
)

// parseEBUR128 从 Summary 段里掰出指标，键名沿用 loudnorm 那套（input_i / input_tp），
// 这样 meta sidecar 里的老结果和上层 loudGainDb/buildLoudFilter 全都不用动。
func parseEBUR128(s string) (map[string]float64, error) {
	i := strings.LastIndex(s, "Summary:")
	if i < 0 {
		return nil, fmt.Errorf("ebur128 输出里没有 Summary")
	}
	tail := s[i:] // 必须在 Summary 之后找，周期日志里也有 I:/LRA: 字样
	ii, tp := reEBURI.FindStringSubmatch(tail), reEBURTP.FindStringSubmatch(tail)
	if ii == nil || tp == nil {
		return nil, fmt.Errorf("ebur128 Summary 缺 I 或 Peak")
	}
	li, _ := strconv.ParseFloat(ii[1], 64)
	tpv, _ := strconv.ParseFloat(tp[1], 64)
	if li <= -69 { // ebur128 的下限 -70：静音文件，线性增益没意义
		return nil, fmt.Errorf("响度量出 -70dB（静音？）")
	}
	m := map[string]float64{"input_i": li, "input_tp": tpv}
	if x := reEBURLRA.FindStringSubmatch(tail); x != nil {
		m["input_lra"], _ = strconv.ParseFloat(x[1], 64)
	}
	if x := reEBURTHR.FindStringSubmatch(tail); x != nil {
		m["input_thresh"], _ = strconv.ParseFloat(x[1], 64)
	}
	return m, nil
}

// tailBuffer 只留最后 limit 字节：用量测的 stderr 动辄几千行，全文留着没意义。
type tailBuffer struct {
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.limit:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// loudMeasureLoudnorm 老的量测方式：只作兜底（ebur128 不可用时）。
func loudMeasureLoudnorm(ff, full, pre string) (map[string]float64, error) {
	af := fmt.Sprintf("loudnorm=I=%g:TP=%g:LRA=%g:print_format=json", loudTargetI, loudTargetTP, loudTargetLRA)
	if pre != "" {
		af = pre + "," + af
	}
	cmd := exec.Command(ff,
		"-i", full, "-vn", "-sn", "-dn",
		"-af", af,
		"-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return parseLoudJSON(stderr.String())
}

func parseLoudJSON(s string) (map[string]float64, error) {
	i := strings.LastIndex(s, "{")
	j := strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return nil, fmt.Errorf("loudnorm 输出里没有 JSON")
	}
	re := regexp.MustCompile(`"([a-z_]+)"\s*:\s*"?(-?[\d.]+|-inf)"?`)
	m := map[string]float64{}
	for _, x := range re.FindAllStringSubmatch(s[i:j+1], -1) {
		if x[2] == "-inf" { // 静音文件：线性归一没意义，让上层退化为动态模式
			return nil, fmt.Errorf("响度量出 -inf（静音？）")
		}
		v, err := strconv.ParseFloat(x[2], 64)
		if err == nil {
			m[x[1]] = v
		}
	}
	for _, k := range []string{"input_i", "input_tp", "input_lra", "input_thresh", "target_offset"} {
		if _, ok := m[k]; !ok {
			return nil, fmt.Errorf("loudnorm JSON 缺 %s", k)
		}
	}
	return m, nil
}

// loudMeasureCached：响度分析结果按（变体key|路径|大小|mtime）落到 cache/eq/meta/<sha1>.json。
// 分析遍要整首解码（约 1~2s），落了 sidecar 之后，同一首歌在响度开关/各预设之间
// 来回切都直接读结果，不再重复分析。key 里带预设 —— 不同 EQ 的响度不一样。
func loudMeasureCached(ff, full string, st os.FileInfo, pre, key string) (map[string]float64, error) {
	sum := sha1.Sum([]byte(key + "|" + full + "|" + strconv.FormatInt(st.Size(), 10) + "|" + strconv.FormatInt(st.ModTime().UnixNano(), 10)))
	dir := filepath.Join(eqCacheRoot(), "meta")
	meta := filepath.Join(dir, fmt.Sprintf("%x.json", sum))
	if b, err := os.ReadFile(meta); err == nil {
		var m map[string]float64
		if json.Unmarshal(b, &m) == nil {
			if _, ok := m["input_i"]; ok {
				return m, nil
			}
		}
	}
	m, err := loudMeasure(ff, full, pre)
	if err != nil {
		return nil, err
	}
	if b, jerr := json.Marshal(m); jerr == nil {
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(meta, b, 0o644)
	}
	return m, nil
}

func f2s(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// buildEQFilter 把预设转成 ffmpeg audio filter 串。bass/treble 是架式滤波，
// equalizer 是峰值滤波（Q 宽度），volume 做整体增益当 headroom。
func buildEQFilter(p EQPreset) string {
	var f strings.Builder
	add := func(s string) {
		if f.Len() > 0 {
			f.WriteByte(',')
		}
		f.WriteString(s)
	}
	if p.Gains[0] != 0 {
		add("bass=g=" + f2s(p.Gains[0]) + ":f=80")
	}
	if p.Gains[1] != 0 {
		add("equalizer=f=300:width_type=q:width=0.7:g=" + f2s(p.Gains[1]))
	}
	if p.Gains[2] != 0 {
		add("equalizer=f=2500:width_type=q:width=0.8:g=" + f2s(p.Gains[2]))
	}
	if p.Gains[3] != 0 {
		add("treble=g=" + f2s(p.Gains[3]) + ":f=8000")
	}
	if p.Pre != 1 {
		add("volume=volume=" + f2s(p.Pre))
	}
	return f.String()
}

func eqCacheRoot() string { return filepath.Join(cacheDir, "eq") }

// eqPresetSig 把一个预设「到底长什么样」压成指纹：
// EQ 四段增益 + 链尾整体增益 +（开了响度均衡时）目标响度与算法版本。
// 这就是「同名预设、不同参数」的唯一区分依据 —— 缓存 key 里只有预设名，
// 光靠名字分不出你后来把低音从 +7dB 调成了 +9dB。
func eqPresetSig(p EQPreset, loud bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "g=%.2f/%.2f/%.2f/%.2f;pre=%s", p.Gains[0], p.Gains[1], p.Gains[2], p.Gains[3], f2s(p.Pre))
	if loud {
		fmt.Fprintf(&b, ";loud=%s/%.1f/%.1f/%.1f", loudAlgoVer, loudTargetI, loudTargetTP, loudTargetLRA)
	}
	h := sha1.Sum([]byte(b.String()))
	return fmt.Sprintf("%x", h[:8])
}

// eqPresetDir 返回某预设的缓存目录，并保证目录里的成品确实是**当前参数**转出来的。
// 目录里放一个 .sig 指纹；指纹对不上就把整个目录清掉重来。
// 老目录没有 .sig 时**不删**——那批就是同一套参数转的，白扔可惜，只补写指纹。
var eqSigMu sync.Mutex

func eqPresetDir(p EQPreset, loud bool) string {
	key := keyOf(p, loud)
	dir := filepath.Join(eqCacheRoot(), key)
	sig := eqPresetSig(p, loud)
	marker := filepath.Join(dir, ".sig")

	eqSigMu.Lock()
	defer eqSigMu.Unlock()
	old, err := os.ReadFile(marker)
	switch {
	case err == nil && string(old) == sig:
		return dir // 参数没变，直接用
	case err == nil && string(old) != sig:
		// 参数改过了：整个预设目录的老成品全部作废（它们是用旧曲线转的）
		if rmErr := os.RemoveAll(dir); rmErr == nil {
			log.Printf("[EQ] 预设参数已变（%s），清掉旧缓存目录重新转", key)
		}
	}
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(marker, []byte(sig), 0o644)
	return dir
}

func eqCachePath(p EQPreset, loud bool, full string, st os.FileInfo) string {
	// 文件名哈希保持与老版本完全一致（key + [loud 算法版本] + 路径 + 大小 + mtime），
	// 这样加指纹机制不会把现存缓存全部作废。
	key := keyOf(p, loud)
	ver := ""
	if strings.HasPrefix(key, "loud_") {
		ver = "|loudalg=" + loudAlgoVer
	}
	h := sha1.Sum([]byte(key + ver + "|" + full + "|" + strconv.FormatInt(st.Size(), 10) + "|" + strconv.FormatInt(st.ModTime().UnixNano(), 10)))
	return filepath.Join(eqPresetDir(p, loud), fmt.Sprintf("%x.mp3", h))
}

var eqLocks sync.Map // cachePath -> *sync.Mutex

func eqLockFor(path string) *sync.Mutex {
	v, _ := eqLocks.LoadOrStore(path, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// eqCacheEvict 把 EQ 缓存目录总大小压到 8GB 以内，删最旧的先。
type eqCacheFile struct {
	p  string
	fi os.FileInfo
}

func eqCacheEvict() {
	const capBytes int64 = 8 << 30
	root := eqCacheRoot()
	var files []eqCacheFile
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.HasSuffix(p, ".mp3") {
			files = append(files, eqCacheFile{p, fi})
		}
		return nil
	})
	if len(files) == 0 {
		return
	}
	total := int64(0)
	for _, f := range files {
		total += f.fi.Size()
	}
	if total <= capBytes {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].fi.ModTime().Before(files[j].fi.ModTime()) })
	for _, f := range files {
		if total <= capBytes {
			break
		}
		// 缓存真实路径是 root/<预设key>/<sha1>.mp3，必须按 Walk 给的完整路径删
		if err := os.Remove(f.p); err == nil {
			total -= f.fi.Size()
		}
	}
}

// handleFSEQ 在已鉴权、路径已解析后调用。loud=响度均衡开关（独立于 EQ 预设）。
// 非音频、ffmpeg 缺失都优雅降级为原文件。
//
// 2026-11-06 四改（用户：「你这逻辑肯定是整首处理完再接回进度，中途肯定要停一下」）：
//   - 成品已在磁盘 → ServeFile（带 Xing 头，最稳）。
//   - 没有成品 → **边转边发**：直接对着 ffmpeg 的输出流写响应，首包 ~150ms，
//     听到的第一个音符就是处理过的，绝不会「先放原声、两秒后突变」。
//   - 同时后台补一份完整成品落盘，下次（任何设备）直接命中、秒开。
//
// 流式还能拖进度，靠的是 MP3 CBR 的字节率是死的（见 eqCBRBytesPerSec）：
// 响应里报 Content-Length + Accept-Ranges，seek 时浏览器发 Range，
// 我们把字节偏移线性换回时间点，用 -ss 从那儿重转一小段。误差上限一个帧（26ms）。
func handleFSEQ(w http.ResponseWriter, r *http.Request, full string, p EQPreset, loud bool) {
	if fileKind(filepath.Base(full), false) != "audio" {
		serveFileHonest(w, r, full)
		return
	}
	ff, _ := findFF()
	if ff == "" {
		log.Printf("[EQ] 没找到 ffmpeg，跳过处理直接原样返回（播放不中断）: %s", full)
		serveFileHonest(w, r, full)
		return
	}
	st, err := os.Stat(full)
	if err != nil {
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}
	cachePath := eqCachePath(p, loud, full, st)
	if fi, err := os.Stat(cachePath); err == nil && fi.Size() > 0 {
		http.ServeFile(w, r, cachePath) // 缓存命中：Range/续播/拖进度全支持
		return
	}
	chain, cerr := eqFilterChain(ff, full, st, p, loud)
	if cerr != nil {
		log.Printf("[EQ] 组装滤波链失败，原样返回: %v", cerr)
		serveFileHonest(w, r, full)
		return
	}
	if chain == "" { // 原声：压根不用转
		serveFileHonest(w, r, full)
		return
	}
	dur := eqDuration(ff, full, st)
	if dur <= 0 {
		log.Printf("[EQ] 读不到时长，退化成原样返回: %s", full)
		serveFileHonest(w, r, full)
		return
	}
	// 后台补成品：这次听完，下次（哪怕换设备）直接 ServeFile
	eqTranscodeAsync(ff, full, st, cachePath, p, loud)
	eqServeStream(w, r, ff, full, chain, dur)
}

// eqFilterChain 组装滤波串：EQ → loudnorm（响度必须放最后，否则又被 EQ 增益改掉）。
// loud 的分析遍要读整首（1~2s），结果按「预设 + 文件」落 sidecar，之后是零成本。
func eqFilterChain(ff, full string, st os.FileInfo, p EQPreset, loud bool) (string, error) {
	var parts []string
	eqPart := buildEQFilter(p)
	if eqPart != "" {
		parts = append(parts, eqPart)
	}
	if loud {
		// 分析遍必须带上同一条 EQ 链，量到的才是「处理之后」的响度
		m, merr := loudMeasureCached(ff, full, st, eqPart, keyOf(p, loud))
		if merr != nil {
			log.Printf("[EQ] 响度分析失败，退化为单遍动态 loudnorm: %v", merr)
			parts = append(parts, buildDynamicLoudFilter())
		} else if lf := buildLoudFilter(m); lf != "" {
			log.Printf("[EQ] 响度均衡 %s：I=%.1fLUFS TP=%.1f → 增益 %+.1fdB",
				filepath.Base(full), m["input_i"], m["input_tp"], loudGainDb(m))
			parts = append(parts, lf)
		} else {
			// 不动是**正确**结果：要么本来就够响，要么峰值已顶到 0 dBFS 没余量可提。
			log.Printf("[EQ] 响度均衡 %s：I=%.1fLUFS TP=%+.1f 不处理（已够响或没峰值余量）",
				filepath.Base(full), m["input_i"], m["input_tp"])
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, ","), nil
}

// ---- 流式转码 ----
// 320k CBR 的 MP3 平均字节率是固定的：LAME 用 padding 帧把码率钉死在 320kbit/s，
// 实测 185.52s → 7,420,908B，正好 40000.2 B/s。所以字节 ↔ 时间可以线性换算，
// 「响应报总长 + Range 换算回时间重转」才成立。换别的码率必须同步改这个数。
const eqCBRBytesPerSec = 40000.0

func (v durEntry) valid(st os.FileInfo) bool {
	return v.size == st.Size() && v.mod == st.ModTime().UnixNano()
}

type durEntry struct {
	size int64
	mod  int64
	d    float64
}

var eqDurCache sync.Map // full -> durEntry

var durRe = regexp.MustCompile(`Duration:\s*(\d+):(\d+):(\d+)\.(\d+)`)

// eqDuration 读源文件时长（秒）。ffmpeg 不给 -o 时会以 1 退出，但 stderr 里有
// 「Duration: 00:03:05.45」，比 ffprobe 少依赖一个二进制。结果按 size+mtime 缓存。
func eqDuration(ff, full string, st os.FileInfo) float64 {
	if v, ok := eqDurCache.Load(full); ok {
		if e := v.(durEntry); e.valid(st) {
			return e.d
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, ff, "-hide_banner", "-i", full)
	cmd.Stderr = &buf
	_ = cmd.Run()
	m := durRe.FindStringSubmatch(buf.String())
	if m == nil {
		return 0
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	s, _ := strconv.Atoi(m[3])
	var frac float64
	fmt.Sscanf("0."+m[4], "%f", &frac)
	d := float64(h*3600+mi*60+s) + frac
	eqDurCache.Store(full, durEntry{st.Size(), st.ModTime().UnixNano(), d})
	return d
}

// parseByteRange 只处理单区间：bytes=a-b / bytes=a- / bytes=-n
func parseByteRange(h string, total int64) (start, end int64, ok bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	s := strings.TrimPrefix(h, "bytes=")
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i] // 多区间不陪玩：浏览器播媒体只用单区间
	}
	p := strings.SplitN(s, "-", 2)
	if len(p) != 2 {
		return 0, 0, false
	}
	if p[0] == "" {
		n, err := strconv.ParseInt(p[1], 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		start, end = total-n, total-1
		if start < 0 {
			start = 0
		}
		return start, end, true
	}
	start, err := strconv.ParseInt(p[0], 10, 64)
	if err != nil || start < 0 || start >= total {
		return 0, 0, false
	}
	end = total - 1
	if p[1] != "" {
		end, err = strconv.ParseInt(p[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false
		}
		if end >= total {
			end = total - 1
		}
	}
	return start, end, true
}

// eqServeStream 对着 ffmpeg 的输出流边转边写。写满 Content-Length 就收工，
// 客户端断开时 r.Context() 取消 → ffmpeg 立刻被杀，不会留着空转。
func eqServeStream(w http.ResponseWriter, r *http.Request, ff, full, chain string, dur float64) {
	total := int64(dur*eqCBRBytesPerSec + 0.5)
	if total <= 0 {
		http.Error(w, "时长无效", http.StatusInternalServerError)
		return
	}
	start, end, ranged := int64(0), total-1, false
	if rh := r.Header.Get("Range"); rh != "" {
		s, e, ok := parseByteRange(rh, total)
		if !ok {
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(total, 10))
			http.Error(w, "Range 不合法", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, end, ranged = s, e, true
	}
	n := end - start + 1
	h := w.Header()
	h.Set("Content-Type", "audio/mpeg")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatInt(n, 10))
	h.Set("Cache-Control", "no-store")
	if ranged {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	}
	if ranged {
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	// HEAD 只想知道长度/可否 seek，别真去转一首（Go 会丢弃 body，但 ffmpeg 照样空转）
	if r.Method == http.MethodHead {
		return
	}
	t0 := float64(start) / eqCBRBytesPerSec
	t1 := float64(end+1) / eqCBRBytesPerSec
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// -write_xing 0 -id3v2_version 0：输出必须是纯帧，多一个字节头部都会让
	// 字节↔时间整体偏移；-ss 放在 -i 前 = 快速 seek（音频每帧都是关键帧，够准）。
	cmd := exec.CommandContext(ctx, ff, "-v", "error", "-nostdin",
		"-ss", fmt.Sprintf("%.3f", t0),
		"-t", fmt.Sprintf("%.3f", t1-t0+0.05), // 多给 50ms，末尾自己截
		"-i", full,
		"-vn", "-map_metadata", "-1",
		"-write_xing", "0", "-id3v2_version", "0",
		"-af", chain,
		"-c:a", "libmp3lame", "-b:a", "320k", "-ar", "44100", "-ac", "2",
		"-f", "mp3", "pipe:1")
	cmd.Stderr = io.Discard
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var sent int64
	for sent < n {
		m, rerr := out.Read(buf)
		if m > 0 {
			if int64(m) > n-sent {
				m = int(n - sent) // 末尾那一帧切一半没关系，下一个 Range 会自己重转
			}
			if _, werr := w.Write(buf[:m]); werr != nil {
				break
			}
			sent += int64(m)
			if flusher != nil {
				flusher.Flush() // 不刷的话 NetHTTP 会攒够缓冲才发，首包就慢了
			}
		}
		if rerr != nil {
			break
		}
	}
	// 源文件比 ffmpeg 报的时长短 / -t 边界差几帧时，这里会差几十字节。
	// Content-Length 少一个字节 Chrome 就整条报 CONTENT_LENGTH_MISMATCH，
	// 所以补 0 填满：解码器会把尾部无效数据丢掉，听感上是几十毫秒静音。
	if sent < n {
		zero := make([]byte, 8192)
		for sent < n {
			k := int64(len(zero))
			if n-sent < k {
				k = n - sent
			}
			if _, werr := w.Write(zero[:k]); werr != nil {
				break
			}
			sent += k
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	cancel()   // 写够了就让 ffmpeg 停，别继续空转
	_ = cmd.Wait()
}

// eqTranscodeAsync 起一个后台转码（同一路径并发只转一次）。
func eqTranscodeAsync(ff, full string, st os.FileInfo, cachePath string, p EQPreset, loud bool) {
	if _, pending := eqPending.Load(cachePath); pending {
		return
	}
	eqPending.Store(cachePath, true)
	go func() {
		defer eqPending.Delete(cachePath)
		eqTranscode(ff, full, st, cachePath, p, loud)
	}()
}

// eqPending：正在后台转码的缓存路径集合。
var eqPending sync.Map

// eqTranscode 把成品转出来落盘。走独立 goroutine，不阻塞任何请求；
// 客户端等不及切走了也照样把缓存写完，下次（哪怕别的设备）直接命中。
func eqTranscode(ff, full string, st os.FileInfo, cachePath string, p EQPreset, loud bool) {
	// 同一首并发只转一次；真撞上了后来者发现缓存已就绪就直接返回。
	lk := eqLockFor(cachePath)
	lk.Lock()
	defer lk.Unlock()
	if fi, err := os.Stat(cachePath); err == nil && fi.Size() > 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return
	}
	// 滤波顺序按母带习惯：先 EQ 调音色，再 loudnorm 定响度（响度均衡放最后，
	// 否则 EQ 增益会把归一好的响度又改掉）。与流式路径共用同一个组装函数，
	// 两条路径的输出必须一致，否则「边听边转」和「缓存命成」听起来会不一样。
	chain, cerr := eqFilterChain(ff, full, st, p, loud)
	if cerr != nil || chain == "" {
		return
	}
	// 用 Background 而不是请求 ctx：客户端等不及切走 / 刷新了，也让这次转码
	// 把缓存写完。15 分钟兜底防 ffmpeg 挂死。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	tmp := cachePath + ".part"
	// -vn + -map_metadata -1：丢弃封面与全部标签。旧管道版输出曾把 MB 级封面塞进
	// 「声明 size=0」的坏 ID3 且无 Xing 头，浏览器对这种文件的时间→字节映射全靠猜，
	// 切音效 seek 会偏好几秒（2026-11-06 用户实测 1:50 切、1:47 播）。
	// 纯音频帧 + 文件输出（非管道）让 ffmpeg 写出正确 Xing，seek 恢复帧级精度。
	cmd := exec.CommandContext(ctx, ff,
		"-y", "-i", full,
		"-vn", "-map_metadata", "-1",
		"-af", chain,
		"-c:a", "libmp3lame", "-b:a", "320k", "-ar", "44100", "-ac", "2",
		"-f", "mp3", tmp)
	cmd.Stderr = io.Discard
	log.Printf("[EQ] 后台转码: %s → %s", filepath.Base(full), keyOf(p, loud))
	if err := cmd.Run(); err != nil {
		os.Remove(tmp)
		log.Printf("[EQ] ffmpeg 转码失败（本次保持原声，下次重试）: %v", err)
		return
	}
	if err := os.Rename(tmp, cachePath); err != nil {
		os.Remove(tmp)
		return
	}
	eqCacheEvict()
	log.Printf("[EQ] 缓存就绪: %s / %s", keyOf(p, loud), filepath.Base(full))
}

func keyOf(p EQPreset, loud bool) string {
	if loud {
		return "loud_" + p.Key
	}
	return p.Key
}

// startEQWarmup 预热：后台把某首歌的某个预设转好缓存，下次播放免等。
// 当前未强制使用；保留接口以备将来「常听歌单预转」。
func startEQWarmup(full string, p EQPreset) {
	_ = full
	_ = p
}
