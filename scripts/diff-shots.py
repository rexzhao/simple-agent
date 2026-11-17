#!/usr/bin/env python3
"""比对两张截图，判断"加载前后视觉上有没有漂移"。

为什么不用文件大小或整体哈希：顶部状态行本来就会变（"加载更早" →
"正在加载"→ "已加载"），整体比对必然不同。真正要验证的是**内容区域
一像素都没动**。

用法：
    python3 scripts/diff-shots.py before.png after.png [--top N] [--bottom N]

--top/--bottom 是要忽略的像素行范围（裁掉顶部状态行/按钮区）。
退出码 0 = 内容区完全一致（零漂移）；1 = 有差异。
"""
import sys

# Windows 控制台默认 GBK，输出 ✓/✗ 会抛 UnicodeEncodeError。
# 统一改成 ASCII 标记，跨平台安全。
OK = "[OK]"
NG = "[NG]"

try:
    from PIL import Image, ImageChops
except ImportError:
    print("需要 Pillow：pip install pillow", file=sys.stderr)
    sys.exit(2)


def load(path):
    return Image.open(path).convert("RGB")


def diff_region(a, b, top, bottom):
    """返回 (差异像素数, 总像素数, 首个差异的 y 坐标)。"""
    w = min(a.width, b.width)
    h = min(a.height, b.height)
    bottom = min(bottom, h)
    if top >= bottom:
        return 0, 0, -1

    box = (0, top, w, bottom)
    ra = a.crop(box)
    rb = b.crop(box)
    delta = ImageChops.difference(ra, rb)
    # 转灰度后找非零像素
    gray = delta.convert("L")
    histogram = gray.histogram()
    total = ra.width * ra.height
    different = total - histogram[0]
    first_y = -1
    if different:
        for y in range(gray.height):
            row = gray.crop((0, y, gray.width, y + 1))
            if row.getextrema()[1] != 0:
                first_y = y + top
                break
    return different, total, first_y


def main(argv):
    if len(argv) < 3:
        print(__doc__)
        return 2
    before_path, after_path = argv[1], argv[2]

    top = 0
    bottom = 10 ** 9
    i = 3
    while i < len(argv):
        if argv[i] == "--top":
            top = int(argv[i + 1]); i += 2
        elif argv[i] == "--bottom":
            bottom = int(argv[i + 1]); i += 2
        else:
            i += 1

    a, b = load(before_path), load(after_path)
    print(f"图片尺寸: {a.width}x{a.height} vs {b.width}x{b.height}")
    if (a.width, a.height) != (b.width, b.height):
        print("尺寸不同，无法逐像素比对", file=sys.stderr)
        return 1

    print(f"比对区域: y = {top} .. {min(bottom, a.height)}")

    different, total, first_y = diff_region(a, b, top, bottom)
    if total == 0:
        print("区域为空", file=sys.stderr)
        return 2

    ratio = different / total * 100
    print(f"差异像素: {different} / {total}  ({ratio:.3f}%)")
    if different == 0:
        print(f"结论: 内容区完全一致 —— 零漂移 {OK}")
        return 0
    print(f"结论: 有漂移 {NG}  首个差异出现在 y = {first_y}")
    return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
