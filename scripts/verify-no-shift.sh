#!/bin/sh
# 零漂移验证（虚拟设备版）
# 滑到中段 → 截图 → 头部加载 60 → 截图 → 逐像素比对内容区
ADB="$HOME/android-sdk/platform-tools/adb"
PKG=com.simpleagent.viewer

SIZE=$("$ADB" shell wm size 2>/dev/null | tr -d '\r' | sed 's/.*: //')
W=$(echo "$SIZE" | cut -dx -f1); H=$(echo "$SIZE" | cut -dx -f2); CX=$((W / 2))
echo "屏幕 ${W}x${H}"

# 320x640 布局：顶部文字约 0..110，按钮 110..170，列表从 ~175 开始
LIST_TOP=$((H * 30 / 100))
LIST_BOT=$((H * 78 / 100))
echo "滑动区间 y: $LIST_TOP -> $LIST_BOT"

"$ADB" shell am force-stop $PKG; sleep 1
"$ADB" shell am start -S -n $PKG/.MainActivity -e screen scrolllab >/dev/null 2>&1
sleep 5

echo "=== 滑到中段 ==="
i=0
while [ $i -lt 12 ]; do
  i=$((i + 1))
  "$ADB" shell input swipe $CX $LIST_TOP $CX $LIST_BOT 300
  sleep 0.3
done
sleep 3

"$ADB" shell screencap -p /sdcard/sb.png
"$ADB" pull /sdcard/sb.png /tmp/sb.png >/dev/null 2>&1
echo "BEFORE 已拍"

echo "=== 定位「头部插入 60」按钮 ==="
"$ADB" shell uiautomator dump /sdcard/ui.xml >/dev/null 2>&1
"$ADB" pull /sdcard/ui.xml /tmp/ui.xml >/dev/null 2>&1
BOUNDS=$(tr '>' '>\n' < /tmp/ui.xml | grep '头部插入' | grep -o 'bounds="\[[0-9,]*\]\[[0-9,]*\]"' | head -1)
echo "bounds=$BOUNDS"
if [ -z "$BOUNDS" ]; then echo "找不到按钮" >&2; exit 1; fi

B=$(echo "$BOUNDS" | sed 's/bounds="//; s/"//')
X1=$(echo "$B" | sed 's/\[\([0-9]*\),\([0-9]*\)\].*/\1/')
Y1=$(echo "$B" | sed 's/\[\([0-9]*\),\([0-9]*\)\].*/\2/')
X2=$(echo "$B" | sed 's/.*\]\[\([0-9]*\),\([0-9]*\)\]/\1/')
Y2=$(echo "$B" | sed 's/.*\]\[\([0-9]*\),\([0-9]*\)\]/\2/')
BX=$(( (X1 + X2) / 2 )); BY=$(( (Y1 + Y2) / 2 ))
echo "点击 ($BX, $BY)"

"$ADB" logcat -c
"$ADB" shell input tap $BX $BY
sleep 5

"$ADB" shell screencap -p /sdcard/sa.png
"$ADB" pull /sdcard/sa.png /tmp/sa.png >/dev/null 2>&1
echo "AFTER 已拍"

echo
echo "=== 落位日志 ==="
"$ADB" logcat -d -s SimpleAgent 2>/dev/null | grep "滚动落位" | tail -3 || true

echo
echo "=== 内容区像素比对（跳过顶部 30%）==="
python3 /tmp/diff-shots.py /tmp/sb.png /tmp/sa.png --top $((H * 30 / 100)) --bottom $((H * 92 / 100))
RESULT=$?
echo "退出码: $RESULT"
exit 0