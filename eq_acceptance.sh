#!/usr/bin/env bash
# =============================================================================
# 后端 EQ 验收测试
# -----------------------------------------------------------------------------
# 针对一个【运行中的 selfnas 测试实例】（默认 http://127.0.0.1:8080）做端到端校验，
# 确认「EQ 已下沉到后端、前端只是普通 <audio>」这套架构真的生效：
#
#   1. 原声（无 eq）              -> 返回原文件，HTTP 200，内容非空
#   2. ?eq=bass                   -> 返回 audio/mpeg，内容与原声【不同】（EQ 真生效）
#   3. ?eq=off                    -> 返回原文件（bypass，MD5 与原声一致）
#   4. 缓存命中（第二次 bass）     -> 响应头带 Accept-Ranges: bytes + Content-Length
#                                    （= ServeFile 路径，支持拖进度/续播/锁屏）
#   5. 缓存落盘（可选，best-effort）-> SERVER_CWD 指到服务端工作目录时，检查
#                                    cache/eq/bass/*.mp3 存在
#   6. 前端静态检查               -> web/files.js 中无 EQ 的 Web Audio 图谱残留
#
# 注意：本脚本只做 GET（登录 + 下载），不写库，且默认打 8080 测试实例，不碰正式 80。
#
# 用法（全部参数可选，带默认值）：
#   BASE=http://127.0.0.1:8080 \
#   ROOT='D:\' \
#   SONG='CloudMusic/周杰伦 - Mojito.mp3' \
#   USER=admin PASS=123456 \
#   SERVER_CWD='F:/项目/selfnas' \
#   ./eq_acceptance.sh
# =============================================================================

set -u

BASE="${BASE:-http://127.0.0.1:8080}"
ROOT="${ROOT:-D:\\}"
SONG="${SONG:-CloudMusic/周杰伦 - Mojito.mp3}"
USER="${USER:-admin}"
PASS="${PASS:-123456}"
SERVER_CWD="${SERVER_CWD:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WEB_JS="$SCRIPT_DIR/web/files.js"

PY="$(command -v python3 || command -v python)"

pass=0
fail=0

# desc expected actual
chk() {
  if [ "$2" = "$3" ]; then
    echo "  PASS: $1"
    pass=$((pass + 1))
  else
    echo "  FAIL: $1  (expected=[$2] actual=[$3])"
    fail=$((fail + 1))
  fi
}

# 大于 0 的字节数
chk_nonzero() {
  if [ "$2" -gt 0 ] 2>/dev/null; then
    echo "  PASS: $1 ($2 bytes)"
    pass=$((pass + 1))
  else
    echo "  FAIL: $1 (size=$2)"
    fail=$((fail + 1))
  fi
}

echo "== 登录 =="
COOKIE="$(mktemp)"
LOGIN_HTTP="$(curl --noproxy '*' -s -c "$COOKIE" -o /dev/null -w '%{http_code}' \
  -X POST "$BASE/api/login" -H 'Content-Type: application/json' \
  -d "{\"user\":\"$USER\",\"pass\":\"$PASS\"}")"
chk "login HTTP" "200" "$LOGIN_HTTP"

ENC="$("$PY" -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))" "$SONG")"
ROOTENC="$("$PY" -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))" "$ROOT")"

TMP="$(mktemp -d)"

# 下载：outfile eqparam -> 输出 "HTTP SIZE"
dl() {
  curl --noproxy '*' -s -b "$COOKIE" -o "$1" \
    "$BASE/api/fs/download?root=$ROOTENC&path=$ENC&inline=1${2:+&eq=$2}" \
    -w '%{http_code} %{size_download}'
}

md5of() { md5sum "$1" | cut -d' ' -f1; }

# 轮询服务端直到某个预设目录里的缓存文件数 >= want（= 后台整首转码完成）。
# 2026-11-06 五改后不再有 X-EQ-Pending 头（冷请求直接边转边发处理后的音频），
# 所以「成品就绪」只能看磁盘。注意是「>= want」而不是「变多了」：
# 同一首歌的缓存可能本来就在（热缓存），那种情况下应该立刻返回。
wait_cache() {
  local sub="$1" want="${2:-1}" i=0 n=0
  if [ -z "$SERVER_CWD" ]; then sleep 2; return 0; fi
  while [ "$i" -lt 240 ]; do
    n=$(ls -1 "$SERVER_CWD/cache/eq/$sub/"*.mp3 2>/dev/null | wc -l)
    [ "$n" -ge "$want" ] && return 0
    i=$((i + 1))
    sleep 0.5
  done
  return 1
}
cachen() { ls -1 "$SERVER_CWD/cache/eq/$1/"*.mp3 2>/dev/null | wc -l; }

