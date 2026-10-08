<#
  pool-transfer.ps1 - export / import the local account pool.

  WHY THIS SCRIPT EXISTS
    Moving the pool by hand is easy to get wrong: people copy auths\ and forget
    data\state.json, which silently resets cooldowns, credit cache and failure
    counters. This script packs exactly the right set, every time.

  WHAT TRAVELS
    auths\*.json    account credentials (accessToken + refreshToken)  -> REQUIRED
    data\*.json     pool state + usage stats (credits, cooldowns)     -> strongly recommended
    config.json     gateway api_key + schedule settings               -> optional
    MIGRATE.txt     the guide shipped in scripts\, copied into the zip

  SECURITY
    The package contains PLAINTEXT tokens. Move it offline (USB stick, private
    drive). Never commit it, never upload it to a public share.

  ENCODING NOTE
    This file is deliberately ASCII-only and hard-codes no path: the project
    folder is non-ASCII, so every path is derived at runtime from $PSScriptRoot.
#>
[CmdletBinding()]
param(
    [ValidateSet('export', 'import')]
    [string]$Mode = 'export',

    # export: output zip path (default backups\pool-<timestamp>.zip)
    # import: input zip path  (default: newest zip in backups\)
    [string]$Package = '',

    # skip the "service is running" abort
    [switch]$Force
)

$ErrorActionPreference = 'Stop'
# Compress-Archive / Expand-Archive draw a progress bar on every file, which
# floods a console window with hundreds of lines for a 120 KB payload.
$ProgressPreference = 'SilentlyContinue'

# <root>\scripts  ->  <root>
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$backupDir = Join-Path $root 'backups'

