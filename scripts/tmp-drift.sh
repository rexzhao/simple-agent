#!/bin/sh
# 真实会话漂移验证：量化"加载前后，同一条消息的 y 坐标变化"
#
# 判据：用户在顶部时再触发加载，新内容插在上方，他正看着的那条
# 应该**纹丝不动**（y 坐标基本不变）。如果 y 明显变了，就是漂移。
ADB="$HOME/android-sdk/platform-tools/adb"
PKG=com.simpleagent.viewer
W=1080; H=2392; CX=540

"$ADB" shell cmd statusbar collapse 2>/dev/null
sleep 1

echo "=== 逐步滑到顶部 ==="
i=0
while [ $i -lt 40 ]; do
  i=$((i+1))
  "$ADB" shell input swipe $CX 500 $CX 1300 180
  sleep 0.15
done

echo "=== 等加载与布局稳定 ==="
sleep 10

"$ADB" shell uiautomator dump /sdcard/p1.xml >/dev/null 2>&1
"$ADB" pull /sdcard/p1.xml /tmp/p1.xml >/dev/null 2>&1
echo "--- BEFORE 文本 ---"
python3 /tmp/dump-texts.py /tmp/p1.xml | /usr/bin/head -12

echo
echo "=== items 状态 ==="
"$ADB" logcat -d -s SimpleAgent 2>/dev/null | /usr/bin/grep -o "refreshTimeline：items=[0-9]*" | /usr/bin/tail -2

echo
echo "=== 触发加载（顶部轻推）==="
"$ADB" logcat -c
"$ADB" shell input swipe $CX 700 $CX 1400 400
sleep 1
"$ADB" shell input swipe $CX 1400 $CX 700 400
sleep 8

"$ADB" shell uiautomator dump /sdcard/p2.xml >/dev/null 2>&1
"$ADB" pull /sdcard/p2.xml /tmp/p2.xml >/dev/null 2>&1
echo "--- AFTER 文本 ---"
python3 /tmp/dump-texts.py /tmp/p2.xml | /usr/bin/head -12

echo
echo "=== items 变化 ==="
"$ADB" logcat -d -s SimpleAgent 2>/dev/null | /usr/bin/grep -o "refreshTimeline：items=[0-9]*" | /usr/bin/tail -3
echo "=== 加载次数 ==="
"$ADB" logcat -d -s SimpleAgent 2>/dev/null | /usr/bin/grep -c "loadEarlierItems 开始"
echo "=== 落位日志 ==="
"$ADB" logcat -d -s SimpleAgent 2>/dev/null | /usr/bin/grep "滚动落位" | /usr/bin/tail -4