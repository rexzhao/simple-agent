#!/usr/bin/env sh
# 本地检查：go vet、staticcheck、竞态测试。
set -e
cd "$(dirname "$0")/.."
echo "== go vet =="            && go vet ./...
echo "== staticcheck =="       && go tool staticcheck ./...
echo "== go test -race =="     && go test -race ./...
# 界面测试是异步驱动的：不同 GOMAXPROCS 反复通过才说明不依赖调度顺序
echo "== ui 调度稳定性 =="     && go test -cpu=1,4 -count=3 ./internal/uiapp/
echo "全部通过"
