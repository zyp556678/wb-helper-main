"""生成应用图标（纯标准库，不依赖 Pillow）。

输出：
  installer/icons/icon-256.png     主图标（README / npm 页可用）
  installer/windows/app.ico        多尺寸 ICO（快捷方式、卸载项、安装包）

绘制思路：4 倍超采样后做盒式降采样得到抗锯齿边缘；图形刻意做得粗、对比高，
因为在 16×16 的任务栏/开始菜单里，细线条和渐变色阶都会糊成一团。
"""

import os
import struct
import zlib

# 品牌色：与面板设计令牌里的主色（青绿）一致
BG_TOP = (16, 172, 150)
BG_BOTTOM = (10, 130, 120)
FG = (255, 255, 255)

SS = 4  # 超采样倍数


def _rounded_rect_alpha(x, y, w, h, r):
    """点 (x, y) 落在圆角矩形内则返回 1，否则 0（坐标为浮点，四角用圆判定）。"""
    cx = min(max(x, r), w - r)
    cy = min(max(y, r), h - r)
    dx = x - cx
    dy = y - cy
    # 若点在"十字区域"内，一定在矩形内；否则用圆角半径判定
    if r <= x <= w - r or r <= y <= h - r:
        return 1.0 if 0 <= x <= w and 0 <= y <= h else 0.0
    return 1.0 if dx * dx + dy * dy <= r * r else 0.0


def _in_poly(x, y, pts):
    """射线法判断点是否在多边形内（设计画布坐标）。"""
    inside = False
    n = len(pts)
    for i in range(n):
        x1, y1 = pts[i]
        x2, y2 = pts[(i + 1) % n]
        if (y1 > y) != (y2 > y):
            xint = x1 + (y - y1) * (x2 - x1) / (y2 - y1)
            if x < xint:
                inside = not inside
    return inside


def _rect(x1, y1, x2, y2):
    return [(x1, y1), (x2, y1), (x2, y2), (x1, y2)]


# 双向箭头（⇄）：上箭头向左、下箭头向右，表示"网关把请求与响应对向中继"。
# 用多边形而不是"矩形 + 三角形判定"分开算：后者在形状相接处会出现缺口或毛刺，
# 16×16 下尤其明显。
_ARROW_SHAPES = [
    _rect(32, 35, 82, 47),          # 上：杆
    [(16, 41), (36, 29), (36, 53)],  # 上：箭头（朝左）
    _rect(18, 53, 68, 65),          # 下：杆
    [(84, 59), (64, 47), (64, 71)],  # 下：箭头（朝右）
]


def _arrows_glyph(x, y, size):
    """把点换算到 100×100 设计画布，判断是否落在箭头图形内。"""
    u = size / 100.0
    px, py = x / u, y / u
    for shape in _ARROW_SHAPES:
        if _in_poly(px, py, shape):
            return 1.0
    return 0.0


def render(size):
    """渲染一张 size×size 的 RGBA 像素缓冲（返回 bytes）。"""
    big = size * SS
    px = bytearray(big * big * 4)
    r = big * 0.22  # 圆角半径

    for by in range(big):
        for bx in range(big):
            gx, gy = bx + 0.5, by + 0.5
            a = _rounded_rect_alpha(gx, gy, big - 1, big - 1, r)
            if a <= 0:
                continue
            # 垂直渐变
            t = gy / big
            cr = int(BG_TOP[0] + (BG_BOTTOM[0] - BG_TOP[0]) * t)
            cg = int(BG_TOP[1] + (BG_BOTTOM[1] - BG_TOP[1]) * t)
            cb = int(BG_TOP[2] + (BG_BOTTOM[2] - BG_TOP[2]) * t)
            g = _arrows_glyph(gx, gy, float(big))
            if g > 0:
                cr, cg, cb = FG
            off = (by * big + bx) * 4
            px[off] = cr
            px[off + 1] = cg
            px[off + 2] = cb
            px[off + 3] = 255

    # 盒式降采样
    out = bytearray(size * size * 4)
    area = SS * SS
    for y in range(size):
        for x in range(size):
            ar = ag = ab = aa = 0
            for dy in range(SS):
                for dx in range(SS):
                    off = ((y * SS + dy) * big + (x * SS + dx)) * 4
                    ar += px[off]
                    ag += px[off + 1]
                    ab += px[off + 2]
                    aa += px[off + 3]
            o = (y * size + x) * 4
            out[o] = ar // area
            out[o + 1] = ag // area
            out[o + 2] = ab // area
            out[o + 3] = aa // area
    return bytes(out)


def png_encode(size, rgba):
    """把 RGBA 缓冲编码成 PNG（filter 0，无交错）。"""
    raw = bytearray()
    stride = size * 4
    for y in range(size):
        raw.append(0)  # filter type 0
        raw += rgba[y * stride:(y + 1) * stride]

    def chunk(tag, data):
        return (
            struct.pack(">I", len(data))
            + tag
            + data
            + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        )

    ihdr = struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0)  # 8bit RGBA
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", zlib.compress(bytes(raw), 9))
        + chunk(b"IEND", b"")
    )


def ico_encode(entries):
    """entries: [(size, png_bytes)] → .ico 字节流。

    用 PNG 负载而不是 BMP：Vista 以后 shell 都支持 ICO 内嵌 PNG，
    而 PNG 负载更小、不需要自己拼 DIB 头与 AND 掩码。
    """
    count = len(entries)
    header = struct.pack("<HHH", 0, 1, count)
    offset = 6 + 16 * count
    directory = bytearray()
    payload = bytearray()
    for size, data in entries:
        w = 0 if size >= 256 else size  # 256 在目录里记 0
        directory += struct.pack(
            "<BBBBHHII", w, w, 0, 0, 1, 32, len(data), offset
        )
        payload += data
        offset += len(data)
    return bytes(header + directory + payload)


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

    sizes = [16, 24, 32, 48, 64, 128, 256]
    rendered = {}
    for s in sizes:
        rendered[s] = render(s)
        print(f"  渲染 {s}x{s}")

    icons_dir = os.path.join(root, "icons")
    win_dir = os.path.join(root, "windows")
    os.makedirs(icons_dir, exist_ok=True)
    os.makedirs(win_dir, exist_ok=True)

    png256 = png_encode(256, rendered[256])
    with open(os.path.join(icons_dir, "icon-256.png"), "wb") as f:
        f.write(png256)

    entries = [(s, png_encode(s, rendered[s])) for s in sizes]
    with open(os.path.join(win_dir, "app.ico"), "wb") as f:
        f.write(ico_encode(entries))

    print("  已写 installer/icons/icon-256.png")
    print("  已写 installer/windows/app.ico")


if __name__ == "__main__":
    main()
