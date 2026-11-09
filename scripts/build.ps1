# 构建脚本：生成 bin\simple-agent.exe
#
# 用法（在仓库任意位置执行都可以）：
#   pwsh scripts/build.ps1                 # 版本号自动取 git describe
#   pwsh scripts/build.ps1 -Version 1.2.3  # 手动指定版本号
#   pwsh scripts/build.ps1 -Output dist    # 输出到别的目录
#   pwsh scripts/build.ps1 -NoStrip        # 保留调试信息（排查崩溃用）
param(
    [string]$Version = "",
    [string]$Output = "bin",
    [switch]$NoStrip
)

$ErrorActionPreference = "Stop"
# 中文输出在部分控制台代码页下会乱码，统一按 UTF-8 输出
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    # 版本号：优先参数，其次 git（tag 优先，退化为提交号，带 -dirty 标记）
    if (-not $Version) {
        $Version = "dev"
        if (Get-Command git -ErrorAction SilentlyContinue) {
            $described = git describe --tags --always --dirty 2>$null
            if ($LASTEXITCODE -eq 0 -and $described) {
                $Version = $described.Trim()
            }
        }
    }

    $outDir = Join-Path $root $Output
    New-Item -ItemType Directory -Force -Path $outDir | Out-Null
    $exe = Join-Path $outDir "simple-agent.exe"

    # -trimpath：不把本机绝对路径写进二进制
    # -s -w：去掉符号表与调试信息（体积约减半；要排查崩溃时用 -NoStrip）
    $ldflags = "-X main.version=$Version"
    if (-not $NoStrip) {
        $ldflags = "-s -w $ldflags"
    }

    Write-Host "构建 simple-agent $Version → $exe" -ForegroundColor Cyan
    go build -trimpath -ldflags $ldflags -o $exe ./cmd/simple-agent
    if ($LASTEXITCODE -ne 0) { throw "构建失败" }

    $size = [math]::Round((Get-Item $exe).Length / 1MB, 1)
    Write-Host "完成：$exe（$size MB）" -ForegroundColor Green
    Write-Host "运行：& '$exe' --help"
}
finally {
    Pop-Location
}
