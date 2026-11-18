#!/bin/sh
# 验证：多次"滑到顶"能持续加载，且每次加载后内容位置稳定
ADB="$HOME/android-sdk/platform-tools/adb"
PKG=com.simpleagent.viewer
CX=540

"$ADB" logcat -c

round=1
while [ $round -le 4 ]; do
  echo "=========== 第 $round 轮 ==========="

  # 记录加载前的首条内容
  "$ADB" shell uiautomator dump /sdcard/r$round-b.xml >/dev/null 2>&1
  "$ADB" pull /sdcard/r$round-b.xml /tmp/r$round-b.xml >/dev/null 2>&1
  echo "--- 加载前首条 ---"
  python3 /tmp/dump-texts.py /tmp/r$round-b.xml | /usr/bin/sed -n '3,5p'

  # 滑到顶部触发一页
  i=0
  while [ $i -lt 8 ]; do
    i=$((i+1))
    "$ADB" shell input swipe $CX 500 $CX 1300 180
    sleep 0.15
  done
  sleep 6

  "$ADB" shell uiautomator dump /sdcard/r$round-a.xml >/dev/null 2>&1
  "$ADB" pull /sdcard/r$round-a.xml /tmp/r$round-a.xml >/dev/null 2>&1
  echo "--- 加载后首条 ---"
  python3 /tmp/dump-texts.py /tmp/r$round-a.xml | /usr/bin/sed -n '3,5p'

  echo "--- items ---"
  "$ADB" logcat -d -s SimpleAgent 2>/dev/null | /usr/bin/grep -F "refreshTimeline" | /usr/bin/tail -1 | /usr/bin/grep -oE "items=[0-9]+|earlierPart=[0-9]+|pages=[0-9]+" | /usr/bin/tr '\n' ' '
  echo

  # 离开顶部再回来，才能触发下一轮
  "$ADB" shell input swipe $CX 1300 $CX 400 250
  sleep 2
  round=$((round+1))
done

echo "=========== 汇总 ==========="
echo "加载次数: $("$ADB" logcat -d -s SimpleAgent 2>/dev/null | /usr/bin/grep -cF 'loadEarlierItems 开始')"
echo "items 变化:"
"$ADB" logcat -d -s SimpleAgent 2>/dev/null | /usr/bin/grep -F "refreshTimeline" | /usr/bin/grep -oE "items=[0-9]+" | /usr/bin/uniq | /usr/bin/tail -10