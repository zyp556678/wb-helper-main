# WorkBuddy Gateway - Windows installer
#
# Install scope is per-user (HKCU / %LOCALAPPDATA%), so no administrator rights are
# needed and the account pool lives in the user's own profile.
#
# Why not a Windows Service / machine-wide install: the gateway reads credential
# files and writes its cache and stats into the *user's* directory. A LocalSystem
# service would look at a different home directory under a different account and the
# account pool would appear empty. A per-user install keeps paths and permissions
# identical to running the binary by hand.
#
# This file is intentionally ASCII-only. Windows PowerShell 5.1 reads .ps1 files as
# ANSI unless they carry a UTF-8 BOM, which would garble any non-ASCII text here.

[CmdletBinding()]
param(
    # Port the gateway listens on. Only loopback is bound by the launcher.
    [int]$Port = 8317,

    # Optional API key. Empty means the panel is reachable without credentials,
    # which is fine because the launcher only binds 127.0.0.1.
    [string]$ApiKey = "",

    # Register a scheduled task that starts the gateway hidden at logon.
    [switch]$AutoStart,

    # Open the management panel when installation finishes.
    [switch]$Launch,

    [string]$InstallDir = "",

    [string]$DataDir = ""
)

$ErrorActionPreference = "Stop"

function Write-Step($msg) { Write-Host "==> $msg" }

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------
$srcDir = Split-Path -Parent $MyInvocation.MyCommand.Path

if ([string]::IsNullOrWhiteSpace($InstallDir)) {
    $InstallDir = Join-Path $env:LOCALAPPDATA "Programs\workbuddy-gateway"
}
if ([string]::IsNullOrWhiteSpace($DataDir)) {
    $DataDir = Join-Path $env:LOCALAPPDATA "wb-gateway"
}

$gatewayName = "workbuddy-gateway.exe"
$agentName = "wb-local-agent.exe"
$srcGateway = Join-Path $srcDir $gatewayName

if (-not (Test-Path $srcGateway)) {
    throw "Cannot find $gatewayName next to this script ($srcDir)."
}

# ---------------------------------------------------------------------------
# Stop a running instance so the binaries can be replaced
# ---------------------------------------------------------------------------
# Only stop processes whose image actually lives in our install dir: another copy
# of the gateway (for example one started by hand from a source checkout) must not
# be killed by this installer.
function Stop-GatewayIn([string]$dir, [string[]]$names) {
    $stopped = 0
    foreach ($name in $names) {
        $procs = Get-CimInstance Win32_Process -Filter "Name='$name'" -ErrorAction SilentlyContinue
        foreach ($p in $procs) {
            $exe = $p.ExecutablePath
            if ($exe -and ($exe -like "$dir*")) {
                Write-Host "    stopping $name (PID $($p.ProcessId))"
                Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
                $stopped++
            }
        }
    }
    if ($stopped -gt 0) { Start-Sleep -Milliseconds 800 }
}

Write-Step "Stopping previously installed instance (if any)"
Stop-GatewayIn $InstallDir @($gatewayName, $agentName)

# ---------------------------------------------------------------------------
# Copy payload
# ---------------------------------------------------------------------------
Write-Step "Installing to $InstallDir"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

Copy-Item -Force $srcGateway (Join-Path $InstallDir $gatewayName)

# The local agent is optional: it only powers the local-client panel page.
$srcAgent = Join-Path $srcDir $agentName
if (Test-Path $srcAgent) {
    Copy-Item -Force $srcAgent (Join-Path $InstallDir $agentName)
} else {
    Write-Host "    note: no local agent in this package (local-client panel page will be hidden)"
}

# Files needed at runtime / at uninstall time
foreach ($f in @("app.ico", "run.cmd", "launch-hidden.vbs", "uninstall.ps1", "INSTALL.txt")) {
    $src = Join-Path $srcDir $f
    if (Test-Path $src) { Copy-Item -Force $src (Join-Path $InstallDir $f) }
}

# Read the version from the binary itself so the uninstall entry can never drift
# from the code (a hardcoded string here would rot on the first release).
$version = "0.0.0"
try {
    $raw = & $srcGateway version 2>$null | Select-Object -First 1
    if ($raw -match "v(\S+)") { $version = $Matches[1] }
} catch {
    Write-Host "    note: could not read version from binary, using $version"
}
Write-Host "    version $version"

