"""准备内置 ffmpeg 资产：下载静态二进制 → zstd 压缩 → assets/ffmpeg/<平台>/ffmpeg.zst。

assets/ffmpeg/ 不进版本库（五平台加起来约 260MB），所以 clone 之后要先跑一遍这个脚本，
再 `go build`。已经准备好的平台会跳过，重复跑是安全的。

为什么用 eugeneware/ffmpeg-static 这个源：
  1. 五个平台齐全 —— Windows / Linux amd64 / Linux arm64 / macOS Intel / macOS Apple Silicon。
     BtbN 只做 Windows + Linux，gyan.dev 只做 Windows；macOS 没有它就只能让用户自己装，
     而 Mac 上很可能连 Homebrew 都没有，`brew install ffmpeg` 对他们等于无解。
  2. 体积小得多：Windows 82MB vs BtbN 125MB，linux-arm64 48MB vs 111MB。
  3. 是 npm 上 ffmpeg-static 包的官方产物，几十万个项目在用，可复现。

用的是 6.1.1 essentials 构建，含 loudnorm / equalizer / volume / aresample 和 libmp3lame
—— 音效、响度、320k 输出全都够（已实测）。它是 GPL 构建（含 x264/x265），
作为独立可执行文件随包分发，README 的 License 章节已注明来源与许可。

用法：
    python scripts/make_ffbundle.py            # 缺什么补什么
    python scripts/make_ffbundle.py --force    # 全部重下重压
"""
import os
import subprocess
import sys
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DL_DIR = os.path.join(ROOT, '.build', 'static')
DST = os.path.join(ROOT, 'assets', 'ffmpeg')

FORCE = '--force' in sys.argv

TAG = 'b6.1.1'  # ffmpeg-static 的版本标签，对应 ffmpeg 6.1.1
BASE = 'https://github.com/eugeneware/ffmpeg-static/releases/download/%s/' % TAG

# 目标平台 → 上游资产名
ASSETS = {
    'windows-amd64': 'ffmpeg-win32-x64',
    'linux-amd64': 'ffmpeg-linux-x64',
    'linux-arm64': 'ffmpeg-linux-arm64',
    'darwin-amd64': 'ffmpeg-darwin-x64',
    'darwin-arm64': 'ffmpeg-darwin-arm64',
}


def download(asset):
    os.makedirs(DL_DIR, exist_ok=True)
    dst = os.path.join(DL_DIR, asset)
    if os.path.exists(dst) and os.path.getsize(dst) > 1000 and not FORCE:
        print('  已下载，跳过: %s (%.1f MB)' % (asset, os.path.getsize(dst) / 1048576), flush=True)
        return dst
    print('  下载 %s ...' % asset, flush=True)
    req = urllib.request.Request(BASE + asset, headers={'User-Agent': 'selfnas-build'})
    with urllib.request.urlopen(req, timeout=120) as r, open(dst, 'wb') as f:
        total = int(r.headers.get('Content-Length') or 0)
        got = last = 0
        while True:
            chunk = r.read(1 << 20)
            if not chunk:
                break
            f.write(chunk)
            got += len(chunk)
            pct = int(got * 100 / total) if total else 0
            if pct // 20 != last // 20:
                last = pct
                print('    %3d%%  %.0f MB' % (pct, got / 1048576), flush=True)
    return dst


def main():
    total = 0
    done = 0
    for plat, asset in ASSETS.items():
        out = os.path.join(DST, plat, 'ffmpeg.zst')
        if os.path.exists(out) and not FORCE:
            sz = os.path.getsize(out)
            print('%s: 已就绪 %.1f MB（--force 可重做）' % (plat, sz / 1048576), flush=True)
            total += sz
            done += 1
            continue
        print('%s' % plat, flush=True)
        src = download(asset)
        if not src:
            continue
        os.makedirs(os.path.dirname(out), exist_ok=True)
        raw = os.path.getsize(src)
        r = subprocess.run(['go', 'run', './scripts/mkzstd', src, out], cwd=ROOT)
        if r.returncode != 0:
            print('  [失败] 压缩 %s' % plat, flush=True)
            continue
        sz = os.path.getsize(out)
        total += sz
        done += 1
        print('  → assets/ffmpeg/%s/ffmpeg.zst  %.1f MB（原始 %.1f MB，压缩率 %.0f%%）'
              % (plat, sz / 1048576, raw / 1048576, sz * 100 / raw), flush=True)

    print('\n完成 %d/%d 个平台，单平台安装包增量约 %.1f MB'
          % (done, len(ASSETS), total / 1048576 / max(1, done)), flush=True)
    if done < len(ASSETS):
        print('⚠ 有平台没准备好，编译对应平台会因为没有 embed 文件而失败', flush=True)


if __name__ == '__main__':
    main()
