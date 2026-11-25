# 启动执行端一次：编译 + 启动 + 等待它退出。
#
# 这个脚本**每次运行都重新算参数**，所以：
#   - 改了本文件的参数拼接逻辑 → 杀掉执行端进程，下一轮自动用新逻辑
#   - 改了 relay.json → 同上
#   - 改了 config.toml → 执行端自己会重读（它是新进程）
#
# 不要直接长期跑这个脚本，用 dev-agent.ps1 的循环包着它（那条循环用
# **新进程**调用本文件，因此本文件的改动无需重启循环即可生效）。
#
# 用法（在仓库根目录）：
#   pwsh -File scripts/start-agent.ps1
#   pwsh -File scripts/start-agent.ps1 -ListenAddr 0.0.0.0:7900
#   pwsh -File scripts/start-agent.ps1 -NoRelay                   # 只本地跑
#   pwsh -File scripts/start-agent.ps1 -TLS:$false                # 退回明文
#
# 默认：注册 relay + 局域网直连（随机端口）+ TLS（mTLS）。

param(
    # relay 地址；留空表示从 relay.json 读（没读过就只本地跑）
    [string]$Relay = "",
    # 执行端在 relay 上的名字；留空用机器名
    [string]$Name = "",
    # 只本地跑，不注册 relay
    [switch]$NoRelay,
    # 监听：局域网直连用。
    #
    # **默认开启**（随机端口，地址经 relay 上报给展示端）：同一 WiFi 下的
    # 手机/另一台电脑会优先直连（1~2ms），不在同一网络时自动回退 relay。
    # 想关掉用 -Listen:$false。
    [switch]$Listen = $true,
    # 与 -Listen 搭配的监听地址（不给 = 随机端口）
    [string]$ListenAddr = "",
    # 监听时是否启用 TLS（**默认开**）。
    #
    # 这是 mTLS（双向证书校验）的前提：没有 TLS 就没有证书，也就没有
    # "验证对端是谁"这回事。首次启动自动自签生成证书，不用手工配置。
    #
    # 想退回明文用 -TLS:$false —— 但注意加密与明文**严格隔离**：
    # 明文的执行端只能被明文的 viewer 连，反之亦然。
    [switch]$TLS = $true,
    # 传给执行端的额外参数
    [string[]]$ExtraArgs = @()
)

$ErrorActionPreference = "Stop"
# 中文输出按 UTF-8 解码（避免日志乱码，见 dev-agent.ps1 里的说明）
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }
try { $OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

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

    # ---- 局域网直连监听 ----
    #
    # 只在显式给了 -Listen 时才加 --listen（监听 = 对外开一个入口，
    # 不该在用户没要求时发生）。
    #
    #   -Listen                          随机端口（:0），地址经 relay 上报
    #   -Listen -ListenAddr 0.0.0.0:7900 指定地址和端口
    #
    # 同一 WiFi 下的手机会优先直连这个端口（1~2ms），
    # 不在同一网络时自动回退到 relay。
    $listenArgs = @()
    if ($Listen) {
        $bindAddr = $ListenAddr
        if ([string]::IsNullOrWhiteSpace($bindAddr)) {
            $bindAddr = ":0"
            Write-Host "局域网直连：随机端口（地址会经 relay 上报）" -ForegroundColor Cyan
        } else {
            Write-Host "局域网直连：监听 $bindAddr" -ForegroundColor Cyan
        }
        # **必须带地址**：PowerShell 的 Start-Process/-ArgumentList 与
        # cmd 风格的参数解析都会把"末尾那个没有值的 --listen"当成开关吃掉，
        # 结果程序收不到 --listen，静默地不监听（踩过）。
        $listenArgs += @("--listen", $bindAddr)
    }

    # **--tls：mTLS 的前提**（独立于 -Listen）。
    #
    # 没有它就没有证书，也就没有"验证对端是谁"这回事：
    #   - 展示端无法校验"这是不是执行端"
    #   - 执行端无法验证来访的 viewer 是谁
    # 也就是说整个指纹体系不生效。
    #
    # 首次启动自动自签生成证书（`<DataDir>/tls`），不用手工配置。
    #
    # 注意**加密与明文严格隔离**：开了 --tls 的执行端只接受出示证书的
    # viewer；没开 --tls 的 viewer 连不上它。这是刻意的 —— 否则攻击者
    # 只要让握手失败，就能把连接降级成无校验的明文。
    if ($TLS) {
        $listenArgs += "--tls"
        Write-Host "加密：已启用 TLS（首次会自签生成证书）" -ForegroundColor Cyan
    }

    $allArgs = $relayArgs + $listenArgs + $ExtraArgs

    # ---- 编译 ----
    Write-Host ""
    Write-Host "== 编译 ==" -ForegroundColor Cyan
    $env:CGO_ENABLED = "0"
    $output = & go build -o $exe ./cmd/simple-agent 2>&1
    if ($LASTEXITCODE -ne 0) {
        Write-Host "编译失败：" -ForegroundColor Red
        $output | ForEach-Object { Write-Host "  $_" }
        # 退出码 2 = 编译失败（循环脚本据此多等一会儿再试）
        exit 2
    }
    $info = Get-Item $exe
    Write-Host ("编译完成：{0:N1} MB  {1:HH:mm:ss}" -f ($info.Length / 1MB), $info.LastWriteTime) -ForegroundColor Green
    Write-Host "  参数：$($allArgs -join ' ')" -ForegroundColor DarkGray

    # ---- 启动 ----
    #
    # **不要用 Start-Process -Wait 配重定向**：那样看不到实时输出。
    # 这里用 & 直接跑（等它退出），输出直接进当前控制台。
    try {
        & $exe @allArgs
    } catch {
        Write-Host "启动失败：$($_.Exception.Message)" -ForegroundColor Red
        exit 3
    }
    $exitCode = $LASTEXITCODE
    if ($null -eq $exitCode) { $exitCode = 0 }
    exit $exitCode
}
finally {
    Pop-Location
}