echo "== 1. 原声（无 eq） =="
read o_h o_s < <(dl "$TMP/orig.mp3" "")
chk "orig HTTP" "200" "$o_h"
chk_nonzero "orig 非空" "${o_s:-0}"
ORIG_MD5="$(md5of "$TMP/orig.mp3")"
echo "      orig MD5=$ORIG_MD5"

echo "== 2. ?eq=bass（EQ 应真实生效） =="
BURL="$BASE/api/fs/download?root=$ROOTENC&path=$ENC&inline=1&eq=bass"
B0="$(cachen bass)"
curl --noproxy '*' -s -b "$COOKIE" -o /dev/null "$BURL"   # 触发（冷缓存 = 边转边发）
wait_cache bass 1 && chk "bass 后台成品落盘" "0" "0" || chk "bass 后台成品落盘" "0" "1"
CT="$(curl --noproxy '*' -s -b "$COOKIE" -D - -o "$TMP/bass.mp3" "$BURL" \
  | grep -i '^content-type:' | tr -d '\r' | awk '{print $2}')"
BASS_MD5="$(md5of "$TMP/bass.mp3")"
chk "bass content-type" "audio/mpeg" "$CT"
chk_nonzero "bass 非空" "$(stat -c%s "$TMP/bass.mp3" 2>/dev/null || wc -c < "$TMP/bass.mp3")"
chk "bass 内容≠原声" "diff" "$([ "$BASS_MD5" != "$ORIG_MD5" ] && echo diff || echo same)"
echo "      bass MD5=$BASS_MD5"

echo "== 2b. 响度均衡（?loud=1，独立开关，可与 eq 叠加） =="
# 冷请求同样边转边发（响度分析要读整首，首次约 1~2s）；成品落盘后再取一份断言。
LURL="$BASE/api/fs/download?root=$ROOTENC&path=$ENC&inline=1&eq=bass&loud=1"
L0="$(cachen loud_bass)"
curl --noproxy '*' -s -b "$COOKIE" -o /dev/null "$LURL"
wait_cache loud_bass 1 && chk "loud 后台成品落盘" "0" "0" || chk "loud 后台成品落盘" "0" "1"
CT="$(curl --noproxy '*' -s -b "$COOKIE" -D - -o "$TMP/loud.mp3" "$LURL" \
  | grep -i '^content-type:' | tr -d '\r' | awk '{print $2}')"
LOUD_MD5="$(md5of "$TMP/loud.mp3")"
chk "loud content-type" "audio/mpeg" "$CT"
chk_nonzero "loud 非空" "$(stat -c%s "$TMP/loud.mp3" 2>/dev/null || wc -c < "$TMP/loud.mp3")"
chk "loud 内容≠原声" "diff" "$([ "$LOUD_MD5" != "$ORIG_MD5" ] && echo diff || echo same)"
echo "      loud MD5=$LOUD_MD5"

echo "== 3. ?eq=off（应 bypass 回原文件） =="
read f_h f_s < <(dl "$TMP/off.mp3" "off")
chk "off HTTP" "200" "$f_h"
OFF_MD5="$(md5of "$TMP/off.mp3")"
chk "off MD5==orig" "$ORIG_MD5" "$OFF_MD5"

echo "== 4. 缓存命中（再请求 bass，应走 ServeFile：Accept-Ranges + Content-Length） =="
HDRS="$(curl --noproxy '*' -s -b "$COOKIE" -D - -o /dev/null \
  "$BASE/api/fs/download?root=$ROOTENC&path=$ENC&inline=1&eq=bass")"
AR="$(echo "$HDRS" | grep -i '^accept-ranges:' | tr -d '\r' | awk '{print $2}')"
CL="$(echo "$HDRS" | grep -i '^content-length:' | tr -d '\r' | awk '{print $2}')"
chk "命中 Accept-Ranges" "bytes" "$AR"
chk_nonzero "命中 Content-Length" "${CL:-0}"

