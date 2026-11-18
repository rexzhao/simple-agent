#!/usr/bin/env python3
"""从 uiautomator dump 里提取可见文本及其 y 坐标。

用来量化滚动漂移：记录加载前后同一条文本的 y 坐标差。
"""
import re
import sys

if len(sys.argv) < 2:
    print("用法: dump-texts.py <uiautomator.xml>", file=sys.stderr)
    sys.exit(2)

xml = open(sys.argv[1], encoding="utf-8").read()

# 每个 node 一行，提取 text 与 bounds
pattern = re.compile(r'text="([^"]*)"[^>]*?bounds="\[(\d+),(\d+)\]\[(\d+),(\d+)\]"')
rows = []
for m in pattern.finditer(xml):
    text = m.group(1).strip()
    if not text:
        continue
    x1, y1, x2, y2 = (int(m.group(i)) for i in range(2, 6))
    rows.append((y1, y2 - y1, text))

rows.sort()
for y, h, text in rows:
    print(f"y={y:5d} h={h:4d} | {text[:50]}")
