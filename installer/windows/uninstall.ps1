# WorkBuddy Gateway - uninstaller
#
# Removes the program, its shortcuts, the optional logon task and the Add/Remove
# Programs entry. The data directory (credentials, config, cache, stats) is kept by
# default and only deleted on an explicit confirmation, because it holds real
# credentials.
#
# Also reachable from Settings > Apps > "WorkBuddy Gateway" (the UninstallString in
# the registry points here).
#
# ASCII only: Windows PowerShell 5.1 reads .ps1 as ANSI unless the file has a UTF-8
# BOM, which would garble any non-ASCII text.

[CmdletBinding()]
param(
    # Answer "no" to every question (used by the quiet uninstall entry).
    [switch]$Silent,

    # Delete the data directory without asking. Destructive: it contains credentials.
    [switch]$RemoveData
)

$ErrorActionPreference = "Stop"

$installDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$dataDir = Join-Path $env:LOCALAPPDATA "wb-gateway"
$taskName = "WorkBuddyGateway"

Write-Host ""
Write-Host "  WorkBuddy Gateway - Uninstall"
Write-Host "  ============================"
Write-Host ""

# ---------------------------------------------------------------------------
# Stop the running gateway (only processes from *this* install dir)
# ---------------------------------------------------------------------------
foreach ($name in @("workbuddy-gateway.exe", "wb-local-agent.exe")) {
    $procs = Get-CimInstance Win32_Process -Filter "Name='$name'" -ErrorAction SilentlyContinue
    foreach ($p in $procs) {
        $exe = $p.ExecutablePath
        if ($exe -and ($exe -like "$installDir*")) {
            Write-Host "  stopping $name (PID $($p.ProcessId))"
            Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
        }
    }
}
Start-Sleep -Milliseconds 800

# ---------------------------------------------------------------------------
# Logon task
# ---------------------------------------------------------------------------
if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
    Write-Host "  logon task removed"
}

# ---------------------------------------------------------------------------
# Shortcuts
# ---------------------------------------------------------------------------
$links = @(
    (Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs\WorkBuddy Gateway.lnk"),
    (Join-Path ([Environment]::GetFolderPath("Desktop")) "WorkBuddy Gateway.lnk")
)
foreach ($l in $links) {
    if (Test-Path $l) {
        Remove-Item -Force $l
        Write-Host "  removed shortcut: $(Split-Path -Leaf $l)"
    }
}

# ---------------------------------------------------------------------------
# Add/Remove Programs entry
# ---------------------------------------------------------------------------
$regPath = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\WorkBuddyGateway"
if (Test-Path $regPath) {
    Remove-Item -Recurse -Force $regPath
    Write-Host "  uninstall entry removed"
}

# ---------------------------------------------------------------------------
# Data directory (kept by default: it holds credentials)
# ---------------------------------------------------------------------------
$dropData = $false
if ($RemoveData) {
    $dropData = $true
} elseif (-not $Silent -and (Test-Path $dataDir)) {
    Write-Host ""
    Write-Host "  Data directory: $dataDir"
    Write-Host "  It contains your credentials (workbuddy*.json), configuration, model"
    Write-Host "  cache and usage statistics."
    Write-Host ""
    $answer = Read-Host "  Delete it as well? This cannot be undone. [y/N]"
    if ($answer -match "^(y|yes)$") { $dropData = $true }
}

if ($dropData -and (Test-Path $dataDir)) {
    Remove-Item -Recurse -Force $dataDir
    Write-Host "  data directory deleted"
} elseif (Test-Path $dataDir) {
    Write-Host "  data directory kept: $dataDir"
}

# ---------------------------------------------------------------------------
# Program files
# ---------------------------------------------------------------------------
# The uninstaller itself lives inside $installDir and cannot delete the directory it
# is running from, so hand the removal to a detached cmd that waits a moment first.
Write-Host "  removing $installDir"
Start-Process -FilePath "cmd.exe" `
    -ArgumentList "/c", ("timeout /t 3 /nobreak >nul & rmdir /s /q `"{0}`"" -f $installDir) `
    -WindowStyle Hidden

Write-Host ""
Write-Host "  Uninstalled."
Write-Host ""
