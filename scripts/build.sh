#!/usr/bin/env sh
set -eu

SCRIPT_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd "$SCRIPT_DIR/.." && pwd)
DIST_DIR="$ROOT_DIR/dist"
VERSION="${VERSION:-dev}"
LDFLAGS="-s -w -X main.version=$VERSION"

build() {
    target_os="$1"
    target_arch="$2"
    output_name="$3"

    CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
        go build -trimpath -ldflags "$LDFLAGS" -o "$DIST_DIR/$output_name" ./cmd/sai-native
    echo "built dist/$output_name"
}

ensure_host_convenience_link() {
    case "$(uname -s)" in
        Linux*)
            target_name="sai-native-linux-amd64"
            link_name="sai-native"
            if [ -f "$DIST_DIR/$target_name" ]; then
                ln -sf "$target_name" "$DIST_DIR/$link_name"
                echo "linked dist/$link_name -> $target_name"
            fi
            ;;
        Darwin*)
            target_name="sai-native-darwin-arm64"
            link_name="sai-native"
            if [ -f "$DIST_DIR/$target_name" ]; then
                ln -sf "$target_name" "$DIST_DIR/$link_name"
                echo "linked dist/$link_name -> $target_name"
            fi
            ;;
        MINGW*|MSYS*|CYGWIN*)
            # Git Bash ln -s silently copies, and a stale copy is worse than
            # none: refresh a real copy on every build instead.
            target_name="sai-native-windows-amd64.exe"
            link_name="sai-native.exe"
            if [ -f "$DIST_DIR/$target_name" ]; then
                cp -f "$DIST_DIR/$target_name" "$DIST_DIR/$link_name"
                echo "copied dist/$link_name <- $target_name"
            fi
            ;;
    esac
}

mkdir -p "$DIST_DIR"

build windows amd64 sai-native-windows-amd64.exe
build linux amd64 sai-native-linux-amd64
build darwin arm64 sai-native-darwin-arm64

ensure_host_convenience_link
