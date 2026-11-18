#!/bin/sh
A="$HOME/android-sdk/platform-tools/adb"
LOG=/tmp/sa-log.txt
"$A" logcat -d -s SimpleAgent 2>/dev/null > $LOG
echo "总行数: $(wc -l < $LOG)"
echo
echo "--- refreshTimeline 的 seq 范围（去重，看游标是否在推进）---"
grep -o 'seq=[0-9]*\.\.[0-9]*' $LOG | uniq | tail -10
echo
echo "--- items 与 earlierPart ---"
grep -o 'items=[0-9]* hasMore=[a-z]* seq=[0-9]*\.\.[0-9]* earlierPart=[0-9]*' $LOG | uniq | tail -10
echo
echo "--- 加载开始（前 3 / 后 3）---"
grep -F 'loadEarlierItems' $LOG | head -3
echo "..."
grep -F 'loadEarlierItems' $LOG | tail -3