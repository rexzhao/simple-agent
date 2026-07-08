param(
    [string]$Version = "dev"
)

$ErrorActionPreference = "Stop"

$Root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$Dist = Join-Path $Root "dist"
$Ldflags = "-s -w -X github.com/rexzhao/simple-agent/internal/cli.Version=$Version"

$Targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Name = "sai-windows-amd64.exe" },
    @{ GOOS = "linux"; GOARCH = "amd64"; Name = "sai-linux-amd64" },
    @{ GOOS = "darwin"; GOARCH = "arm64"; Name = "sai-darwin-arm64" }
)

function Ensure-WindowsConvenienceLink {
    $Target = Join-Path $Dist "sai-windows-amd64.exe"
    $Link = Join-Path $Dist "sai.exe"
    if (-not (Test-Path -LiteralPath $Target -PathType Leaf)) {
        return
    }
    if ($null -ne (Get-Item -LiteralPath $Link -Force -ErrorAction SilentlyContinue)) {
        return
    }
    New-Item -ItemType SymbolicLink -Path $Link -Target "sai-windows-amd64.exe" | Out-Null
    Write-Host "linked dist/sai.exe -> sai-windows-amd64.exe"
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

        $Output = Join-Path $Dist $Target.Name
        go build -trimpath -ldflags $Ldflags -o $Output ./cmd/sai
        Write-Host "built dist/$($Target.Name)"
    }
    Ensure-WindowsConvenienceLink
} finally {
    Pop-Location

    if ($null -eq $OldCGOEnabled) {
        Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue
    } else {
        $env:CGO_ENABLED = $OldCGOEnabled
    }

    if ($null -eq $OldGOOS) {
        Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
    } else {
        $env:GOOS = $OldGOOS
    }

    if ($null -eq $OldGOARCH) {
        Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
    } else {
        $env:GOARCH = $OldGOARCH
    }
}
