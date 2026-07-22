#!/usr/bin/env sh
set -eu

SCRIPT_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd "$SCRIPT_DIR/.." && pwd)
DIST_DIR="$ROOT_DIR/dist"
WEB_DIR="$ROOT_DIR/web"
VERSION="${VERSION:-dev}"
LDFLAGS="-s -w -X github.com/rexzhao/simple-agent/internal/webapp.Version=$VERSION"

build() {
    target_os="$1"
    target_arch="$2"
    output_name="$3"

    CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
        go build -trimpath -ldflags "$LDFLAGS" -o "$DIST_DIR/$output_name" ./cmd/sai
    echo "built dist/$output_name"
}

ensure_host_convenience_link() {
    case "$(uname -s)" in
        Linux*)
            target_name="sai-linux-amd64"
            link_name="sai"
            ;;
        Darwin*)
            target_name="sai-darwin-arm64"
            link_name="sai"
            ;;
        MINGW*|MSYS*|CYGWIN*)
            target_name="sai-windows-amd64.exe"
            link_name="sai.exe"
            ;;
        *)
            return
            ;;
    esac

    target="$DIST_DIR/$target_name"
    link="$DIST_DIR/$link_name"
    if [ ! -f "$target" ]; then
        return
    fi
    if [ -e "$link" ] || [ -L "$link" ]; then
        return
    fi
    ln -s "$target_name" "$link"
    echo "linked dist/$link_name -> $target_name"
}

mkdir -p "$DIST_DIR"
cd "$WEB_DIR"
npm ci
npm run build
cd "$ROOT_DIR"

build windows amd64 sai-windows-amd64.exe
build linux amd64 sai-linux-amd64
build darwin arm64 sai-darwin-arm64
ensure_host_convenience_link
