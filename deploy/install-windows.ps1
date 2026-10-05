# WorkBuddy Gateway - Windows installer (Scheduled Task based)
#
# Why a Scheduled Task instead of a Windows Service:
#   The gateway needs to read credential files and write its cache/stats into the
#   *user's* own directory. A LocalSystem service would look at a different home
#   directory and run under a different account, so the account pool would appear
#   empty. Triggering at logon under the current user keeps paths and permissions
#   identical to running it by hand.
#
# Usage (in the extracted release folder, normal PowerShell, no admin needed for
# a per-user task):
#   powershell -ExecutionPolicy Bypass -File .\install-windows.ps1
#
# Idempotent: re-running replaces the binaries and re-registers the task.

# NOTE: this file is intentionally ASCII-only. Windows PowerShell 5.1 reads .ps1
# files as ANSI unless they carry a UTF-8 BOM, which would garble any non-ASCII
# text in here. All user-facing Chinese documentation lives in DEPLOY.md instead.

[CmdletBinding()]
param(
    # Directory that holds credentials, config.json, model cache and stats.
    # Defaults to %LOCALAPPDATA%\wb-gateway.
    [string]$DataDir = (Join-Path $env:LOCALAPPDATA "wb-gateway"),

    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA "wb-gateway\bin"),

    [int]$Port = 8317,

    # Optional API key. When empty the gateway runs without panel authentication,
    # which is fine for loopback-only use.
    [string]$ApiKey = "",

    [string]$TaskName = "WorkBuddyGateway"
)

$ErrorActionPreference = "Stop"

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$exeName = "workbuddy-gateway.exe"
$agentName = "wb-local-agent.exe"

$srcExe = Join-Path $scriptDir $exeName
if (-not (Test-Path $srcExe)) {
    throw "Cannot find $exeName next to this script ($scriptDir)."
}

Write-Host "==> Creating directories"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

Write-Host "==> Installing binaries"
Copy-Item -Force $srcExe (Join-Path $InstallDir $exeName)

# The local agent is optional: it only powers the local-client panel page.
# On a server there is nothing for it to read, so a missing file is not an error.
$srcAgent = Join-Path $scriptDir $agentName
if (Test-Path $srcAgent) {
    Copy-Item -Force $srcAgent (Join-Path $InstallDir $agentName)
    Write-Host "    local agent installed (local client panel page enabled)"
} else {
    Write-Host "    no local agent in package (local client panel page will be hidden)"
}

# Seed a config example so the file layout is discoverable, but never overwrite
# an existing config.json - that would wipe user settings.
$srcExample = Join-Path $scriptDir "config.example.json"
$dstConfig = Join-Path $DataDir "config.json"
if ((Test-Path $srcExample) -and -not (Test-Path $dstConfig)) {
    Copy-Item -Force $srcExample (Join-Path $DataDir "config.example.json")
}

Write-Host "==> Registering scheduled task '$TaskName' (trigger: at logon)"
$targetExe = Join-Path $InstallDir $exeName
$argList = "serve --addr 127.0.0.1 --port $Port"
if ($ApiKey -ne "") {
    $argList = "$argList --api-key $ApiKey"
}

$action = New-ScheduledTaskAction -Execute $targetExe -Argument $argList -WorkingDirectory $DataDir
$trigger = New-ScheduledTaskTrigger -AtLogOn
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -RestartCount 3 `
    -RestartInterval (New-TimeSpan -Minutes 1) `
    -ExecutionTimeLimit (New-TimeSpan -Seconds 0)   # 0 = no limit, it is a long-running server

# Unregister first so re-running the installer does not fail on a name conflict.
if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
}
Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings | Out-Null
Start-ScheduledTask -TaskName $TaskName

Write-Host ""
Write-Host "==> Done"
Write-Host ""
Write-Host "  Panel     http://127.0.0.1:$Port/panel/"
Write-Host "  Data dir  $DataDir"
Write-Host "  Start     Start-ScheduledTask   -TaskName $TaskName"
Write-Host "  Stop      Stop-ScheduledTask    -TaskName $TaskName"
Write-Host "  Remove    Unregister-ScheduledTask -TaskName $TaskName -Confirm:`$false"
Write-Host ""
Write-Host "Drop credential files (workbuddy*.json) into the data dir and they appear"
Write-Host "in the panel automatically."
Write-Host ""
Write-Host "Note: at startup the gateway pulls the model catalog once before it starts"
Write-Host "listening, so /healthz refuses connections for the first ~30 seconds."
