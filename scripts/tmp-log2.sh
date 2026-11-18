#!/bin/sh
A="$HOME/android-sdk/platform-tools/adb"
LOG=/tmp/sa2.txt
"$A" logcat -d -s SimpleAgent 2>/dev/null > $LOG

echo "--- loadEarlierItems 相关 ---"
grep -F "loadEarlierItems" $LOG | tail -8

echo
echo "--- refreshTimeline 精简序列 ---"
grep -F "refreshTimeline" $LOG | sed 's/.*refreshTimeline：//' | uniq | tail -10