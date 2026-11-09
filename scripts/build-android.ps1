# 构建 Android 用的 Go 内核（AAR）。
#
# 用法（在仓库根目录）：
#   pwsh scripts/build-android.ps1
#   pwsh scripts/build-android.ps1 -Api 24 -Target android/arm64
#
# 依赖：
#   - Go 1.27+
#   - gomobile / gobind：go install golang.org/x/mobile/cmd/gomobile golang.org/x/mobile/cmd/gobind
#   - Android SDK + NDK（ANDROID_HOME / ANDROID_NDK_HOME）
#
# 产物：android/app/libs/bridge.aar
param(
    [int]$Api = 24,
    # 386 是给 32 位 x86 模拟器用的：少了它，x86 模拟器上会以
    # "dlopen failed: library libgojni.so not found" 直接崩在启动阶段。
    [string]$Target = "android/arm64,android/arm,android/amd64,android/386"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    if (-not $env:ANDROID_HOME) {
        if (Test-Path "F:\Android\Sdk") { $env:ANDROID_HOME = "F:\Android\Sdk" }
        else { throw "没有设置 ANDROID_HOME" }
    }
    if (-not $env:ANDROID_NDK_HOME) {
        $ndk = Get-ChildItem "$env:ANDROID_HOME\ndk" -ErrorAction SilentlyContinue |
            Sort-Object Name -Descending | Select-Object -First 1
        if (-not $ndk) { throw "没有找到 NDK（$env:ANDROID_HOME\ndk）" }
        $env:ANDROID_NDK_HOME = $ndk.FullName
    }
    $gomobile = Join-Path (go env GOPATH) "bin\gomobile.exe"
    if (-not (Test-Path $gomobile)) {
        throw "找不到 gomobile：go install golang.org/x/mobile/cmd/gomobile@latest"
    }

    # gomobile 的收尾步骤要调 javac 生成 Java 绑定，必须把 JDK 放进 PATH。
    if (-not $env:JAVA_HOME) {
        if (Test-Path "F:\Android\Studio\jbr") { $env:JAVA_HOME = "F:\Android\Studio\jbr" }
    }
    if (-not $env:JAVA_HOME) { throw "没有设置 JAVA_HOME（gomobile 需要 javac）" }
    $env:PATH = (Join-Path $env:JAVA_HOME "bin") + ";" + (Join-Path (go env GOPATH) "bin") + ";" + $env:PATH

    $outDir = Join-Path $root "android\app\libs"
    New-Item -ItemType Directory -Force -Path $outDir | Out-Null
    $aar = Join-Path $outDir "bridge.aar"

    Write-Host "== gomobile bind ($Target, api $Api) ==" -ForegroundColor Cyan
    # 先写临时文件再替换：gomobile 会先清空 -o 指定的文件再写入，直接指向
    # 目标文件的话，构建中途失败会留下一个 0 字节的 AAR（APK 里就没有
    # libgojni.so，装到手机上直接崩在启动阶段）。
    # 临时文件名必须以 .aar 结尾（gomobile 会校验扩展名）。
    $temp = Join-Path $outDir "bridge-build.aar"
    Remove-Item $temp -ErrorAction SilentlyContinue
    $output = & $gomobile bind "-target=$Target" "-androidapi=$Api" -o $temp ./mobile/bridge 2>&1
    $code = $LASTEXITCODE
    if ($output) { $output | ForEach-Object { Write-Host "  $_" } }
    if ($code -ne 0) {
        Remove-Item $temp -ErrorAction SilentlyContinue
        throw "gomobile bind 失败（退出码 $code）"
    }
    if (-not (Test-Path $temp) -or (Get-Item $temp).Length -eq 0) {
        throw "gomobile bind 没有产出有效 AAR"
    }
    Move-Item -Force $temp $aar

    $size = [math]::Round((Get-Item $aar).Length / 1MB, 1)
    Write-Host "完成：$aar（$size MB）" -ForegroundColor Green
}
finally {
    Pop-Location
}
