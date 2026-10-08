<#
.SYNOPSIS
    绕开「git 经 shell 调凭据助手失败」问题的推送脚本。

.DESCRIPTION
    背景（详见 .workbuddy/memory/2026-10-08.md）：
    在 WorkBuddy 托管的环境里，`git push` 到 HTTPS 远程会**静默失败**——
    直接返回 exit 128，stdout/stderr 全空。

    trace2 证据：
        child_start  use_shell:true, argv:["git credential-store get"]
        child_exit   code:5, t_rel:6.84      <- 助手经 shell spawn 失败
        exit         transport-helper.c:1280, code:128

    即 git 通过 shell 去调凭据助手，而本机 shell 被 shim 污染
    （shell-runtime-bash-env.sh 报 dirname/cd not found），助手取不到凭据。

    对策：凭据内联进 URL + 显式清空助手链（-c credential.helper=），
    这样 git 不再 spawn shell，推送恢复正常。

.PARAMETER Remote
    要推送到的 git remote 名，默认 mine。

.PARAMETER Branch
    要推送的本地分支，默认 main。

.PARAMETER Message
    提交信息；给了就先 git add -A + commit 再推送。

.PARAMETER DryRun
    只做预演（--dry-run），不真正推送。

.EXAMPLE
    powershell -NoProfile -ExecutionPolicy Bypass -File scripts\push-github.ps1
    powershell -NoProfile -ExecutionPolicy Bypass -File scripts\push-github.ps1 -Message "fix: 修个东西"
