#!/usr/bin/env sh
# 构建脚本（Linux / macOS）：生成 bin/simple-agent
#
# 用法：
#   scripts/build.sh              # 版本号自动取 git describe
#   VERSION=1.2.3 scripts/build.sh
#   OUT=dist scripts/build.sh
#   STRIP=0 scripts/build.sh      # 保留调试信息
set -eu
cd "$(dirname "$0")/.."

version="${VERSION:-}"
if [ -z "$version" ]; then
    version="dev"
    if command -v git >/dev/null 2>&1; then
        described="$(git describe --tags --always --dirty 2>/dev/null || true)"
        [ -n "$described" ] && version="$described"
    fi
fi

out_dir="${OUT:-bin}"
mkdir -p "$out_dir"
target="$out_dir/simple-agent"

ldflags="-X main.version=$version"
if [ "${STRIP:-1}" = "1" ]; then
    ldflags="-s -w $ldflags"
fi

echo "构建 simple-agent $version → $target"
go build -trimpath -ldflags "$ldflags" -o "$target" ./cmd/simple-agent
echo "完成：$target（$(du -h "$target" | cut -f1)）"
echo "运行：./$target --help"
