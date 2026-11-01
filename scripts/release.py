# -*- coding: utf-8 -*-
"""把 dist/ 下的二进制打成可发布的压缩包，并生成 SHA256 校验和。

Windows 用 zip（双击解压就行），Linux/macOS 用 tar.gz（保留可执行权限位）。
"""
import os
import hashlib
import zipfile
import tarfile

def _version():
    """从 main.go 里读 const version —— 版本号只写在一处，改完不用再回来同步。"""
    with open('main.go', encoding='utf-8') as f:
        for line in f:
            s = line.strip()
            if s.startswith('const version'):
                return s.split('"')[1]
    raise SystemExit('main.go 里找不到 const version')


V = _version()
DIST = 'dist'

README_TXT = """selfnas __V__ - 自建文件服务器
================================================================

启动
----
Windows:    双击 selfnas.exe
Linux/macOS: ./selfnas

首次启动会自动生成 config.json（在程序旁边）。
账号 admin，密码 123456。

打开 http://127.0.0.1:8080/admin 登录，第一件事就是到「修改密码」里改掉它。

然后手机连同一个 WiFi，扫管理台里的二维码即可。

停止
----
按 Ctrl+C。后台运行时也可以在任务管理器里结束进程 ——
每次写库后都会立刻落盘，强杀不会丢「最近 / 收藏」。


开机自启 —— Linux / macOS
-------------------------
程序**不会**偷偷往 /etc 或 /Library 写东西（那需要 root，动的是系统目录）。
它只生成一份配置文件，把「装机命令」交给你执行。命令按用途分三种：

    (a) 开启自启（并立即启动）
    (b) 仅现在启动一次（下次开机不自动拉起）—— Linux 才有；macOS 用 launchd 不单列
    (c) 关闭并卸载自启

怎么拿到这三条命令 —— 任选其一：

  【方式一 · 最直观】打开管理台 →「系统设置」→「开机自启」卡片，
      三条命令带用途标签列在那里，点「复制」执行即可。

  【方式二 · 纯命令行 / 让 AI 无人值守装】启动程序后调接口，
      返回 JSON 里的 command / commandStart / commandOff 就是可直接执行的完整命令：

        # 1) 先启动一次（首次会生成 config.json），后台跑起来
        ./selfnas &

        # 2) 登录拿会话 cookie
        curl -s -c /tmp/sn.txt -X POST http://127.0.0.1:8080/api/login \\
             -H 'Content-Type: application/json' \\
             -d '{"user":"admin","pass":"123456"}'

        # 3) 取命令：command=开启 / commandStart=仅启动 / commandOff=关闭
        curl -s -b /tmp/sn.txt http://127.0.0.1:8080/api/autostart

  命令长这样（Linux systemd，路径以程序实际生成的为准）：
        # 开启自启
        sudo cp '/root/.config/selfnas/selfnas.service' /etc/systemd/system/selfnas.service \\
          && sudo systemctl daemon-reload && sudo systemctl enable --now selfnas
        # 仅现在启动
        sudo cp '/root/.config/selfnas/selfnas.service' /etc/systemd/system/selfnas.service \\
          && sudo systemctl daemon-reload && sudo systemctl start selfnas
        # 关闭并卸载
        sudo systemctl disable --now selfnas \\
          && sudo rm -f /etc/systemd/system/selfnas.service && sudo systemctl daemon-reload

  macOS 同理，用的是 LaunchDaemon（开机即起、以 root 跑、能绑 80/445）：
        # 开启自启
        sudo cp '<家目录>/.config/selfnas/cc.selfnas.server.plist' /Library/LaunchDaemons/ \\
          && sudo chown root:wheel /Library/LaunchDaemons/cc.selfnas.server.plist \\
          && sudo chmod 644 /Library/LaunchDaemons/cc.selfnas.server.plist \\
          && sudo launchctl bootstrap system /Library/LaunchDaemons/cc.selfnas.server.plist
        # 关闭并卸载
        sudo launchctl bootout system/cc.selfnas.server 2>/dev/null; \\
          sudo rm -f /Library/LaunchDaemons/cc.selfnas.server.plist

  ⚠️ 重点：.service / .plist 是**程序运行时生成**的，落在 ~/.config/selfnas/ 下。
     还没生成就先启动一次程序（或先调一次上面的 GET /api/autostart）。
     **不要自己手写一份** —— 里面的 exe 真实路径、运行用户写错，服务直接起不来。


开机自启 —— Windows
-------------------
不用敲命令，两种方式：

  【方式一】管理台 →「系统设置」→「开机自启」开关，点一下即可
      （优先建计划任务：开机即起、无黑窗、不用先登录桌面；建失败自动退回注册表启动项）。

  【方式二 · 调接口，AI 无人值守】POST /api/autostart，body 里 enable=true/false。
      PowerShell 示例：

        # 登录，保存会话
        $s = New-Object Microsoft.PowerShell.Commands.WebRequestSession
        Invoke-RestMethod -Uri http://127.0.0.1:8080/api/login -Method Post -WebSession $s `
          -ContentType 'application/json' -Body '{"user":"admin","pass":"123456"}'

        # 开启 / 关闭
        Invoke-RestMethod -Uri http://127.0.0.1:8080/api/autostart -Method Post -WebSession $s `
          -ContentType 'application/json' -Body '{"enable":true}'
        Invoke-RestMethod -Uri http://127.0.0.1:8080/api/autostart -Method Post -WebSession $s `
          -ContentType 'application/json' -Body '{"enable":false}'

        # 查状态（GET，返回 supported/enabled/mode/exe…）
        Invoke-RestMethod -Uri http://127.0.0.1:8080/api/autostart -WebSession $s

      注：Linux/macOS 也能 POST，但那边 POST 只是重新生成配置文件并返回命令，
      真正的启停要你把返回的命令执行一遍（写系统目录需要 root）。Windows 是直接生效。


注意
----
- 管理台默认允许局域网内任意设备访问（adminOnlyLocal=false）。
  只想本机访问，就在管理台里关掉，或 config.json 把 adminOnlyLocal 改 true。
- 根目录：Windows 首次启动自动枚举磁盘；Linux/macOS 也会扫「家目录 + /media、
  /mnt 下的挂载」。但**若用 systemd 以服务方式跑、而 unit 里没设 HOME 环境变量**，
  家目录探测会失败、根目录为空 —— 这时在管理台手动「添加目录」即可，
  或在 unit 里加一行 Environment=HOME=/root（或对应用户的家目录）。
- macOS 首次运行可能提示「无法验证开发者」，执行：
      xattr -d com.apple.quarantine ./selfnas
- 播放 MKV/AVI/TS 需要 ffmpeg 在 PATH 里，或者管理台里填绝对路径。
  不装也能用，只是这些格式会降级成下载。

文档：https://github.com/sylvesterzhang/selfnas
许可：MIT
""".replace('__V__', V)

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
