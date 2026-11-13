#!/bin/sh
# 在 macOS 上一键构建并安装 Android 客户端。
#
# 用法（在仓库根目录）：
#   ./scripts/build-android-mac.sh              # 拉代码 + 建 AAR + 建 APK + 装到手机
#   ./scripts/build-android-mac.sh --no-pull    # 不拉代码（改的是本地未提交代码）
#   ./scripts/build-android-mac.sh --no-aar     # 跳过 AAR（只改了 Kotlin 时快很多）
#   ./scripts/build-android-mac.sh --no-install # 只构建，不安装
#
# 依赖（首次配置见 docs/android-dev-mac.md）：
#   JDK 17        —— 路径记在 ~/.jdk-path
#   Go 1.27+      —— ~/go-sdk/go/bin/go 或 PATH 里的 go
#   gomobile      —— ~/go/bin/gomobile
#   Android SDK   —— ANDROID_HOME（默认 ~/android-sdk），需 platform-tools/platforms/build-tools
#   Android NDK   —— ANDROID_HOME/ndk/<version>
#
# 踩过的坑（都在这里处理了）：
#   1. git checkout/pull 会把 android/gradlew 的执行位还原掉 → 每次 chmod +x
#   2. gomobile 会先清空 -o 指定的文件，直接写目标文件时中途失败会留下 0 字节
#      AAR（APK 里没有 libgojni.so，装上去直接崩）→ 先写临时文件再替换
#   3. gomobile 收尾要调 javac，JDK 必须在 PATH 上
#   4. 临时文件必须以 .aar 结尾（gomobile 校验扩展名）

set -e

PULL=1
BUILD_AAR=1
INSTALL=1
for arg in "$@"; do
    case "$arg" in
    --no-pull) PULL=0 ;;
    --no-aar) BUILD_AAR=0 ;;
    --no-install) INSTALL=0 ;;
    -h | --help)
        sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'
        exit 0
        ;;
    *)
        echo "未知参数：$arg（用 --help 查看用法）" >&2
        exit 2
        ;;
    esac
done

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

# ---- 环境 ----

if [ -z "$JAVA_HOME" ]; then
    if [ -f "$HOME/.jdk-path" ]; then
        JAVA_HOME=$(cat "$HOME/.jdk-path")
    elif [ -d "/Library/Java/JavaVirtualMachines" ]; then
        JAVA_HOME=$(/usr/libexec/java_home -v 17 2>/dev/null || true)
    fi
fi
[ -n "$JAVA_HOME" ] && [ -x "$JAVA_HOME/bin/javac" ] || {
    echo "找不到 JDK 17：设置 JAVA_HOME，或把路径写进 ~/.jdk-path" >&2
    exit 1
}

# Go：优先用单独装的 SDK（mini 上系统 go 太旧）
if [ -x "$HOME/go-sdk/go/bin/go" ]; then
    GO="$HOME/go-sdk/go/bin/go"
    PATH="$HOME/go-sdk/go/bin:$PATH"
elif command -v go >/dev/null 2>&1; then
    GO=go
else
    echo "找不到 go" >&2
    exit 1
fi
export GOTOOLCHAIN=auto # 项目要 1.27+，让 Go 自己下载

GOMOBILE="$HOME/go/bin/gomobile"
[ -x "$GOMOBILE" ] || GOMOBILE=$(command -v gomobile || true)
[ -n "$GOMOBILE" ] || {
    echo "找不到 gomobile：$GO install golang.org/x/mobile/cmd/gomobile" >&2
    exit 1
}

export JAVA_HOME
PATH="$JAVA_HOME/bin:$HOME/go/bin:$PATH"

if [ -z "$ANDROID_HOME" ]; then
    ANDROID_HOME="$HOME/android-sdk"
fi
[ -d "$ANDROID_HOME" ] || {
    echo "找不到 Android SDK：$ANDROID_HOME" >&2
    exit 1
}
export ANDROID_HOME
export ANDROID_SDK_ROOT="$ANDROID_HOME"

if [ -z "$ANDROID_NDK_HOME" ]; then
    ANDROID_NDK_HOME=$(ls -d "$ANDROID_HOME"/ndk/* 2>/dev/null | sort -V | tail -1)
fi
[ -n "$ANDROID_NDK_HOME" ] && [ -d "$ANDROID_NDK_HOME" ] || {
    echo "找不到 NDK：$ANDROID_HOME/ndk（用 sdkmanager 安装）" >&2
    exit 1
}
export ANDROID_NDK_HOME

ADB="$ANDROID_HOME/platform-tools/adb"

step() { printf '\n\033[36m== %s ==\033[0m\n' "$1"; }

# ---- 1. 拉代码 ----
if [ "$PULL" = 1 ]; then
    step "拉取最新代码"
    git pull --ff-only
fi

# gradlew 的执行位常被 git 还原掉，每次都要补
chmod +x android/gradlew

# ---- 2. 构建 AAR（Go 内核）----
if [ "$BUILD_AAR" = 1 ]; then
    step "构建 AAR（gomobile bind）"
    mkdir -p android/app/libs
    TMP_AAR=android/app/libs/bridge-build.aar
    rm -f "$TMP_AAR"
    # 必须先写临时文件：见文件头注释的坑 2
    if ! CGO_ENABLED=0 "$GOMOBILE" bind \
        -target=android/arm64,android/arm,android/amd64,android/386 \
        -androidapi=24 -o "$TMP_AAR" ./mobile/bridge; then
        rm -f "$TMP_AAR"
        echo "gomobile bind 失败" >&2
        exit 1
    fi
    if [ ! -s "$TMP_AAR" ]; then
        echo "gomobile bind 没有产出有效 AAR" >&2
        exit 1
    fi
    mv -f "$TMP_AAR" android/app/libs/bridge.aar
    echo "AAR 完成"
else
    step "跳过 AAR（--no-aar）"
    [ -f android/app/libs/bridge.aar ] || {
        echo "android/app/libs/bridge.aar 不存在，不能跳过 AAR" >&2
        exit 1
    }
fi

# ---- 3. 构建 APK ----
step "构建 APK"
(
    cd android
    ./gradlew :app:assembleDebug --console=plain
)
APK="android/app/build/outputs/apk/debug/app-debug.apk"
[ -f "$APK" ] || {
    echo "APK 没有生成：$APK" >&2
    exit 1
}
# Gradle 的 outputs 目录偶尔会被后续任务清掉，先复制一份到稳定位置再装
STABLE_APK=build/app-debug.apk
mkdir -p build
cp -f "$APK" "$STABLE_APK"
echo "APK：$STABLE_APK（$(du -h "$STABLE_APK" | cut -f1)）"

# ---- 4. 安装 ----
if [ "$INSTALL" = 1 ]; then
    step "安装到设备"
    if ! "$ADB" devices | tail -n +2 | grep -q "device$"; then
        echo "没有连接的设备（adb devices 为空），跳过安装" >&2
        exit 1
    fi
    # -r 覆盖安装并保留数据。签名不同才会失败——那时需要先 uninstall，
    # 但那会清空 App 里保存的连接参数，所以不自动做。
    if ! "$ADB" install -r "$STABLE_APK"; then
        echo "" >&2
        echo "安装失败。若提示签名不一致（INSTALL_FAILED_UPDATE_INCOMPATIBLE），" >&2
        echo "需要用同一份 debug keystore 构建，或手动 adb uninstall（会清空 App 数据）" >&2
        exit 1
    fi
    echo "已安装"
fi

step "完成"
