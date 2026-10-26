# -*- coding: utf-8 -*-
"""交叉编译五个平台，产物丢进 dist/，然后把本地 exe 也更新一份。

为什么不用 Makefile：Windows 上默认没有 make，而 Go 用户基本都有 Python 或
至少能看懂这段脚本在干什么 —— 单独复制一条命令出来手跑也行。

用法：
    python scripts/build.py            # 全平台
    python scripts/build.py windows    # 只编 Windows（迭代时快一点）
"""
import os
import subprocess
import sys

DIST = 'dist'

# 平台 -> dist 里的文件名后缀
TARGETS = [
    ('windows', 'amd64'),
    ('linux', 'amd64'),
    ('linux', 'arm64'),
    ('darwin', 'amd64'),
    ('darwin', 'arm64'),
]


def version():
    """从 main.go 里读 const version，避免版本号两处写、改一处忘一处。"""
    with open('main.go', encoding='utf-8') as f:
        for line in f:
            s = line.strip()
            if s.startswith('const version'):
                return s.split('"')[1]
    raise SystemExit('main.go 里找不到 const version')


def build(goos, goarch, out, v):
    env = dict(os.environ)
    env.update({
        'GOOS': goos,
        'GOARCH': goarch,
        # 纯 Go（sqlite 用 modernc.org/sqlite），关掉 cgo 才能交叉编译
        'CGO_ENABLED': '0',
    })
    cmd = [
        'go', 'build',
        # -trimpath：不把构建机路径（F:\项目\selfnas）写进二进制，
        # 否则用户的杀软/panic 堆栈里会出现别人的目录名
        '-trimpath',
        # -s -w：去掉符号表和调试信息，体积小 30% 左右
        '-ldflags', '-s -w',
        '-o', out, '.',
    ]
    print(f'  编译 {goos}/{goarch} -> {out}')
    subprocess.run(cmd, check=True, env=env)


def main():
    v = version()
    only = sys.argv[1].lower() if len(sys.argv) > 1 else ''
    os.makedirs(DIST, exist_ok=True)

    print(f'版本 {v}')
    for goos, goarch in TARGETS:
        if only and only not in (goos, f'{goos}-{goarch}', goarch):
            continue
        name = f'selfnas-{v}-{goos}-{goarch}' + ('.exe' if goos == 'windows' else '')
        build(goos, goarch, os.path.join(DIST, name), v)
        # 同时把 Windows 版放到项目根，方便本地直接双击
        if goos == 'windows' and goarch == 'amd64':
            build(goos, goarch, 'selfnas.exe', v)

    print('完成。打包请跑：python scripts/release.py')


if __name__ == '__main__':
    main()
