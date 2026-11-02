# 本地 Dendrite 开发身份（假设 dendrite.exe 已在 8008 上应答）
#
# 做四件事：建 3 个账号 → 登录拿 token → 建房间 #dev 把三个人拉进去 → 写 actors.json。
#
#   pwsh scripts/dev-dendrite.ps1
#
# 三个账号故意跨 principal：acme 的 planner + coder，bob 的 reviewer。
# 这样 e2e 才能同时覆盖「同 principal」和「跨 principal → trust=peer」。

param(
    [string]$Homeserver = "http://127.0.0.1:8008",
    [string]$ServerName = "localhost",
    [string]$DataDir = ".dev/matrix",
    [string]$DendriteConfig = ".dev/dendrite/dendrite.yaml"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root

$cfg = (Resolve-Path $DendriteConfig).Path
$accounts = @(
    @{ name = "planner";  local = "teamcp_acme_a_planner";  pass = "dev-password-1" },
    @{ name = "coder";    local = "teamcp_acme_a_coder";     pass = "dev-password-2" },
    @{ name = "reviewer"; local = "teamcp_bob_a_reviewer";   pass = "dev-password-3" }
)

# 1) 建账号。-url 是 client API 的地址；"-user already exists" 不算失败。
foreach ($a in $accounts) {
    $out = & .\.dev\bin\create-account.exe --config $cfg -url $Homeserver `
        -username $a.local -password $a.pass 2>&1 | Out-String
    if ($out -match "already exists") {
        "已存在  @$($a.local):$ServerName"
    } elseif ($out -match "(?i)error|fatal|panic") {
        throw "建账号失败 @$($a.local)：$($out.Trim())"
    } else {
        "已创建  @$($a.local):$ServerName"
    }
}

# 2) 登录
function Login($local, $pass) {
    $body = @{
        type       = "m.login.password"
        identifier = @{ type = "m.id.user"; user = $local }
        password   = $pass
    } | ConvertTo-Json -Compress
    $r = Invoke-RestMethod -Uri "$Homeserver/_matrix/client/v3/login" -Method Post `
        -Body $body -ContentType "application/json" -TimeoutSec 20
    return $r.access_token
}

$tokens = @{}
foreach ($a in $accounts) {
    $tokens[$a.name] = Login $a.local $a.pass
    $who = Invoke-RestMethod -Uri "$Homeserver/_matrix/client/v3/account/whoami" `
        -Headers @{ Authorization = "Bearer $($tokens[$a.name])" }
    "$($a.name.PadRight(9)) -> $($who.user_id)"
}

# 3) 建房间并拉人
# room_alias_name 让 homeserver 写入 m.room.canonical_alias。
# 只调 /directory/room 发布别名是不够的：那样目录里有条目，但房间的 canonical_alias
# 仍是空的，于是 Teamcp 的别名解析找不到它，只能拿 !id 寻址。
$create = @{
    name            = "发布流程"
    topic           = "发布与联调。把 acme 的发布流程教给新加入的 AI。"
    preset          = "private_chat"
    room_alias_name = "dev"
    invite = @("@teamcp_acme_a_coder:$ServerName", "@teamcp_bob_a_reviewer:$ServerName")
} | ConvertTo-Json -Compress

$room = (Invoke-RestMethod -Uri "$Homeserver/_matrix/client/v3/createRoom" -Method Post `
    -Body $create -ContentType "application/json" `
    -Headers @{ Authorization = "Bearer $($tokens['planner'])" }).room_id
"房间       $room"

foreach ($who in @("coder", "reviewer")) {
    try {
        Invoke-RestMethod -Uri "$Homeserver/_matrix/client/v3/join/$([uri]::EscapeDataString($room))" `
            -Method Post -Body "{}" -ContentType "application/json" `
            -Headers @{ Authorization = "Bearer $($tokens[$who])" } | Out-Null
        "$who 已加入"
    } catch { "$who 加入失败：$($_.Exception.Message)" }
}

try {
    Invoke-RestMethod -Uri "$Homeserver/_matrix/client/v3/directory/room/$([uri]::EscapeDataString('#dev:' + $ServerName))" `
        -Method Put -Body (@{ room_id = $room } | ConvertTo-Json -Compress) -ContentType "application/json" `
        -Headers @{ Authorization = "Bearer $($tokens['planner'])" } | Out-Null
    "别名       #dev:$ServerName"
} catch { "别名设置失败（可用 room id 寻址）：$($_.Exception.Message)" }

# 4) actors.json —— 唯一持 token 的地方，0600
New-Item -ItemType Directory -Force $DataDir | Out-Null
$actors = foreach ($a in $accounts) {
    @{
        name         = $a.name
        homeserver   = $Homeserver
        user_id      = "@$($a.local):$ServerName"
        access_token = $tokens[$a.name]
    }
}
@{ default = "planner"; actors = @($actors) } | ConvertTo-Json -Depth 5 |
    Set-Content (Join-Path $DataDir "actors.json") -Encoding utf8
"身份配置   $DataDir/actors.json"

""
"下一步： go run ./cmd/teamcp -data-dir $DataDir"