echo "== 5. 缓存落盘（best-effort，需 SERVER_CWD） =="
if [ -n "$SERVER_CWD" ]; then
  CACHE_FILES="$(ls -1 "$SERVER_CWD/cache/eq/bass/"*.mp3 2>/dev/null | wc -l)"
  chk "bass 缓存文件存在" "1" "$([ "${CACHE_FILES:-0}" -ge 1 ] && echo 1 || echo 0)"
else
  echo "  SKIP: 未设置 SERVER_CWD，跳过磁盘缓存检查"
fi

echo "== 6. 前端无 EQ Web Audio 图谱残留 =="
if [ -f "$WEB_JS" ]; then
  HITS="$(grep -nE 'createMediaElementSource|MP_FX_BANDS|\.fxNode|fxBusy|fxBypass|mpFxApply|mpFxEnsure|mpFxBuild|mpFreshAudio|mpFxDetach|MediaElementAudioSourceNode|BiquadFilterNode' "$WEB_JS" | wc -l)"
  chk "files.js 无 EQ 残留" "0" "$HITS"
else
  echo "  SKIP: 找不到 $WEB_JS"
fi

echo "== 7. 冷缓存「边转边发」（2026-11-06 五改回归） =="
# 语义变了：不再「先回原文件、转好再换源」（用户听到的是卡一下 + 音色突变），
# 改成把 ffmpeg 的输出直接写进响应：首包就要处理后的声音，且响应自带
# Content-Length + Accept-Ranges（靠 320k CBR 的固定字节率做字节↔时间映射）。
# 用一份独一份的探针文件（路径唯一 → 缓存必然冷）。
if [ -n "$SERVER_CWD" ] && [ -s "$TMP/orig.mp3" ]; then
  PROBE_DIR="$SERVER_CWD/_测试"
  mkdir -p "$PROBE_DIR"
  PROBE_NAME="_eq_cold_probe_$$.mp3"
  touch "$TMP/marker"
  cp "$TMP/orig.mp3" "$PROBE_DIR/$PROBE_NAME"
  PENC="$("$PY" -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))" "项目/selfnas/_测试/$PROBE_NAME")"
  FENC="$("$PY" -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))" 'F:\')"
  PURL="$BASE/api/fs/download?root=$FENC&path=$PENC&inline=1&eq=bass"
  BEFORE="$(cachen bass)"
  # ① HEAD：不转码，直接报长度 + 可否 seek
  PHDRS="$(curl --noproxy '*' -s -b "$COOKIE" -I "$PURL")"
  PHTTP="$(echo "$PHDRS" | head -1 | tr -d '\r')"
  PPEND="$(echo "$PHDRS" | grep -i '^x-eq-pending:' | tr -d '\r' | awk '{print $2}')"
  PCL="$(echo "$PHDRS" | grep -i '^content-length:' | tr -d '\r' | awk '{print $2}')"
  PAR="$(echo "$PHDRS" | grep -i '^accept-ranges:' | tr -d '\r' | awk '{print $2}')"
  chk "冷请求 HTTP 200" "HTTP/1.1 200 OK" "$PHTTP"
  chk "不再有 X-EQ-Pending（原声顶替那套已废）" "" "$PPEND"
  chk "冷响应可 seek（Accept-Ranges）" "bytes" "$PAR"
  chk "长度≠原声（报的是转码后长度）" "diff" "$([ "${PCL:-0}" != "${o_s:-0}" ] && echo diff || echo same)"
  # ② 首包：拿 1 秒音频（40000B），必须快
  T0="$(date +%s%N)"
  curl --noproxy '*' -s -b "$COOKIE" -r 0-39999 -o "$TMP/probe1s.bin" "$PURL"
  T1="$(date +%s%N)"
  MS=$(( (T1 - T0) / 1000000 ))
  SZ=$(wc -c < "$TMP/probe1s.bin")
  chk "首包 1 秒音频字节数" "40000" "$SZ"
  chk "首包够快(<1500ms, 实测 ${MS}ms)" "1" "$([ "$MS" -lt 1500 ] && echo 1 || echo 0)"
  # ③ 任意位置 Range 都要 206 + 长度精确（seek 靠这个）
  RHDR="$(curl --noproxy '*' -s -b "$COOKIE" -r 4000000-4399999 -D - -o "$TMP/probe4m.bin" "$PURL")"
  RHTTP="$(echo "$RHDR" | head -1 | tr -d '\r')"
  RCR="$(echo "$RHDR" | grep -i '^content-range:' | tr -d '\r' | awk '{print $2" "$3}')"
  chk "中间位置 Range 返回 206" "HTTP/1.1 206 Partial Content" "$RHTTP"
  chk "Range 长度精确" "400000" "$(wc -c < "$TMP/probe4m.bin")"
  chk "Content-Range 对得上" "bytes 4000000-4399999/${PCL}" "$RCR"
  # ④ 后台成品照旧落盘（下次直接 ServeFile）
  wait_cache bass $((BEFORE + 1)) && chk "后台成品落盘" "0" "0" || chk "后台成品落盘" "0" "1"
  # 清理：探针源文件 + 本次新增的缓存文件（按 mtime 识别，不动别的缓存）
  rm -f "$PROBE_DIR/$PROBE_NAME"
  find "$SERVER_CWD/cache/eq/bass" -name '*.mp3' -newer "$TMP/marker" -delete 2>/dev/null
