param(
    [string]$Version = "dev"
)

$ErrorActionPreference = "Stop"

$Root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$Dist = Join-Path $Root "dist"
$Ldflags = "-s -w -X main.version=$Version"

$Targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Name = "sai-native-windows-amd64.exe" },
    @{ GOOS = "linux"; GOARCH = "amd64"; Name = "sai-native-linux-amd64" },
    @{ GOOS = "darwin"; GOARCH = "arm64"; Name = "sai-native-darwin-arm64" }
)

function Ensure-WindowsConvenienceLink {
    $Target = Join-Path $Dist "sai-native-windows-amd64.exe"
    $Link = Join-Path $Dist "sai-native.exe"
    if (-not (Test-Path -LiteralPath $Target -PathType Leaf)) {
        return
    }
    # A symlink needs Developer Mode; a refreshed copy is always correct and
    # can never go stale after a rebuild.
    Copy-Item -LiteralPath $Target -Destination $Link -Force
    Write-Host "copied dist/sai-native.exe <- sai-native-windows-amd64.exe"
}

$OldCGOEnabled = $env:CGO_ENABLED
$OldGOOS = $env:GOOS
$OldGOARCH = $env:GOARCH

New-Item -ItemType Directory -Force -Path $Dist | Out-Null

Push-Location $Root
try {
    foreach ($Target in $Targets) {
        $env:CGO_ENABLED = "0"
        $env:GOOS = $Target.GOOS
        $env:GOARCH = $Target.GOARCH
        go build -trimpath -ldflags $Ldflags -o (Join-Path $Dist $Target.Name) ./cmd/sai-native
        Write-Host "built dist/$($Target.Name)"
    }
} finally {
    $env:CGO_ENABLED = $OldCGOEnabled
    $env:GOOS = $OldGOOS
    $env:GOARCH = $OldGOARCH
    Pop-Location
}

Ensure-WindowsConvenienceLink
