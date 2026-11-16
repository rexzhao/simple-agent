#!/usr/bin/env python3
"""检查 .so 是否满足 Android 16 KB 页大小要求。

用法：
    python3 scripts/check-16kb.py <apk 或目录或 .so...>

背景：Android 15 起，16 KB 页大小的设备要求原生库满足两条规则
（https://developer.android.com/guide/practices/page-sizes）：
  1. 每个 PT_LOAD 段的 p_align >= 16384
  2. 每个 PT_LOAD 段的 p_offset 与 p_vaddr 在 16384 下同余
不满足时，安装/启动会弹「此应用不符合 16 KB 对齐要求」。

本脚本只读文件，用来在构建后自检。退出码 0 表示全部合格。
"""
import glob
import os
import struct
import sys
import tempfile
import zipfile

PAGE = 16384


def load_segments(path):
    """返回 [(p_offset, p_vaddr, p_align)]，只含 PT_LOAD。非 ELF 返回 None。"""
    with open(path, "rb") as f:
        data = f.read()
    if data[:4] != b"\x7fELF":
        return None
    is64 = data[4] == 2
    endian = "<" if data[5] == 1 else ">"
    if is64:
        e_phoff = struct.unpack_from(endian + "Q", data, 0x20)[0]
        e_phentsize = struct.unpack_from(endian + "H", data, 0x36)[0]
        e_phnum = struct.unpack_from(endian + "H", data, 0x38)[0]
    else:
        e_phoff = struct.unpack_from(endian + "I", data, 0x1C)[0]
        e_phentsize = struct.unpack_from(endian + "H", data, 0x2A)[0]
        e_phnum = struct.unpack_from(endian + "H", data, 0x2C)[0]

    segs = []
    for i in range(e_phnum):
        off = e_phoff + i * e_phentsize
        if struct.unpack_from(endian + "I", data, off)[0] != 1:  # PT_LOAD
            continue
        if is64:
            p_offset = struct.unpack_from(endian + "Q", data, off + 0x08)[0]
            p_vaddr = struct.unpack_from(endian + "Q", data, off + 0x10)[0]
            p_align = struct.unpack_from(endian + "Q", data, off + 0x30)[0]
        else:
            p_offset = struct.unpack_from(endian + "I", data, off + 0x04)[0]
            p_vaddr = struct.unpack_from(endian + "I", data, off + 0x08)[0]
            p_align = struct.unpack_from(endian + "I", data, off + 0x1C)[0]
        segs.append((p_offset, p_vaddr, p_align))
    return segs


def check_one(path, label):
    """检查单个 .so，返回问题列表（空表示通过）。"""
    segs = load_segments(path)
    if segs is None:
        return [f"{label}: 不是 ELF 文件"]
    if not segs:
        return [f"{label}: 没有 PT_LOAD 段"]

    problems = []
    for idx, (p_offset, p_vaddr, p_align) in enumerate(segs):
        if p_align < PAGE:
            problems.append(
                f"{label}: LOAD[{idx}] p_align=0x{p_align:x} < 0x{PAGE:x}")
        delta = (p_offset - p_vaddr) % PAGE
        if delta != 0:
            problems.append(
                f"{label}: LOAD[{idx}] (offset-vaddr)%0x{PAGE:x}=0x{delta:x} "
                f"[offset=0x{p_offset:x} vaddr=0x{p_vaddr:x}]")
    return problems


def check_apk(apk):
    """解出 APK 里的 .so 逐个检查。"""
    problems = []
    total = 0
    with tempfile.TemporaryDirectory() as tmp:
        with zipfile.ZipFile(apk) as zf:
            names = [n for n in zf.namelist() if n.endswith(".so")]
            if not names:
                return [f"{apk}: APK 里没有 .so"], 0
            for name in names:
                zf.extract(name, tmp)
                total += 1
                problems += check_one(os.path.join(tmp, name), name)
    return problems, total


def check_zip_align(apk):
    """检查 APK zip 里未压缩 .so 的 16 KB 边界对齐（AGP 会自动做）。"""
    problems = []
    with open(apk, "rb") as f:
        data = f.read()

    eocd = -1
    for i in range(len(data) - 22, max(-1, len(data) - 22 - 65535), -1):
        if data[i:i + 4] == b"PK\x05\x06":
            eocd = i
            break
    if eocd < 0:
        return ["找不到 zip EOCD"]

    cd_size, cd_off = struct.unpack_from("<II", data, eocd + 12)
    pos = cd_off
    while pos < cd_off + cd_size and data[pos:pos + 4] == b"PK\x01\x02":
        method = struct.unpack_from("<H", data, pos + 10)[0]
        name_len = struct.unpack_from("<H", data, pos + 28)[0]
        extra_len = struct.unpack_from("<H", data, pos + 30)[0]
        cmt_len = struct.unpack_from("<H", data, pos + 32)[0]
        lho = struct.unpack_from("<I", data, pos + 42)[0]
        name = data[pos + 46:pos + 46 + name_len].decode("utf-8", "replace")

        if name.endswith(".so") and method == 0:  # 只查未压缩的
            if data[lho:lho + 4] == b"PK\x03\x04":
                ln_len = struct.unpack_from("<H", data, lho + 26)[0]
                le_len = struct.unpack_from("<H", data, lho + 28)[0]
                data_off = lho + 30 + ln_len + le_len
                if data_off % PAGE != 0:
                    problems.append(
                        f"{name}: zip 内偏移 0x{data_off:x} 未按 16 KB 对齐")
        pos += 46 + name_len + extra_len + cmt_len
    return problems


def main(args):
    if not args:
        print(__doc__)
        return 2

    problems = []
    total = 0
    for arg in args:
        if arg.endswith(".apk"):
            p, n = check_apk(arg)
            problems += p
            total += n
            problems += check_zip_align(arg)
        elif os.path.isdir(arg):
            for so in sorted(glob.glob(os.path.join(arg, "**", "*.so"),
                                       recursive=True)):
                total += 1
                problems += check_one(so, os.path.relpath(so, arg))
        else:
            total += 1
            problems += check_one(arg, arg)

    if problems:
        print(f"16 KB 对齐检查：{total} 个 .so，发现 {len(problems)} 个问题")
        for p in problems:
            print("  " + p)
        return 1
    print(f"16 KB 对齐检查：{total} 个 .so 全部合格")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))