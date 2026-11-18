#!/bin/sh
# 真实会话的滚动漂移验证
#
# 为什么不能像验证页那样比对整屏像素：
#   触发加载必须"滑到顶部"，而这一滑本身就改变了位置，
#   前后截图必然不同 —— 那验的是"我滑了多少"，不是"加载让它跳了多少"。
#
# 正确做法：**只观察加载发生的那一瞬间**。
#   LazyColumn 的 composable 在滚动/布局时不会重建，所以这里改用
#   "加载前后各截一张，但都停在同一个滚动位置"的方式：
#     1. 滑到顶部，让界面停在顶部（此时可能有加载在飞）
#     2. 等加载彻底结束、界面稳定
#     3. 记录当前屏幕上第一条可见消息的文字
#     4. 再次触发一次加载（此时还在顶部）
#     5. 加载结束后，检查第 3 步那条消息是否还在屏幕上同一位置
#
# 第 4 步是关键：用户已经在顶部时再加载，如果补偿正确，
# 他刚才看着的那条应该纹丝不动（内容插在它**上方**）。
ADB="$HOME/android-sdk/platform-tools/adb"
PKG=com.simpleagent.viewer
SIZE=$("$ADB" shell wm size 2>/dev/null | tr -d '\r' | sed 's/.*: //')
W=$(echo "$SIZE" | cut -dx -f1); H=$(echo "$SIZE" | cut -dx -f2); CX=$((W / 2))
Y1=$((H * 22 / 100)); Y2=$((H * 55 / 100))

"$ADB" shell cmd statusbar collapse 2>/dev/null
sleep 1

echo "=== 滑到顶部（让界面停在顶部）==="
i=0
while [ $i -lt 40 ]; do
  i=$((i+1))
  "$ADB" shell input swipe $CX $Y1 $CX $Y2 200
  sleep 0.2
done
sleep 5

echo "=== 等一切稳定 ==="
sleep 5
"$ADB" shell screencap -p /sdcard/s1.png
"$ADB" pull /sdcard/s1.png /tmp/s1.png >/dev/null 2>&1

echo "=== 记录 items 与加载状态 ==="
"$ADB" logcat -d -s SimpleAgent 2>/dev/null | grep -o "refreshTimeline：items=[0-9]*" | tail -1

echo "=== 再触发一次加载（安静地，不再滑动）==="
# 轻触顶部状态行区域不滚动，只让 snapshotFlow 重新评估
"$ADB" shell input swipe $CX $((H * 30 / 100)) $CX $((H * 34 / 100)) 800
sleep 2
"$ADB" shell input swipe $CX $((H * 34 / 100)) $CX $((H * 30 / 100)) 800
sleep 8

"$ADB" shell screencap -p /sdcard/s2.png
"$ADB" pull /sdcard/s2.png /tmp/s2.png >/dev/null 2>&1

echo "=== items 变化 ==="
"$ADB" logcat -d -s SimpleAgent 2>/dev/null | grep -o "refreshTimeline：items=[0-9]*" | tail -3
echo "=== 落位日志 ==="
"$ADB" logcat -d -s SimpleAgent 2>/dev/null | grep "滚动落位" | tail -5
ls -l /tmp/s1.png /tmp/s2.png