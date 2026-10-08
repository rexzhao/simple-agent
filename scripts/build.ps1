param(
    [string]$Version = "dev"
)

$ErrorActionPreference = "Stop"

$Root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$Dist = Join-Path $Root "dist"
$Ldflags = "-s -w -X main.version=$Version"

$Targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Name = "sai-windows-amd64.exe" },
    @{ GOOS = "linux"; GOARCH = "amd64"; Name = "sai-linux-amd64" },
    @{ GOOS = "darwin"; GOARCH = "arm64"; Name = "sai-darwin-arm64" }
)

# dist/sai.exe links to the Windows build, refreshed on every build. A
# symbolic link needs Developer Mode or elevation; a hard link needs neither
# (same volume, same file data, no extra disk); a copy is the last resort.
function Ensure-WindowsConvenienceLink {
    $Target = "sai-windows-amd64.exe"
    $Link = Join-Path $Dist "sai.exe"
    if (-not (Test-Path -LiteralPath (Join-Path $Dist $Target) -PathType Leaf)) {
        return
    }
    Remove-Item -LiteralPath $Link -Force -ErrorAction SilentlyContinue
    try {
        New-Item -ItemType SymbolicLink -Path $Link -Target $Target | Out-Null
        Write-Host "linked dist/sai.exe -> $Target (symbolic)"
        return
    } catch {}
    try {
        New-Item -ItemType HardLink -Path $Link -Target (Join-Path $Dist $Target) | Out-Null
        Write-Host "linked dist/sai.exe -> $Target (hard link)"
        return
    } catch {}
    Copy-Item -LiteralPath (Join-Path $Dist $Target) -Destination $Link -Force
    Write-Host "copied dist/sai.exe <- $Target"
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
        go build -trimpath -ldflags $Ldflags -o (Join-Path $Dist $Target.Name) ./cmd/sai
        Write-Host "built dist/$($Target.Name)"
    }
} finally {
    $env:CGO_ENABLED = $OldCGOEnabled
    $env:GOOS = $OldGOOS
    $env:GOARCH = $OldGOARCH
    Pop-Location
}

Ensure-WindowsConvenienceLink