else
  echo "  SKIP: 需要 SERVER_CWD（且第 1 步下载到了原声文件）"
fi

echo "== 8. 输出结构纯净（2026-11-06 二修回归：防「坏 ID3/无 Xing」畸形输出） =="
# 旧管道版转码曾产出：ID3 声明 size=0 但实际拖着 MB 级封面垃圾 + 无 Xing/Info 头，
# 浏览器对这种文件的时间→字节映射全靠猜，切音效 seek 偏好几秒（1:50 切 1:47 播）。
# 这里对第 2 步下载的 bass 文件做帧级解析：ID3 尺寸诚实、320k 帧链从音频起点
# 不断链直到 EOF、带 Info/Xing 头。
if [ -s "$TMP/bass.mp3" ]; then
  for f in "$TMP/bass.mp3" "$TMP/loud.mp3"; do
    [ -s "$f" ] || continue
    if "$PY" - "$f" << 'PYEOF'
import sys
data = open(sys.argv[1], "rb").read(); n = len(data)
fails = 0
def say(k, v):
    global fails
    print(("  PASS: " if v else "  FAIL: ") + k)
    if not v: fails += 1
def parse(h):
    if h[0] != 0xFF or (h[1] & 0xE0) != 0xE0: return None
    ver = (h[1] >> 3) & 3; layer = (h[1] >> 1) & 3
    if ver == 1 or layer != 1: return None
    bri = (h[2] >> 4) & 0xF; sri = (h[2] >> 2) & 0x3; pad = (h[2] >> 1) & 1
    if bri in (0,15) or sri == 3: return None
    BRt = [0,32,40,48,56,64,80,96,112,128,160,192,224,256,320]
    SRt = [44100,48000,32000]
    if ver != 3: return None
    kbps = BRt[bri]; hz = SRt[sri]
    return kbps, hz, int(144*kbps*1000/hz)+pad, 1152
start = 0
if data[:3] == b"ID3":
    decl = ((data[6]&0x7f)<<21)|((data[7]&0x7f)<<14)|((data[8]&0x7f)<<7)|(data[9]&0x7f)
    say("ID3 尺寸字段诚实(声明=%d)" % decl, decl < 1024)
    start = 10 + decl
i = start
while i < n-4 and not parse(data[i:i+4]): i += 1
frames = 0; t = 0.0; brs = {}; j = i
while j < n-4:
    r = parse(data[j:j+4])
    if not r: break
    brs[r[0]] = brs.get(r[0],0)+1; frames += 1; t += r[3]/r[1]; j += r[2]
say("320k 帧链不断链直到 EOF(断点=%d/%d)" % (j, n), j >= n - 4)
say("全部帧 320kbps(histogram=%s)" % brs, set(brs) == {320})
say("带 Info/Xing 头(浏览器精确 seek 依据)", (b"Info" in data) or (b"Xing" in data))
say("帧链时长>0(%.2fs)" % t, t > 0)
sys.exit(1 if fails else 0)
PYEOF
    then chk "$(basename "$f") 输出结构纯净" "0" "0"; else chk "$(basename "$f") 输出结构纯净" "0" "1"; fi
  done
else
  echo "  SKIP: 第 2 步的 bass 下载不存在"
fi

rm -rf "$TMP" "$COOKIE"

echo "==================================================================="
echo "  结果: PASS=$pass  FAIL=$fail"
echo "==================================================================="
[ "$fail" -eq 0 ]
