"""准备内置 cloudflared 资产：下载官方二进制 →（darwin 解 tgz）→ zstd → assets/cloudflared/<平台>/cloudflared.zst。

assets/cloudflared/ 不进版本库（五平台压缩后约 60MB），clone 之后先跑这个脚本再 go build。
已经准备好的平台会跳过，重复跑是安全的。

为什么用官方 release 而不是 import 它的 Go 包：
  编译 cloudflared 的库会撞依赖版本（`undefined: altsrc.ApplyInputSource`），
  官方从没打算让别人 import 它。而它的 CLI 反倒极稳 —— quick tunnel 的地址
  就打在 stderr 里，抓一行正则即可。于是走和 ffmpeg 完全一样的路子：
  官方二进制 → zstd → go:embed → 首次使用释放到 cache/。

版本固定 TAG，不用 latest：上游换版本不能悄悄改变我们已经验过的行为。
升级时改这一个常量即可；包长度变化会让 .ok 标记失效，自动重新释放。

用法：
    python scripts/make_cfbundle.py            # 缺什么补什么
    python scripts/make_cfbundle.py --force    # 全部重下重压
"""
import os
import subprocess
import sys
import tarfile
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DL_DIR = os.path.join(ROOT, '.build', 'static')
DST = os.path.join(ROOT, 'assets', 'cloudflared')

FORCE = '--force' in sys.argv

TAG = '2026.11.0'
BASE = 'https://github.com/cloudflare/cloudflared/releases/download/%s/' % TAG

# 平台 → (上游资产名, tgz 里要取出的成员名；None 表示资产本身就是可执行文件)
# darwin 只发 .tgz，linux 是**无后缀**文件名，都不能靠扩展名猜。
ASSETS = {
    'windows-amd64': ('cloudflared-windows-amd64.exe', None),
    'linux-amd64': ('cloudflared-linux-amd64', None),
    'linux-arm64': ('cloudflared-linux-arm64', None),
    'darwin-amd64': ('cloudflared-darwin-amd64.tgz', 'cloudflared'),
    'darwin-arm64': ('cloudflared-darwin-arm64.tgz', 'cloudflared'),
}


def download(asset):
    os.makedirs(DL_DIR, exist_ok=True)
    dst = os.path.join(DL_DIR, asset)
    if os.path.exists(dst) and os.path.getsize(dst) > 1000 and not FORCE:
        print('  已下载，跳过: %s (%.1f MB)' % (asset, os.path.getsize(dst) / 1048576), flush=True)
        return dst
    print('  下载 %s ...' % asset, flush=True)
    req = urllib.request.Request(BASE + asset, headers={'User-Agent': 'selfnas-build'})
    with urllib.request.urlopen(req, timeout=180) as r, open(dst, 'wb') as f:
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


def extract(src, member, plat):
    """tgz 里取出 cloudflared，落到 .build/static/cloudflared-<平台>。"""
    out = os.path.join(DL_DIR, 'cloudflared-' + plat)
    with tarfile.open(src, 'r:gz') as t:
        f = t.extractfile(member)
        if f is None:
            raise RuntimeError('%s 里没有 %s' % (src, member))
        with open(out, 'wb') as w:
            w.write(f.read())
    return out


def main():
    total = 0
    done = 0
    for plat, (asset, member) in ASSETS.items():
        out = os.path.join(DST, plat, 'cloudflared.zst')
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
        if member:
            src = extract(src, member, plat)
        os.makedirs(os.path.dirname(out), exist_ok=True)
        raw = os.path.getsize(src)
        r = subprocess.run(['go', 'run', './scripts/mkzstd', src, out], cwd=ROOT)
        if r.returncode != 0:
            print('  [失败] 压缩 %s' % plat, flush=True)
            continue
        sz = os.path.getsize(out)
        total += sz
        done += 1
        print('  → assets/cloudflared/%s/cloudflared.zst  %.1f MB（原始 %.1f MB，压缩率 %.0f%%）'
              % (plat, sz / 1048576, raw / 1048576, sz * 100 / raw), flush=True)

    print('\n完成 %d/%d 个平台，单平台安装包增量约 %.1f MB'
          % (done, len(ASSETS), total / 1048576 / max(1, done)), flush=True)
    if done < len(ASSETS):
        print('⚠ 有平台没准备好，编译对应平台会因为没有 embed 文件而失败', flush=True)


if __name__ == '__main__':
    main()
