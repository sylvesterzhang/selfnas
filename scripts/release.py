# -*- coding: utf-8 -*-
"""把 dist/ 下的二进制打成可发布的压缩包，并生成 SHA256 校验和。

Windows 用 zip（双击解压就行），Linux/macOS 用 tar.gz（保留可执行权限位）。
"""
import os
import hashlib
import zipfile
import tarfile

V = '0.19.3'
DIST = 'dist'

README_TXT = """selfnas {v} - 自建文件服务器
================================================================

启动
----
Windows:    双击 selfnas.exe
Linux/macOS: ./selfnas

首次启动会自动生成 config.json（在程序旁边），把本机磁盘作为可访问的根目录，
并在控制台打印一个随机密码：

    [首次密码] Xk9mQ2vLp7RtZa

拿这个密码打开 http://127.0.0.1:8080/admin ，第一件事就是改掉它。

然后手机连同一个 WiFi，扫管理台里的二维码即可。

停止
----
按 Ctrl+C。后台运行时也可以在任务管理器里结束进程 ——
每次写库后都会立刻落盘，强杀不会丢「最近 / 收藏」。

注意
----
- Linux/macOS 上不会自动枚举磁盘（那是 Windows 独有的行为），
  需要在管理台里手动添加根目录。
- macOS 首次运行可能提示「无法验证开发者」，执行：
      xattr -d com.apple.quarantine ./selfnas
- 播放 MKV/AVI/TS 需要 ffmpeg 在 PATH 里，或者管理台里填绝对路径。
  不装也能用，只是这些格式会降级成下载。

文档：https://github.com/sylvesterzhang/selfnas
许可：MIT
""".format(v=V)

README_NAME = 'README.txt'

# 平台 -> 压缩格式
TARGETS = [
    ('windows-amd64', 'zip'),
    ('linux-amd64', 'tar.gz'),
    ('linux-arm64', 'tar.gz'),
    ('darwin-amd64', 'tar.gz'),
    ('darwin-arm64', 'tar.gz'),
]

readme_bytes = README_TXT.replace('\n', '\r\n').encode('utf-8')

made = []

for plat, fmt in TARGETS:
    exe = os.path.join(DIST, f'selfnas-{V}-{plat}' + ('.exe' if plat.startswith('windows') else ''))
    if not os.path.exists(exe):
        print(f'跳过 {plat}：找不到 {exe}')
        continue

    if fmt == 'zip':
        out = os.path.join(DIST, f'selfnas-{V}-{plat}.zip')
        with zipfile.ZipFile(out, 'w', zipfile.ZIP_DEFLATED, compresslevel=9) as z:
            z.write(exe, 'selfnas.exe')
            # 文件名用 ASCII，避免 Windows 资源管理器解压 UTF-8 文件名时乱码
            z.writestr(README_NAME, readme_bytes)
    else:
        out = os.path.join(DIST, f'selfnas-{V}-{plat}.tar.gz')
        with tarfile.open(out, 'w:gz') as t:
            # 权限位显式给 0755：在 Windows 上打包拿不到真实的 exec 位
            ti = t.gettarinfo(exe, arcname='selfnas')
            ti.mode = 0o755
            ti.uid = ti.gid = 0
            ti.uname = ti.gname = ''
            with open(exe, 'rb') as f:
                t.addfile(ti, f)

            ri = tarfile.TarInfo(README_NAME)
            ri.size = len(readme_bytes)
            ri.mode = 0o644
            ri.uid = ri.gid = 0
            ri.uname = ri.gname = ''
            import io
            t.addfile(ri, io.BytesIO(readme_bytes))

    size = os.path.getsize(out)
    made.append((out, size))
    print(f'{os.path.basename(out):42s} {size/1024/1024:6.1f} MB')

# 校验和：用户下载后可以核对，尤其是从别处转存过的文件
print()
print('SHA256:')
lines = []
for out, _ in made:
    h = hashlib.sha256()
    with open(out, 'rb') as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b''):
            h.update(chunk)
    line = f'{h.hexdigest()}  {os.path.basename(out)}'
    lines.append(line)
    print('  ' + line)

with open(os.path.join(DIST, 'SHA256SUMS'), 'w', encoding='utf-8', newline='\n') as f:
    f.write('\n'.join(lines) + '\n')
print()
print('已生成 dist/SHA256SUMS')
