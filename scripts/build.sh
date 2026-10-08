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

# The Windows build product is dist/sai-windows-amd64.exe; dist/sai.exe is
# a symbolic link to it, refreshed on every build. (On a Git Bash host
# without native symlink support the link degrades to a refreshed copy.)
mkdir -p "$DIST_DIR"

build windows amd64 sai-windows-amd64.exe
build linux amd64 sai-linux-amd64
build darwin arm64 sai-darwin-arm64

# On the host platform, link the matching build to dist/sai for convenience;
# the link is refreshed every build, and rm -f clears a stale plain file.
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
    MINGW*|MSYS*|CYGWIN*)
        if [ -f "$DIST_DIR/sai-windows-amd64.exe" ]; then
            rm -f "$DIST_DIR/sai.exe"
            if MSYS=winsymlinks:nativestrict ln -s sai-windows-amd64.exe "$DIST_DIR/sai.exe" 2>/dev/null; then
                echo "linked dist/sai.exe -> sai-windows-amd64.exe (symbolic)"
            elif ln "$DIST_DIR/sai-windows-amd64.exe" "$DIST_DIR/sai.exe" 2>/dev/null; then
                # No symlink privilege on this host: a hard link shares the
                # same file data, needs no elevation, and costs no extra disk.
                echo "linked dist/sai.exe -> sai-windows-amd64.exe (hard link)"
            else
                cp -f "$DIST_DIR/sai-windows-amd64.exe" "$DIST_DIR/sai.exe"
                echo "copied dist/sai.exe <- sai-windows-amd64.exe"
            fi
        fi
        ;;
esac
