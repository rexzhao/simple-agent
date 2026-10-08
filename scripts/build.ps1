param(
    [string]$Version = "dev"
)

$ErrorActionPreference = "Stop"

$Root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$Dist = Join-Path $Root "dist"
$Ldflags = "-s -w -X main.version=$Version"

$Targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Name = "sai.exe" },
    @{ GOOS = "linux"; GOARCH = "amd64"; Name = "sai-linux-amd64" },
    @{ GOOS = "darwin"; GOARCH = "arm64"; Name = "sai-darwin-arm64" }
)

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
        go build -trimpath -ldflags $Ldflags -o (Join-Path $Dist $Target.Name) ./cmd/sai
        Write-Host "built dist/$($Target.Name)"
    }
} finally {
    $env:CGO_ENABLED = $OldCGOEnabled
    $env:GOOS = $OldGOOS
    $env:GOARCH = $OldGOARCH
    Pop-Location
}
