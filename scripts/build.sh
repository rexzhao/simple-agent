#!/usr/bin/env sh
set -eu

SCRIPT_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd "$SCRIPT_DIR/.." && pwd)
DIST_DIR="$ROOT_DIR/dist"
VERSION="${VERSION:-dev}"
LDFLAGS="-s -w -X github.com/rexzhao/simple-agent/internal/cli.Version=$VERSION"

build() {
    target_os="$1"
    target_arch="$2"
    output_name="$3"

    CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
        go build -trimpath -ldflags "$LDFLAGS" -o "$DIST_DIR/$output_name" ./cmd/sai
    echo "built dist/$output_name"
}

mkdir -p "$DIST_DIR"
cd "$ROOT_DIR"

build windows amd64 sai-windows-amd64.exe
build linux amd64 sai-linux-amd64
build darwin arm64 sai-darwin-arm64