# ---------------------------------------------------------------------------
# Shortcuts
# ---------------------------------------------------------------------------
Write-Step "Creating shortcuts"
$shell = New-Object -ComObject WScript.Shell

function New-Shortcut([string]$linkPath) {
    $dir = Split-Path -Parent $linkPath
    if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
    $lnk = $shell.CreateShortcut($linkPath)
    $lnk.TargetPath = Join-Path $InstallDir "run.cmd"
    $lnk.WorkingDirectory = $DataDir
    $lnk.IconLocation = (Join-Path $InstallDir "app.ico") + ",0"
    $lnk.Description = "WorkBuddy Gateway - local gateway and admin panel"
    $lnk.Save()
}

$startMenu = Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs"
New-Shortcut (Join-Path $startMenu "WorkBuddy Gateway.lnk")
New-Shortcut (Join-Path ([Environment]::GetFolderPath("Desktop")) "WorkBuddy Gateway.lnk")
Write-Host "    start menu + desktop"

# ---------------------------------------------------------------------------
# Optional: start at logon
# ---------------------------------------------------------------------------
$taskName = "WorkBuddyGateway"
$taskPath = Join-Path $InstallDir "launch-hidden.vbs"

if ($AutoStart) {
    Write-Step "Registering logon task '$taskName'"
    $action = New-ScheduledTaskAction -Execute "wscript.exe" `
        -Argument ('"{0}" {1}' -f $taskPath, $Port)
    $trigger = New-ScheduledTaskTrigger -AtLogOn
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries `
        -DontStopIfGoingOnBatteries -StartWhenAvailable `
        -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) `
        -ExecutionTimeLimit (New-TimeSpan -Seconds 0)
    if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
    }
    Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger `
        -Settings $settings | Out-Null
    Write-Host "    the gateway will start hidden at logon; log: $DataDir\logs\gateway.log"
} else {
    # Remove a task left over from a previous install so the choice is not sticky
    if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
        Write-Host "    previous logon task removed (auto start disabled)"
    }
}

# ---------------------------------------------------------------------------
# Uninstall entry (Add/Remove Programs)
# ---------------------------------------------------------------------------
Write-Step "Registering uninstall entry"
$regPath = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\WorkBuddyGateway"
New-Item -Path $regPath -Force | Out-Null
$uninstallPs = Join-Path $InstallDir "uninstall.ps1"
$sizeKb = 0
Get-ChildItem $InstallDir -File -ErrorAction SilentlyContinue | ForEach-Object {
    $sizeKb += [int]($_.Length / 1KB)
}

$reg = @{
    DisplayName     = "WorkBuddy Gateway"
    DisplayVersion  = $version
    Publisher       = "workbuddy-gateway"
    InstallLocation = $InstallDir
    DisplayIcon     = (Join-Path $InstallDir "app.ico")
    UninstallString = 'powershell.exe -NoProfile -ExecutionPolicy Bypass -File "{0}"' -f $uninstallPs
    QuietUninstallString = 'powershell.exe -NoProfile -ExecutionPolicy Bypass -File "{0}" -Silent' -f $uninstallPs
    NoModify        = 1
    NoRepair        = 1
    EstimatedSize   = $sizeKb
}
foreach ($k in $reg.Keys) {
    # Compute the registry type in a statement, not inline: Windows PowerShell 5.1
    # has no "if as expression" form (that arrives with PowerShell 7).
    $type = "String"
    if ($reg[$k] -is [int]) { $type = "DWord" }
    New-ItemProperty -Path $regPath -Name $k -Value $reg[$k] -PropertyType $type -Force | Out-Null
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
Write-Host ""
Write-Host "  Installed:  $InstallDir"
Write-Host "  Data dir:   $DataDir"
Write-Host "  Panel:      http://127.0.0.1:$Port/panel/"
Write-Host ""
Write-Host "  Drop credential files (workbuddy*.json) into the data dir, or use the"
Write-Host "  'Add account' button in the panel, and they appear automatically."
Write-Host ""
Write-Host "  Note: at startup the gateway pulls the model catalog once before it"
Write-Host "  starts listening, so the panel answers after roughly 30 seconds."

if ($Launch) {
    Write-Step "Starting the gateway"
    Start-Process -FilePath (Join-Path $InstallDir "run.cmd") -WorkingDirectory $DataDir | Out-Null
}
