# 执行端开发循环：构建 → 启动 → 等待退出 → 等 5 秒 → 再来一遍。
#
# 用法（在仓库根目录）：
#   pwsh -File scripts/dev-agent.ps1
#   pwsh -File scripts/dev-agent.ps1 -Relay 129.211.171.158:7900
#   pwsh -File scripts/dev-agent.ps1 -NoRelay      # 只本地跑，不注册 relay
#
# 更新二进制的办法：**杀掉当前的 simple-agent 进程**（任务管理器、
# `taskkill /IM simple-agent.exe /F`，或在这个窗口按 Ctrl+C 停脚本后重开）。
# 脚本发现进程退出，等 5 秒后重新编译并拉起新版本。
#
# 为什么需要它：每次改完代码都要「停进程 → 编译 → 再启动」，手动做很容易
# 忘掉其中一步 —— 出现"改了代码但跑的还是旧二进制"（本项目已经踩过：
# HANDOFF 里记的 AAR 过期是同类问题）。
#
# relay 参数从数据目录的 relay.json 读（GUI 里 /relay 命令保存的那份），
# 所以 token 不写死在这份脚本里，也不会进 git。

param(
    # relay 地址；留空表示从 relay.json 读（没读过就只本地跑）
    [string]$Relay = "",
    # 执行端在 relay 上的名字；留空用机器名
    [string]$Name = "",
    # 只本地跑，不注册 relay
    [switch]$NoRelay,
    # 两次重启之间的等待秒数
    [int]$RestartDelaySeconds = 5,
    # 传给执行端的额外参数
    [string[]]$ExtraArgs = @()
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    $exe = Join-Path $root "bin\simple-agent.exe"
    $dataDir = Join-Path $env:APPDATA "simple-agent"

    # ---- relay 参数 ----
    $relayArgs = @()
    if (-not $NoRelay) {
        $addr = $Relay
        $token = ""
        $fingerprint = ""
        $statePath = Join-Path $dataDir "relay.json"
        if (Test-Path $statePath) {
            try {
                $state = Get-Content $statePath -Raw | ConvertFrom-Json
                if (-not $addr) { $addr = [string]$state.addr }
                $token = [string]$state.token
                $fingerprint = [string]$state.fingerprint
            } catch {
                Write-Host "relay.json 解析失败：$($_.Exception.Message)" -ForegroundColor Yellow
            }
        }
        if ($addr) {
            # **不加 --headless**：要的是正常的图形执行端（有窗口、能看会话）。
            # --relay 与 --headless 是两件独立的事：前者把执行端注册到 relay，
            # 后者只是"不建窗口"。加 --headless 会跑成一个没有界面的进程，
            # 界面上看不到也操作不了。
            $relayArgs += @("--relay", $addr)
            if ($token) { $relayArgs += @("--token", $token) }
            if ($fingerprint) { $relayArgs += @("--tls-fingerprint", $fingerprint) }
            if ($Name) { $relayArgs += @("--name", $Name) }
        } else {
            Write-Host "没有 relay 地址（也没有 relay.json）：只本地运行" -ForegroundColor Yellow
        }
    }
    $allArgs = $relayArgs + $ExtraArgs

    function Invoke-Build {
        Write-Host ""
        Write-Host "== 编译 ==" -ForegroundColor Cyan
        $env:CGO_ENABLED = "0"
        $output = & go build -o $exe ./cmd/simple-agent 2>&1
        if ($LASTEXITCODE -ne 0) {
            Write-Host "编译失败：" -ForegroundColor Red
            $output | ForEach-Object { Write-Host "  $_" }
            return $false
        }
        $info = Get-Item $exe
        Write-Host ("编译完成：{0:N1} MB  {1:HH:mm:ss}" -f ($info.Length / 1MB), $info.LastWriteTime) -ForegroundColor Green
        return $true
    }

    Write-Host "执行端开发循环已启动（Ctrl+C 结束）" -ForegroundColor Cyan
    Write-Host "  二进制：$exe"
    Write-Host "  参数：$($allArgs -join ' ')"
    Write-Host "  >>> 想更新版本：杀掉 simple-agent 进程即可，脚本会自动重编重拉 <<<"

    $round = 0
    while ($true) {
        $round++
        Write-Host ""
        Write-Host "---- 第 $round 轮 ----" -ForegroundColor DarkCyan

        if (-not (Invoke-Build)) {
            # 编译失败时等待，避免刷屏；用户改好代码杀进程即可触发重试
            Write-Host "等待 $RestartDelaySeconds 秒后重试…" -ForegroundColor Yellow
            Start-Sleep -Seconds $RestartDelaySeconds
            continue
        }

        # 启动。**不要用 Start-Process -Wait 配重定向**：那样看不到实时输出。
        # 这里用 & 直接跑（等它退出），输出直接进当前控制台。
        $startedAt = Get-Date
        try {
            & $exe @allArgs
        } catch {
            Write-Host "启动失败：$($_.Exception.Message)" -ForegroundColor Red
        }
        $exitCode = $LASTEXITCODE
        $ran = (Get-Date) - $startedAt

        Write-Host ""
        if ($null -eq $exitCode) {
            Write-Host "执行端已退出（被杀死或被替换）" -ForegroundColor Yellow
        } else {
            Write-Host "执行端退出，退出码 $exitCode，运行了 $([int]$ran.TotalSeconds) 秒" -ForegroundColor Yellow
        }

        # 如果它启动后立刻崩了（< 3 秒），多等一会儿，避免疯狂重启刷屏
        $delay = $RestartDelaySeconds
        if ($ran.TotalSeconds -lt 3) {
            $delay = [Math]::Max($RestartDelaySeconds, 5)
            Write-Host "启动后很快退出，可能是配置/端口问题；等 $delay 秒" -ForegroundColor Yellow
        }
        Write-Host "等待 $delay 秒…" -ForegroundColor DarkGray
        Start-Sleep -Seconds $delay
    }
}
finally {
    Pop-Location
}