#>
[CmdletBinding()]
param(
    [string]$Remote = 'mine',
    [string]$Branch = 'main',
    [string]$Message,
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'

# 本机 PowerShell 5.1 默认按 ANSI/GBK 解码原生命令输出，
# 会让 git 吐出的中文路径/提交信息变成乱码。必须显式设为 UTF-8。
try {
    [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
    $OutputEncoding = New-Object System.Text.UTF8Encoding($false)
} catch { }

function Fail([string]$msg) {
    Write-Host "[X] $msg" -ForegroundColor Red
    exit 1
}

# --- 定位仓库根 ---
# 优先用脚本自身位置（本脚本固定位于 <repo>\scripts\），
# 避免解析 git rev-parse 输出的非 ASCII 路径。
$repoRoot = Split-Path $PSScriptRoot -Parent
if (-not (Test-Path (Join-Path $repoRoot '.git'))) {
    $repoRoot = (& git rev-parse --show-toplevel 2>$null)
    if (-not $repoRoot) { Fail '当前目录不在 git 仓库里。' }
}
Set-Location $repoRoot
Write-Host "[*] 仓库: $repoRoot" -ForegroundColor Cyan

# --- 可选：先提交 ---
if ($Message) {
    Write-Host "[*] 提交改动: $Message" -ForegroundColor Cyan
    & git add -A
    if ($LASTEXITCODE -ne 0) { Fail 'git add 失败。' }
    $staged = @(& git diff --cached --name-only 2>$null)
    if ($staged.Count -gt 0) {
        & git commit -m $Message | Out-Null
        if ($LASTEXITCODE -ne 0) { Fail 'git commit 失败。' }
        Write-Host "[+] 已提交 $($staged.Count) 个文件。" -ForegroundColor Green
    } else {
        Write-Host "[*] 没有需要提交的改动，跳过提交。" -ForegroundColor Yellow
    }
}

# --- 取远程 URL ---
$remoteUrl = (& git remote get-url $Remote 2>$null)
if (-not $remoteUrl) {
    $allRemotes = ((& git remote) -join ', ')
    Fail "找不到 remote '$Remote'（可用: $allRemotes）"
}
if ($remoteUrl -notmatch '^https?://') {
    Fail "remote '$Remote' 不是 HTTP(S) 地址（$remoteUrl）。本脚本只处理 HTTPS 推送。"
}
Write-Host "[*] 目标: $Remote -> $remoteUrl" -ForegroundColor Cyan

# --- 校验工作区 ---
$dirty = (& git status --porcelain 2>$null)
if (-not $Message -and $dirty) {
    Write-Host "[!] 工作区有未提交改动，本次只推送已提交内容。" -ForegroundColor Yellow
}

# --- 从 ~/.git-credentials 取凭据 ---
$credPath = Join-Path $env:USERPROFILE '.git-credentials'
if (-not (Test-Path $credPath)) { Fail "找不到凭据文件 $credPath" }

$hostName = ([uri]$remoteUrl).Host
$line = (Get-Content $credPath -Encoding UTF8 |
         Where-Object { $_ -match [regex]::Escape($hostName) } |
         Select-Object -First 1)
if (-not $line) { Fail "凭据文件里没有 $hostName 的记录。（先 git config credential.helper store 并登录一次）" }

$auth = (($line -replace '^[a-z]+://', '') -replace "@$([regex]::Escape($hostName)).*$", '')
$parts = $auth -split ':', 2
if ($parts.Count -lt 2) { Fail '凭据格式异常，期望 user:token@host。' }
$user = $parts[0]
$token = $parts[1]
Write-Host "[*] 凭据: $user@$hostName （token 已读取，不回显）" -ForegroundColor Cyan

# --- 构造带凭据的 URL（只在本进程内存在，不落盘）---
$bareUrl = $remoteUrl -replace '^https?://', ''
$pushUrl = "https://${user}:${token}@${bareUrl}"

# --- 推送 ---
$gitArgs = @('-c', 'credential.helper=', 'push')
if ($DryRun) { $gitArgs += '--dry-run' }
$gitArgs += @($pushUrl, "${Branch}:${Branch}")

Write-Host "[*] 执行: git -c credential.helper= push <url> ${Branch}:${Branch}" -ForegroundColor Cyan
if ($DryRun) { Write-Host "    (dry-run，不会真正推送)" -ForegroundColor Yellow }

$outFile = Join-Path $env:TEMP ("pushout-" + [guid]::NewGuid().ToString('N') + ".txt")
# 原生命令会把正常进度也写到 stderr（例如 "Everything up-to-date"），
# 在 $ErrorActionPreference='Stop' 下会被当成终止错误，必须临时放宽。
$prevEap = $ErrorActionPreference
$ErrorActionPreference = 'Continue'
& git @gitArgs *> $outFile
$code = $LASTEXITCODE
$ErrorActionPreference = $prevEap
if (Test-Path $outFile) {
    # PowerShell 5.1 会把原生命令的 stderr 包装成 ErrorRecord，
    # 用 *> 落盘后会带上一堆错误格式噪声，这里过滤掉，只留 git 原文。
    $noise = 'CategoryInfo|FullyQualifiedErrorId|NativeCommandError|RemoteException|所在位置|^\s*\+'
    Get-Content $outFile -Encoding UTF8 |
        Where-Object { $_ -and ($_ -notmatch $noise) } |
        ForEach-Object { Write-Host ("    " + ($_ -replace '^(git|powershell)\.exe\s*:\s*', '')) }
    Remove-Item $outFile -Force -ErrorAction SilentlyContinue
}

if ($code -ne 0) { Fail "推送失败（exit $code）。" }

# --- 补上分支跟踪（不含凭据）---
& git config "branch.$Branch.remote" $Remote
& git config "branch.$Branch.merge" "refs/heads/$Branch"

$localHead = (& git rev-parse HEAD)
$remoteHead = ((& git ls-remote $Remote "refs/heads/$Branch" 2>$null) -split '\s+')[0]
Write-Host ''
if ($localHead -eq $remoteHead) {
    Write-Host "[+] 推送完成，本地与 $Remote/$Branch 一致: $($localHead.Substring(0,7))" -ForegroundColor Green
} else {
    Write-Host "[!] 推送结束但两端不一致: local=$($localHead.Substring(0,7)) remote=$remoteHead" -ForegroundColor Yellow
}
