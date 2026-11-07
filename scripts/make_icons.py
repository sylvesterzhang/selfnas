"""生成站点图标 icon-180 / icon-192 / icon-512 / icon-32 / favicon.ico，
以及无封面歌曲的通用专辑图。

站点图标设计（2026-11-06 重做）：**黑底 + 白色大写 S**，纯字标。
原来的「蓝色渐变 + 白色云朵 + 三条横线」被用户否掉（"太难看"、"还有渐变色"）。
这个项目在 GitHub 上就是纯文字，所以图标也做成字标：一个字母，不做图形、
不做渐变、不做阴影。黑底是为了让白字在浅色/深色标签栏上都立得住。

三点实现细节：
  · 圆角画在方图内部，四周留白——iOS/Android 的遮罩自己会再裁一轮，画满会被切。
  · 用字形实际包围盒做光学居中，不用 em 框（em 框会让字母整体偏下）。
  · 全方形不透明：iOS 的 apple-touch-icon 会把透明区域填成黑色。

cover-default.jpg 给 iOS 锁屏 MediaSession artwork 兜底用：歌里没内嵌封面、
目录里也没 cover.jpg 时，服务端返回这张，而不是 404 让系统回退成站点图标。

改配色/字形直接改下面的常量后重跑即可。
用法：python scripts/make_icons.py
"""
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

OUT = Path(__file__).resolve().parent.parent / "web"
BASE = 512
SS = 4                      # 4 倍超采样，缩小时边缘才干净
S = BASE * SS
INK = (10, 10, 12)          # 底：近黑，不用纯 #000（纯黑在深色标签栏里跟背景糊成一片）
WHITE = (255, 255, 255)
RADIUS = 0.225              # 圆角比例：跟 iOS 图标观感接近
LETTER = "S"
LETTER_RATIO = 0.63         # 字号 / 画布；实测 0.63 留白最舒服，0.66 就顶了
FONT_CANDIDATES = [
    "C:/Windows/Fonts/seguibl.ttf",   # Segoe UI Black：笔画最厚，16px 不糊
    "C:/Windows/Fonts/arialbd.ttf",
    "/System/Library/Fonts/Supplemental/Arial Bold.ttf",
    "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
]


def sc(v):
    return int(round(v * SS))


def pick_font(px):
    """按顺序找第一个存在的字体；都没有就报错，别静默画出一个空图标。"""
    for p in FONT_CANDIDATES:
        if Path(p).exists():
            return ImageFont.truetype(p, px)
    raise SystemExit("找不到可用的粗体字体，改 FONT_CANDIDATES")


def draw_icon():
    # 透明画布 + 圆角底：标签栏里是个圆角块（深色底上也认得出轮廓）。
    # iOS 会把透明角填成黑色——本来就是黑底，看不出来，所以不用为它单独出一版。
    im = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    d = ImageDraw.Draw(im)
    d.rounded_rectangle([0, 0, S - 1, S - 1], radius=sc(RADIUS * BASE), fill=INK)
    f = pick_font(sc(LETTER_RATIO * BASE))
    box = d.textbbox((0, 0), LETTER, font=f)
    w, h = box[2] - box[0], box[3] - box[1]
    d.text(((S - w) / 2 - box[0], (S - h) / 2 - box[1]), LETTER, font=f, fill=WHITE)
    del d
    return im


def main():
    big = draw_icon()
    for size in (32, 180, 192, 512):
        p = OUT / f"icon-{size}.png"
        big.resize((size, size), Image.LANCZOS).save(p, "PNG", optimize=True)
        print(f"{p}  {size}x{size}")

    # favicon.ico：老浏览器/书签栏只认这个，多塞几档尺寸由系统挑
    ico = OUT / "favicon.ico"
    big.resize((256, 256), Image.LANCZOS).save(ico, sizes=[(16, 16), (32, 32), (48, 48)])
    print(f"{ico}  16/32/48")

    make_cover_default()


def make_cover_default():
    """640x640 通用专辑图：深蓝底 + 白色双八分音符。锁屏小图也能一眼认出是音乐。"""
    size, ss = 640, 2
    s = size * ss
    im = Image.new("RGB", (s, s))
    d = ImageDraw.Draw(im)
    top, bottom = (18, 42, 72), (36, 92, 150)
    for y in range(s):
        t = y / (s - 1)
        c = tuple(round(top[i] + (bottom[i] - top[i]) * t) for i in range(3))
        d.line([(0, y), (s, y)], fill=c)

    def k(v):
        return int(round(v * ss))

    # 后面垫一个更深的圆，像唱片也像光晕，避免纯色块太死
    d.ellipse([k(96), k(96), k(544), k(544)], fill=(14, 34, 60))

    # 两个符头 + 两根符干 + 一条符尾横梁（连起来就是一对八分音符）
    d.ellipse([k(150), k(372), k(258), k(452)], fill=WHITE)
    d.ellipse([k(360), k(332), k(468), k(412)], fill=WHITE)
    d.rounded_rectangle([k(238), k(190), k(256), k(410)], radius=k(8), fill=WHITE)
    d.rounded_rectangle([k(448), k(176), k(466), k(370)], radius=k(8), fill=WHITE)
    d.polygon([(k(238), k(190)), (k(466), k(150)), (k(466), k(198)), (k(238), k(238))], fill=WHITE)
    del d

    p = OUT / "cover-default.jpg"
    im.resize((size, size), Image.LANCZOS).save(p, "JPEG", quality=88, optimize=True)
    print(f"{p}  {size}x{size}")


if __name__ == "__main__":
    main()
