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
    # Port the gateway listens on. Written into launch.txt so **every** entry point
    # (shortcut, logon task) uses the same value -- a port that only reaches the
    # scheduled task makes the shortcut start a second instance on the default port
    # and both fight over the same data directory.
    [int]$Port = 8317,

    # Optional API key. Written into launch.txt and passed as --api-key by every
    # launcher. Empty means the panel is reachable without credentials, which is
    # fine because the launchers only bind 127.0.0.1.
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
# launch.txt is *generated* below, not copied: a stale file from a previous install
# would silently keep the old port / API key.

# ---------------------------------------------------------------------------
# Launch settings (port + optional API key)
# ---------------------------------------------------------------------------
# Written to a file instead of being baked into the shortcut / scheduled task:
# both launchers read it, so "start from the shortcut" and "start at logon" can
# never disagree about the port or the key.
#
# Why a file and not an environment variable: the scheduled task, the shortcut and
# a user's own `run.cmd` are three different launch contexts; only a file in the
# install dir is visible to all of them.
$launchFile = Join-Path $InstallDir "launch.txt"
$launchLines = @("port=$Port")
if (-not [string]::IsNullOrWhiteSpace($ApiKey)) {
    $launchLines += "api_key=$ApiKey"
}
# No BOM: the .cmd launchers parse this with plain `for /f`, and a BOM would end up
# glued to the first value. ASCII-only, so the ANSI/UTF-8 ambiguity does not matter.
Set-Content -Path $launchFile -Value $launchLines -Encoding ASCII
# The file holds a secret when -ApiKey was used: restrict it to the current user.
try {
    $acl = Get-Acl $launchFile
    $acl.SetAccessRuleProtection($true, $false)
    $rule = New-Object System.Security.AccessControl.FileSystemAccessRule(
        [System.Security.Principal.WindowsIdentity]::GetCurrent().Name,
        "FullControl", "Allow")
    $acl.SetAccessRule($rule)
    Set-Acl -Path $launchFile -AclObject $acl
} catch {
    Write-Host "    note: could not restrict permissions on launch.txt ($_)"
}
if ([string]::IsNullOrWhiteSpace($ApiKey)) {
    Write-Host "    launch settings: port $Port, no API key (loopback only)"
} else {
    Write-Host "    launch settings: port $Port, API key set"
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
# The shortcut name is deliberately **not** "WorkBuddy Gateway": the desktop
# package (Tauri/NSIS) creates a shortcut with exactly that name, and both
# installers would then overwrite each other's icon and delete it on uninstall
# while the other program is still installed. Two installs, one shortcut slot.
#
# The desktop build keeps the plain name because it is the form published on the
# Release page; this command-line form is the secondary one, so it gets a suffix.
Write-Step "Creating shortcuts"
$shell = New-Object -ComObject WScript.Shell
$shortcutName = "WorkBuddy Gateway (command line).lnk"

function New-Shortcut([string]$linkPath) {
    $dir = Split-Path -Parent $linkPath
    if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
    $lnk = $shell.CreateShortcut($linkPath)
    $lnk.TargetPath = Join-Path $InstallDir "run.cmd"
    $lnk.WorkingDirectory = $DataDir
    $lnk.IconLocation = (Join-Path $InstallDir "app.ico") + ",0"
    $lnk.Description = "WorkBuddy Gateway (command line form) - local gateway and admin panel"
    $lnk.Save()
}

$startMenu = Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs"
$desktopDir = [Environment]::GetFolderPath("Desktop")

# Older versions of this installer used the bare name. Remove that one first:
# leaving it behind means the desktop package's shortcut keeps getting clobbered
# on every upgrade of either form.
$legacyName = "WorkBuddy Gateway.lnk"
foreach ($dir in @($startMenu, $desktopDir)) {
    $legacy = Join-Path $dir $legacyName
    if (Test-Path $legacy) {
        Remove-Item -Force $legacy -ErrorAction SilentlyContinue
    }
}

New-Shortcut (Join-Path $startMenu $shortcutName)
New-Shortcut (Join-Path $desktopDir $shortcutName)
Write-Host "    start menu + desktop ($shortcutName)"

# ---------------------------------------------------------------------------
# Optional: start at logon
# ---------------------------------------------------------------------------
$taskName = "WorkBuddyGateway"
$taskPath = Join-Path $InstallDir "launch-hidden.vbs"

if ($AutoStart) {
    Write-Step "Registering logon task '$taskName'"
    # No port argument: launch-hidden.vbs reads launch.txt, exactly like run.cmd.
    # Baking the port in here is what made "-Port 9000" half-effective (task on
    # 9000, shortcut on 8317, two instances, one data directory).
    $action = New-ScheduledTaskAction -Execute "wscript.exe" `
        -Argument ('"{0}"' -f $taskPath)
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