# Compress-Archive on Windows PowerShell 5.1 stores entry names with BACKSLASH
# separators, which violates the zip spec: unzip on Linux/macOS then creates
# files literally named "auths\workbuddy-xxx.json". Writing the archive through
# System.IO.Compression lets us force forward slashes.
function New-ZipFromDirectory {
    param(
        [Parameter(Mandatory)][string]$SourceDir,
        [Parameter(Mandatory)][string]$Destination
    )

    Add-Type -AssemblyName System.IO.Compression | Out-Null
    Add-Type -AssemblyName System.IO.Compression.FileSystem | Out-Null

    if (Test-Path $Destination) { Remove-Item $Destination -Force }

    $fs = [System.IO.File]::Open($Destination, [System.IO.FileMode]::CreateNew)
    try {
        $zip = New-Object System.IO.Compression.ZipArchive($fs, [System.IO.Compression.ZipArchiveMode]::Create)
        try {
            $base = $SourceDir.TrimEnd('\', '/') + [System.IO.Path]::DirectorySeparatorChar
            foreach ($f in (Get-ChildItem -Path $SourceDir -Recurse -File)) {
                $rel = $f.FullName.Substring($base.Length) -replace '\\', '/'
                $entry = $zip.CreateEntry($rel, [System.IO.Compression.CompressionLevel]::Optimal)
                $es = $entry.Open()
                try {
                    $in = [System.IO.File]::OpenRead($f.FullName)
                    try { $in.CopyTo($es) } finally { $in.Dispose() }
                } finally { $es.Dispose() }
            }
        } finally { $zip.Dispose() }
    } finally { $fs.Dispose() }
}

function Get-ServiceRunning {
    [bool](Get-Process -Name 'wb2api' -ErrorAction SilentlyContinue)
}

function Get-PoolSummary {
    $authDir = Join-Path $root 'auths'
    if (-not (Test-Path $authDir)) { return $null }

    $files = @(Get-ChildItem -Path $authDir -Filter '*.json' -File -ErrorAction SilentlyContinue)
    if ($files.Count -eq 0) { return $null }

    $now = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
    $ok = 0; $expired = 0; $noRefresh = 0
    $realms = @{}

    foreach ($f in $files) {
        # -Encoding UTF8 is mandatory: Windows PowerShell 5.1 defaults to the
        # ANSI codepage, so a UTF-8 nickname containing an emoji gets decoded as
        # GBK. The trailing byte can then swallow the closing quote and the JSON
        # fails to parse even though the file on disk is perfectly valid.
        try { $j = Get-Content -Raw -Encoding UTF8 -Path $f.FullName | ConvertFrom-Json }
        catch {
            Write-Output "[WARN] unreadable credential file: $($f.Name)"
            continue
        }
        $a = $j.auth
        if ($null -eq $a) { continue }
        if ([int64]$a.expiresAt -gt $now) { $ok++ } else { $expired++ }
        if ([string]::IsNullOrEmpty($a.refreshToken)) { $noRefresh++ }
        $r = if ([string]::IsNullOrEmpty($a.realm)) { '(none)' } else { $a.realm }
        if ($realms.ContainsKey($r)) { $realms[$r] = $realms[$r] + 1 } else { $realms[$r] = 1 }
    }

    [pscustomobject]@{
        Total     = $files.Count
        Valid     = $ok
        Expired   = $expired
        NoRefresh = $noRefresh
        Realms    = $realms
    }
}

function Write-Header([string]$text) {
    Write-Output ''
    Write-Output ('=' * 64)
    Write-Output "  $text"
    Write-Output ('=' * 64)
}

# ---------------------------------------------------------------- export
function Invoke-Export {
    Write-Header 'EXPORT ACCOUNT POOL'

    if (-not (Test-Path (Join-Path $root 'auths'))) {
        throw "auths\ not found under $root - run this from the project folder."
    }

    $sum = Get-PoolSummary
    if ($null -eq $sum) {
        throw 'auths\ holds no account file - nothing to export.'
    }

    if (Get-ServiceRunning) {
        Write-Output '[WARN] wb2api.exe is running.'
        Write-Output '       Files are written atomically so the copy stays valid, but the'
        Write-Output '       package may miss the very latest cooldown / credit updates.'
        Write-Output '       Stopping the service first is cleaner:  taskkill /IM wb2api.exe /F'
    }

    # ---- staging: copy the payload, add the guide, then zip the contents
    $stage = Join-Path $env:TEMP ('wb2api-pool-' + [Guid]::NewGuid().ToString('N').Substring(0, 8))
    New-Item -ItemType Directory -Path $stage -Force | Out-Null

    $packed = @()
    foreach ($name in @('auths', 'data')) {
        $src = Join-Path $root $name
        if (Test-Path $src) {
            Copy-Item -Path $src -Destination (Join-Path $stage $name) -Recurse -Force
            $packed += $name
        }
    }
    foreach ($name in @('config.json')) {
        $src = Join-Path $root $name
        if (Test-Path $src) {
            Copy-Item -Path $src -Destination (Join-Path $stage $name) -Force
            $packed += $name
        }
    }

    # The guide lives in scripts\ so it survives a fresh git clone: backups\ is
    # gitignored, so a copy kept there would be lost on the next machine. The
    # old location is still honoured for backwards compatibility.
    $guide = @(
        (Join-Path $PSScriptRoot 'MIGRATE.txt'),
        (Join-Path $backupDir  'MIGRATE.txt')
    ) | Where-Object { Test-Path $_ } | Select-Object -First 1
    if ($guide) {
        Copy-Item -Path $guide -Destination (Join-Path $stage 'MIGRATE.txt') -Force
        $packed += 'MIGRATE.txt'
    }

    if (-not (Test-Path $backupDir)) { New-Item -ItemType Directory -Path $backupDir -Force | Out-Null }

    $out = if ($Package) {
        if ([System.IO.Path]::IsPathRooted($Package)) { $Package } else { Join-Path $root $Package }
    } else {
        Join-Path $backupDir ('pool-' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '.zip')
    }

    $outDir = Split-Path -Parent $out
    if ($outDir -and -not (Test-Path $outDir)) { New-Item -ItemType Directory -Path $outDir -Force | Out-Null }
    if (Test-Path $out) { Remove-Item $out -Force }

    Write-Output '[1/3] packing ...'
    New-ZipFromDirectory -SourceDir $stage -Destination $out
    Remove-Item -Path $stage -Recurse -Force

    # ---- verify by reading the archive back
    Write-Output '[2/3] verifying ...'
    Add-Type -AssemblyName System.IO.Compression.FileSystem | Out-Null
    $zip = [System.IO.Compression.ZipFile]::OpenRead($out)
    try {
        $entries = @($zip.Entries)
        $authInZip = @($entries | Where-Object { $_.FullName -match '^auths/.+\.json$' }).Count
        $dataInZip = @($entries | Where-Object { $_.FullName -match '^data/.+$' -and $_.Length -gt 0 }).Count
        $hasConfig = [bool](@($entries | Where-Object { $_.FullName -eq 'config.json' }).Count)
        $backslashes = @($entries | Where-Object { $_.FullName -like '*\*' }).Count
    } finally {
        $zip.Dispose()
    }

    if ($authInZip -ne $sum.Total) {
        throw "verification failed: auths in zip = $authInZip, expected $($sum.Total)"
    }
    if ($backslashes -gt 0) {
        throw "verification failed: $backslashes entry name(s) still use backslash separators"
    }

    $size = [math]::Round((Get-Item $out).Length / 1KB, 1)

    Write-Output '[3/3] done'
    Write-Output ''
    Write-Output "  package : $out"
    Write-Output "  size    : $size KB"
    Write-Output "  packed  : $($packed -join ', ')"
    Write-Output ''
    Write-Output "  accounts        : $($sum.Total)   (auths in zip: $authInZip, data files: $dataInZip, config.json: $hasConfig)"
    Write-Output "  token valid     : $($sum.Valid)"
    Write-Output "  token expired   : $($sum.Expired)   (refreshToken can renew them)"
    Write-Output "  missing refresh : $($sum.NoRefresh)"
    $realmText = ($sum.Realms.GetEnumerator() | Sort-Object Name | ForEach-Object { "$($_.Key)=$($_.Value)" }) -join '  '
    Write-Output "  realms          : $realmText"
    Write-Output ''
    Write-Output '  NEXT: copy this .zip to the new machine, put it in the project root,'
    Write-Output '        then run  import-pool.bat  there.'
    Write-Output ''
    Write-Output '  IMPORTANT: the zip holds PLAINTEXT tokens.'
    Write-Output '             Move it offline. Never commit it. Never share it publicly.'
    Write-Output ''
}

# ---------------------------------------------------------------- import
function Invoke-Import {
    Write-Header 'IMPORT ACCOUNT POOL'

    if (-not $Package) {
        if (-not (Test-Path $backupDir)) {
            throw "no package given and no backups\ folder under $root"
        }
        $newest = Get-ChildItem -Path $backupDir -Filter '*.zip' -File -ErrorAction SilentlyContinue |
                  Sort-Object LastWriteTime -Descending | Select-Object -First 1
        if ($null -eq $newest) {
            throw 'no .zip found in backups\ - pass the package path explicitly.'
        }
        $Package = $newest.FullName
    }

    $zip = if ([System.IO.Path]::IsPathRooted($Package)) { $Package } else { Join-Path $root $Package }
    if (-not (Test-Path $zip)) { throw "package not found: $zip" }

    Write-Output "  package : $zip"

    Add-Type -AssemblyName System.IO.Compression.FileSystem | Out-Null
    $probe = [System.IO.Compression.ZipFile]::OpenRead($zip)
    try {
        $entries = @($probe.Entries)
        # Accept both separators: packages made by Explorer or older PowerShell
        # may carry backslash entry names.
        $authCount = @($entries | Where-Object { $_.FullName -match '^auths[\\/].+\.json$' }).Count
        $topLevel = @($entries |
                     ForEach-Object { ($_.FullName -replace '\\', '/').Split('/')[0] } |
                     Sort-Object -Unique) -join ', '
    } finally {
        $probe.Dispose()
    }

    Write-Output "  accounts in package : $authCount"
    Write-Output "  top-level entries   : $topLevel"
    Write-Output ''

    if ($authCount -eq 0) {
        throw 'package contains no auths\*.json - refusing to import.'
    }

    # NOTE: parentheses are required. Without them PowerShell parses
    # "Get-ServiceRunning -and -not $Force" as a function call with -and / -not
    # as parameters, so -Force would never bypass the guard.
    if ((Get-ServiceRunning) -and (-not $Force)) {
        Write-Output '[STOP] wb2api.exe is running. It would keep the old pool in memory and'
        Write-Output '       may overwrite the imported files with stale state.'
        Write-Output ''
        Write-Output '       Stop it first, then run this again:'
        Write-Output '           taskkill /IM wb2api.exe /F'
        Write-Output ''
        Write-Output '       (or re-run with -Force to import anyway)'
        throw 'aborted: service is running.'
    }

    # ---- safety net: keep whatever is already here
    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    $prior = Join-Path $backupDir ("pre-import-$stamp")
    $saved = @()
    foreach ($name in @('auths', 'data', 'config.json')) {
        $src = Join-Path $root $name
        if (Test-Path $src) {
            if (-not (Test-Path $prior)) { New-Item -ItemType Directory -Path $prior -Force | Out-Null }
            Copy-Item -Path $src -Destination (Join-Path $prior $name) -Recurse -Force
            $saved += $name
        }
    }
    if ($saved.Count -gt 0) {
        Write-Output "[1/3] existing data saved to: $prior"
        Write-Output "      ($($saved -join ', '))"
    } else {
        Write-Output '[1/3] nothing to back up - clean target'
    }

    Write-Output '[2/3] extracting ...'
    Expand-Archive -Path $zip -DestinationPath $root -Force

    Write-Output '[3/3] done'
    $sum = Get-PoolSummary
    if ($null -ne $sum) {
        Write-Output ''
        Write-Output "  accounts        : $($sum.Total)"
        Write-Output "  token valid     : $($sum.Valid)"
        Write-Output "  token expired   : $($sum.Expired)"
        Write-Output "  missing refresh : $($sum.NoRefresh)"
    }
    Write-Output ''
    Write-Output '  NEXT: run start.bat, then open http://127.0.0.1:7863/panel/'
    Write-Output '        Keep the OLD machine stopped - two machines sharing the same'
    Write-Output '        refreshToken will kick each other out of the session.'
    Write-Output ''
}

switch ($Mode) {
    'export' { Invoke-Export }
    'import' { Invoke-Import }
}
