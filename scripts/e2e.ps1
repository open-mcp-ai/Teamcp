# Teamcp 真机 e2e
#
# 假定两件事：Dendrite 在 $Homeserver 应答，Teamcp 在 $Teamcp 应答。
# 前置：pwsh scripts/dev-dendrite.ps1，然后起 teamcp。
#
#   pwsh scripts/e2e.ps1
#
# 每条断言都用真实 Matrix 事件走完整链路：MCP 调用 → Teamcp → homeserver。
# 退出码非 0 表示有失败项。

param(
    [string]$Teamcp = "http://127.0.0.1:18080",
    [string]$DataDir = ".dev/matrix",
    [string]$UserHeader = "",       # 认证模式下必填，如 X-Teamcp-User: acme
    [string]$SecretHeader = ""
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root

$actors = Get-Content (Join-Path $DataDir "actors.json") -Raw | ConvertFrom-Json
$room = $null
$pass = 0; $fail = 0

function Team([hashtable]$args_, [string]$as_) {
    $a = @{ cmd = $args_.cmd }
    if ($args_.ContainsKey("params")) { $a.args = $args_.params }
    if ($as_) { $a.as = $as_ }
    if ($args_.ContainsKey("timeout")) { $a.timeout = $args_.timeout }
    $body = @{ jsonrpc = "2.0"; id = 1; method = "tools/call"; params = @{ name = "team"; arguments = $a } } |
        ConvertTo-Json -Depth 6 -Compress

    $headers = @{ Accept = "application/json, text/event-stream"; "MCP-Protocol-Version" = "2025-06-18" }
    if ($UserHeader) { $headers["X-Teamcp-User"] = $UserHeader }
    if ($SecretHeader) { $headers["X-Teamcp-Secret"] = $SecretHeader }

    $r = Invoke-WebRequest -Uri "$Teamcp/mcp" -Method Post -Body $body -ContentType "application/json" `
        -Headers $headers -UseBasicParsing -TimeoutSec 60
    $line = (($r.Content -split "`n") | Where-Object { $_ -match '^data: ' }) -replace '^data: ', ''
    return (($line | ConvertFrom-Json).result.content[0].text)
}

function Check([string]$name, [scriptblock]$test) {
    try {
        $ok = & $test
        if ($ok) { Write-Host "  ✅ $name"; $script:pass++ }
        else { Write-Host "  ❌ $name"; $script:fail++ }
    } catch {
        Write-Host "  ❌ $name —— $($_.Exception.Message)"
        $script:fail++
    }
}

Write-Host "`n== 身份与房间 =="

Check "whoami 报出真实 MXID" {
    (Team @{ cmd = "whoami" }) -match '@teamcp_\w+_[ah]_\w+:'
}

Check "ls rooms 至少有一个房间" {
    $out = Team @{ cmd = "ls rooms" }
    if ($out -match '(![A-Za-z0-9]+:[^ ]+)') { $script:room = $matches[1] }
    [bool]$script:room
}

if (-not $room) {
    Write-Host "`n没有可用房间，先建一个"
    $out = Team @{ cmd = "new room e2e-room --alias e2e-room --topic e2e" }
    if ($out -match '(![A-Za-z0-9]+:[^ ]+)') { $room = $matches[1] }
}
Write-Host "  room = $room"

Write-Host "`n== 消息往复 =="

$marker = "e2e-marker-" + [guid]::NewGuid().ToString("N").Substring(0, 8)

Check "send 成功并返回 event id" {
    $out = Team @{ cmd = "send $room --kind ask `$1"; params = @("$marker ; rm -rf /") }
    $out -match 'TEAM/1 200' -and $out -match 'Event: \$'
}

Check "正文原样到达（分号没有变成第二条语句）" {
    $out = Team @{ cmd = "read $room" }
    ($out -match [regex]::Escape($marker)) -and ($out -match [regex]::Escape('; rm -rf /'))
}

Check "read 渲染成 teamcp-message 块并带 trust" {
    (Team @{ cmd = "read $room" }) -match '<teamcp-message[^>]*kind="ask"[^>]*trust="(self|peer)"'
}

Check "wait 空等到期是 204 + Timed-Out" {
    $out = Team @{ cmd = "wait $room"; timeout = 1 }
    $out -match 'TEAM/1 204' -and $out -match 'Timed-Out: true'
}

Check "catchup 给出简报" {
    (Team @{ cmd = "catchup $room" }) -match '本窗 \d+ 条消息'
}

Write-Host "`n== 产物 =="
# slug 每次唯一：e2e 必须可重复运行，写死 slug 的话第二次跑就撞上上一版的版本号。
$slug = "e2e-" + $marker.Substring($marker.Length - 8)

Check "put art 写入 v1" {
    (Team @{ cmd = "put art $slug --kind handover --room $room `$1"; params = @('## 1. 目标`n验收标准：staging 绿') }) -match 'Version: 1'
}

Check "put art 第二次自增到 v2" {
    (Team @{ cmd = "put art $slug --kind handover --room $room `$1"; params = @("第二版") }) -match 'Version: 2'
}

Check "get art 读回最新一版正文" {
    # 第二次 put 覆盖了正文，所以这里断言的是"最新版"，不是"第一版"。
    # 第一次写的"staging"已经不在当前 state 里了——state event 是单值的。
    $out = Team @{ cmd = "get art $slug --room $room" }
    $out -match '第二版' -and $out -match '"version":2'
}

Check "ls artifacts 列出产物" {
    (Team @{ cmd = "ls artifacts --room $room" }) -match $slug
}

Write-Host "`n== 安全边界 =="

Check "跨 principal 的 command 被拒（403）" {
    $out = Team @{ cmd = "send $room --kind command --to reviewer `$1"; params = @("删库") }
    $out -match 'TEAM/1 403' -and $out -match 'E_COMMAND_CROSS_PRINCIPAL'
}

Check "同 principal 的 command 放行" {
    (Team @{ cmd = "send $room --kind command --to coder `$1"; params = @("跑测试") }) -match 'TEAM/1 200'
}

Check "超长正文被 413 拒绝" {
    $big = "x" * (64 * 1024 + 1)
    (Team @{ cmd = "send $room --kind tell `$1"; params = @($big) }) -match 'E_TOO_LARGE'
}

Check "未定义的变量在解析期就被拒" {
    (Team @{ cmd = "send $room --kind tell `$9" }) -match 'E_BAD_DSL'
}

Write-Host "`n== 协议表面 =="

Check "tools/list 只有一个工具" {
    $body = @{ jsonrpc = "2.0"; id = 1; method = "tools/list"; params = @{} } | ConvertTo-Json -Compress
    $headers = @{ Accept = "application/json, text/event-stream"; "MCP-Protocol-Version" = "2025-06-18"; "Mcp-Method" = "tools/list" }
    if ($UserHeader) { $headers["X-Teamcp-User"] = $UserHeader }
    if ($SecretHeader) { $headers["X-Teamcp-Secret"] = $SecretHeader }
    $r = Invoke-WebRequest -Uri "$Teamcp/mcp" -Method Post -Body $body -ContentType "application/json" `
        -Headers $headers -UseBasicParsing -TimeoutSec 20
    $line = (($r.Content -split "`n") | Where-Object { $_ -match '^data: ' }) -replace '^data: ', ''
    $tools = ($line | ConvertFrom-Json).result.tools
    ($tools.Count -eq 1) -and ($tools[0].name -eq "team")
}

Check "help 里没有未实现标记" {
    $out = Team @{ cmd = "help" }
    ($out -match '高层') -and ($out -match '原生') -and ($out -notmatch '·')
}

Write-Host "`n$pass 通过, $fail 失败"
if ($fail -gt 0) { exit 1 }
exit 0
