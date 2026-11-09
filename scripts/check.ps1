# 本地检查：go vet、staticcheck、竞态测试、调度稳定性测试。
#
# 用法（在仓库根目录）：
#   pwsh scripts/check.ps1            # 全部跑一遍
#   pwsh scripts/check.ps1 -NoRace    # 跳过 -race（更快）
param(
    [switch]$NoRace
)

$ErrorActionPreference = "Stop"
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    Write-Host "== go vet ==" -ForegroundColor Cyan
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw "go vet 失败" }

    Write-Host "== staticcheck ==" -ForegroundColor Cyan
    go tool staticcheck ./...
    if ($LASTEXITCODE -ne 0) { throw "staticcheck 失败" }

    Write-Host "== go test ==" -ForegroundColor Cyan
    if ($NoRace) {
        go test ./...
    } else {
        go test -race ./...
    }
    if ($LASTEXITCODE -ne 0) { throw "测试失败" }

    # 界面测试是异步驱动的（开流在 goroutine 里）：必须在不同 GOMAXPROCS 下
    # 反复通过，才能说明它不依赖 goroutine 的调度先后。单核最容易暴露问题。
    Write-Host "== ui 调度稳定性（-cpu=1,4 -count=3） ==" -ForegroundColor Cyan
    go test "-cpu=1,4" -count=3 ./internal/uiapp/
    if ($LASTEXITCODE -ne 0) { throw "界面测试在 -cpu=1,4 下不稳定" }

    Write-Host "全部通过" -ForegroundColor Green
}
finally {
    Pop-Location
}
