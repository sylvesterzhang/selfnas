from pathlib import Path
from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "docs" / "screenshots" / "remote-access.png"
FONT = "C:/Windows/Fonts/msyh.ttc"
FONT_BOLD = "C:/Windows/Fonts/msyhbd.ttc"

W, H = 1600, 820
img = Image.new("RGB", (W, H), "#F7F9FC")
d = ImageDraw.Draw(img)

font_title = ImageFont.truetype(FONT_BOLD, 48)
font_subtitle = ImageFont.truetype(FONT, 25)
font_section = ImageFont.truetype(FONT_BOLD, 29)
font_card = ImageFont.truetype(FONT_BOLD, 27)
font_body = ImageFont.truetype(FONT, 22)
font_small = ImageFont.truetype(FONT, 19)
font_tiny = ImageFont.truetype(FONT, 17)

blue = "#185FA5"
teal = "#0F6E56"
coral = "#C2412D"
ink = "#172033"
muted = "#64748B"
line = "#B8C7D9"
white = "#FFFFFF"

# Header
d.text((80, 58), "在外面，也能访问家里的 SelfNAS", fill=ink, font=font_title)
d.text((84, 126), "不需要把文件上传到网盘，也不需要手动配置路由器端口映射", fill=muted, font=font_subtitle)

# Main containers
home = (80, 220, 470, 660)
channel = (585, 220, 1015, 660)
phone = (1130, 220, 1520, 660)

def rounded(box, fill, outline, radius=28, width=3):
    d.rounded_rectangle(box, radius=radius, fill=fill, outline=outline, width=width)

rounded(home, "#EAF3FF", "#9CC7EE")
rounded(channel, "#EAF8F2", "#8BD3B7")
rounded(phone, "#FFF5E7", "#F2C487")

d.text((120, 255), "家里的电脑 / NAS", fill=blue, font=font_section)
d.text((120, 302), "SelfNAS 运行在自己的机器上", fill=muted, font=font_small)

# Home device card
rounded((125, 370, 425, 505), white, "#5B9BD5", 20, 3)
d.rounded_rectangle((160, 402, 390, 438), radius=8, fill="#DCEBFA")
d.rectangle((178, 438, 372, 452), fill="#5B9BD5")
d.text((175, 464), "文件 · 音乐 · 视频", fill=ink, font=font_card)
d.text((150, 545), "SelfNAS 客户端", fill=blue, font=font_card)
d.text((150, 585), "主动建立安全访问通道", fill=muted, font=font_small)

# Channel title
d.text((625, 255), "外网访问通道", fill=teal, font=font_section)
d.text((625, 302), "按你的场景选择一种", fill=muted, font=font_small)

# Tailscale card
rounded((625, 355, 975, 455), white, "#40A77A", 18, 3)
d.ellipse((650, 386, 696, 432), fill="#D8F1E6", outline=teal, width=2)
d.line((662, 409, 684, 409), fill=teal, width=5)
d.text((720, 374), "Tailscale", fill=ink, font=font_card)
d.text((720, 415), "自己用 · 地址稳定 · 优先点对点", fill=teal, font=font_small)

# Cloudflare card
rounded((625, 485, 975, 605), white, "#DE8A58", 18, 3)
d.ellipse((650, 516, 696, 562), fill="#FCE5D8", outline=coral, width=2)
d.arc((658, 526, 688, 554), 180, 360, fill=coral, width=3)
d.text((720, 495), "Cloudflare", fill=ink, font=font_card)
d.text((720, 537), "临时地址 · 一键分享", fill=coral, font=font_small)
d.text((720, 568), "固定地址 · 自有域名长期使用", fill=coral, font=font_small)

# Phone
d.text((1170, 255), "外面的手机", fill="#A15C0A", font=font_section)
d.text((1170, 302), "浏览器直接打开", fill=muted, font=font_small)
rounded((1225, 370, 1425, 570), white, "#D99A3F", 28, 4)
d.rounded_rectangle((1245, 405, 1405, 518), radius=10, fill="#FFF8EE", outline="#E9C17D", width=2)
d.line((1300, 540, 1350, 540), fill="#D99A3F", width=5)
d.text((1260, 425), "SelfNAS", fill=ink, font=font_card)
d.text((1260, 469), "/files", fill="#A15C0A", font=font_card)
d.text((1255, 600), "文件 · 音乐 · 视频", fill="#A15C0A", font=font_body)

# Arrows
def arrow(x1, y1, x2, y2, color):
    d.line((x1, y1, x2, y2), fill=color, width=6)
    d.polygon([(x2, y2), (x2 - 18, y2 - 12), (x2 - 18, y2 + 12)], fill=color)

arrow(470, 440, 580, 440, blue)
arrow(1020, 440, 1125, 440, teal)

# Footer labels
d.text((130, 710), "局域网内：直接访问局域网 IP", fill=blue, font=font_body)
d.text((850, 710), "外网：Tailscale 或 Cloudflare", fill=teal, font=font_body)

img.save(OUT, "PNG", optimize=True)
print(OUT)
