#!/bin/sh
# 验证：请求期间不再重复拉、同一游标只拉一次
ADB="$HOME/android-sdk/platform-tools/adb"
PKG=com.simpleagent.viewer
CX=540

"$ADB" shell cmd statusbar collapse 2>/dev/null
sleep 1
"$ADB" shell am force-stop $PKG
sleep 1
"$ADB" shell am start -n $PKG/.MainActivity >/dev/null 2>&1
sleep 8

# 打开侧栏 → 选第一个会话
"$ADB" shell input tap 75 175
sleep 2
"$ADB" shell input tap 540 470
sleep 8

"$ADB" logcat -c
echo "=== 滑到顶部 ==="
i=0
while [ $i -lt 40 ]; do
  i=$((i+1))
  "$ADB" shell input swipe $CX 500 $CX 1300 180
  sleep 0.15
done

echo "=== 停在顶部 10 秒（观察是否还在重复拉）==="
sleep 10

echo
echo "--- 加载次数（应很少，不是几十上百）---"
"$ADB" logcat -d -s SimpleAgent 2>/dev/null | /usr/bin/grep -cF "loadEarlierItems 开始"
echo "--- 跳过：游标已拉过 ---"
"$ADB" logcat -d -s SimpleAgent 2>/dev/null | /usr/bin/grep -cF "游标"
echo "--- items 序列 ---"
"$ADB" logcat -d -s SimpleAgent 2>/dev/null | /usr/bin/grep -oF "items=" | /usr/bin/wc -l
"$ADB" logcat -d -s SimpleAgent 2>/dev/null | /usr/bin/grep -F "refreshTimeline" | /usr/bin/grep -oE "items=[0-9]+ hasMore=[a-z]+ earlierPart=[0-9]+" | /usr/bin/uniq | /usr/bin/tail -8
echo "--- 最新 items ---"
"$ADB" logcat -d -s SimpleAgent 2>/dev/null | /usr/bin/grep -F "refreshTimeline" | /usr/bin/tail -1