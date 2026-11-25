# 执行端开发循环：反复调用 start-agent.ps1（编译 + 启动 + 等退出）。
#
# 用法（在仓库根目录）：
#   pwsh -File scripts/dev-agent.ps1
#   pwsh -File scripts/dev-agent.ps1 -Listen                 # 随机端口 + relay
#   pwsh -File scripts/dev-agent.ps1 -Listen -ListenAddr 0.0.0.0:7900
#   pwsh -File scripts/dev-agent.ps1 -NoRelay                # 只本地跑
#
# 更新二进制的办法：**杀掉当前的 simple-agent 进程**（任务管理器、
# `taskkill /IM simple-agent.exe /F`，或在这个窗口按 Ctrl+C 停脚本后重开）。
#
# # 为什么拆成两个脚本
#
# 这条循环用**新进程**调用 start-agent.ps1，所以**改了启动参数（relay 地址、
# --listen、附加参数）之后，只要杀掉执行端进程，下一轮就会按新参数拉起** ——
# 不需要重启这个循环窗口。
#
# 以前是一整个脚本：参数在循环开始时算一次，改完脚本必须 Ctrl+C 停下来
# 再重开，很容易忘（表现是"改了脚本但跑的还是旧参数"，本项目踩过多次）。

param(
    # 以下参数原样透传给 start-agent.ps1（含义见那个脚本的注释）
    [string]$Relay = "",
    [string]$Name = "",
    [switch]$NoRelay,
    [switch]$Listen,
    [string]$ListenAddr = "",
    # 两次重启之间的等待秒数
    [int]$RestartDelaySeconds = 5,
    # 传给执行端的额外参数
    [string[]]$ExtraArgs = @()
)

$ErrorActionPreference = "Stop"
# **子进程的中文输出要按 GBK 解码**。
#
# 不加这一行，子进程（simple-agent.exe）打的中文日志会变成乱码：
# 它输出 UTF-8 字节，而 Windows PowerShell 的默认控制台编码是 GBK，
# 两边对不上就成了"鏃犳硶鎵撳紑鏁版嵁鐩綍"这种。
#
# 只影响显示，不影响程序行为（日志内容本身是对的）。
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }
try { $OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

$child = Join-Path $PSScriptRoot "start-agent.ps1"
if (-not (Test-Path $child)) {
    Write-Host "找不到 $child" -ForegroundColor Red
    exit 1
}

Write-Host "执行端开发循环已启动（Ctrl+C 结束）" -ForegroundColor Cyan
Write-Host "  单次启动脚本：$child"
Write-Host "  >>> 想更新版本：杀掉 simple-agent 进程即可，循环会自动重编重拉 <<<"
Write-Host "  >>> 改了单次启动脚本：同样杀掉进程，下一轮就用新参数 <<<"

$round = 0
while ($true) {
    $round++
    Write-Host ""
    Write-Host "---- 第 $round 轮 ----" -ForegroundColor DarkCyan

    # 参数每次重新拼：这是"改脚本不用重启循环"的关键 —— 子进程读的是
    # 磁盘上的最新脚本，而不是这里的某个快照。
    $childArgs = @()
    if ($Relay) { $childArgs += @("-Relay", $Relay) }
    if ($Name) { $childArgs += @("-Name", $Name) }
    if ($NoRelay) { $childArgs += "-NoRelay" }
    if ($Listen) { $childArgs += "-Listen" }
    if ($ListenAddr) { $childArgs += @("-ListenAddr", $ListenAddr) }
    if ($ExtraArgs) { $childArgs += @("-ExtraArgs", ($ExtraArgs -join ",")) }

    $startedAt = Get-Date
    # **用 pwsh 起子进程**（而不是 . 点源）：点源会在当前作用域执行，
    # 脚本里的 $ErrorActionPreference / 函数定义会污染这个循环，
    # 而且改动不会重新读取。子进程每次都读最新文件。
    & pwsh -NoProfile -File $child @childArgs
    $childExit = $LASTEXITCODE
    $ran = (Get-Date) - $startedAt

    Write-Host ""
    switch ($childExit) {
        2 {
            Write-Host "编译失败（等 $RestartDelaySeconds 秒后重试；改好代码会自动重编）" -ForegroundColor Yellow
        }
        3 {
            Write-Host "启动失败，等 $RestartDelaySeconds 秒后重试" -ForegroundColor Yellow
        }
        0 {
            Write-Host "执行端已退出（被杀死或被替换），运行了 $([int]$ran.TotalSeconds) 秒" -ForegroundColor Green
        }
        default {
            Write-Host "执行端退出，退出码 $childExit，运行了 $([int]$ran.TotalSeconds) 秒" -ForegroundColor Yellow
        }
    }

    # 启动后立刻崩溃（< 3 秒）多等一会儿，避免疯狂重启刷屏
    $delay = $RestartDelaySeconds
    if ($ran.TotalSeconds -lt 3) {
        $delay = [Math]::Max($RestartDelaySeconds, 5)
        Write-Host "启动后很快退出，可能是配置/端口问题；等 $delay 秒" -ForegroundColor Yellow
    }
    Write-Host "等待 $delay 秒…" -ForegroundColor DarkGray
    Start-Sleep -Seconds $delay
}
