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
        go build -trimpath -ldflags "$LDFLAGS" -o "$DIST_DIR/$output_name" ./cmd/sai
    echo "built dist/$output_name"
}

# The Windows build product is dist/sai.exe itself: never a symlink or a
# convenience copy, either of which can go stale after a rebuild.
mkdir -p "$DIST_DIR"

build windows amd64 sai.exe
build linux amd64 sai-linux-amd64
build darwin arm64 sai-darwin-arm64

# On a Unix host, link the matching build to dist/sai for convenience; the
# link is refreshed on every build, and rm -f clears a stale plain file.
case "$(uname -s)" in
    Linux*)
        if [ -f "$DIST_DIR/sai-linux-amd64" ]; then
            rm -f "$DIST_DIR/sai"
            ln -s sai-linux-amd64 "$DIST_DIR/sai"
            echo "linked dist/sai -> sai-linux-amd64"
        fi
        ;;
    Darwin*)
        if [ -f "$DIST_DIR/sai-darwin-arm64" ]; then
            rm -f "$DIST_DIR/sai"
            ln -s sai-darwin-arm64 "$DIST_DIR/sai"
            echo "linked dist/sai -> sai-darwin-arm64"
        fi
        ;;
esac
