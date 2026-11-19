#!/usr/bin/env pwsh
# 用真实请求体直接打网关两次，看它给多少 cached_tokens。
#
# 目的：把"我们这边"和"网关"分开。请求体是我们实际发出的那一份
# （从 <data_dir>/requests/ 取的），中间不经过任何我们的代码，
# 所以拿到的 cached_tokens 就是网关的原始态度。
param(
    [string]$RequestFile = "",
    [string]$Base = "https://tc-paperhub.diezhi.net/v1"
)

$ErrorActionPreference = "Stop"
$key = $env:PAPERHUB_API_KEY
if (-not $key) { throw "缺少 PAPERHUB_API_KEY" }

$dir = Join-Path $env:APPDATA "simple-agent\requests"
if (-not $RequestFile) {
    # 取最小的一份（7MB 那个），太大的等太久
    $f = Get-ChildItem $dir -Filter "*.json" |
        Sort-Object Length |
        Where-Object { $_.Length -gt 1MB } |
        Select-Object -First 1
    if (-not $f) { throw "找不到可用的请求体" }
    $RequestFile = $f.FullName
}
Write-Host "请求体: $(Split-Path $RequestFile -Leaf)  ($([Math]::Round((Get-Item $RequestFile).Length/1MB,1)) MB)"

# 原样发送：这是关键——不改任何字节，网关看到的就是我们发的那份
$body = [IO.File]::ReadAllBytes($RequestFile)
$headers = @{ Authorization = "Bearer $key"; "Content-Type" = "application/json" }

function Send-Once {
    param([int]$N)
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $resp = Invoke-WebRequest -Uri "$Base/chat/completions" -Method Post `
        -Headers $headers -Body $body -TimeoutSec 300
    $sw.Stop()

    # 流式响应里 usage 在最后一块，抓出来
    $text = $resp.Content
    $matches2 = [regex]::Matches($text, '"usage":\{[^}]*"prompt_tokens":(\d+)[^}]*"cached_tokens":(\d+)')
    if ($matches2.Count -eq 0) {
        # 兜底：usage 字段顺序可能不同，单独找两个数
        $pt = [regex]::Match($text, '"prompt_tokens":(\d+)')
        $ct = [regex]::Match($text, '"cached_tokens":(\d+)')
        if ($pt.Success -and $ct.Success) {
            $p = [int]$pt.Groups[1].Value; $c = [int]$ct.Groups[1].Value
        } else { Write-Host "第 $N 次：响应里没有 usage"; return }
    } else {
        $p = [int]$matches2[0].Groups[1].Value
        $c = [int]$matches2[0].Groups[2].Value
    }
    $pct = if ($p -gt 0) { [Math]::Round($c * 100 / $p, 1) } else { 0 }
    Write-Host ("第 {0} 次：prompt={1}  cached={2}  ({3}%)  耗时 {4:N1}s" -f $N, $p, $c, $pct, $sw.Elapsed.TotalSeconds)
}

Write-Host "`n=== 同一份请求体连发 3 次 ==="
Write-Host "第 1 次是建缓存（应接近 0），第 2/3 次才看命中。`n"
for ($i = 1; $i -le 3; $i++) {
    try { Send-Once -N $i } catch { Write-Host "第 $i 次失败：$($_.Exception.Message)" }
    if ($i -lt 3) { Start-Sleep -Seconds 3 }
}