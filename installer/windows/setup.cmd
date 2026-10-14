@echo off
rem Entry point launched by the self-extracting setup.exe after it unpacks the
rem payload. Asks the two choices that cannot be expressed by double-clicking, then
rem hands off to the gateway's own `install` subcommand -- which owns every bit of
rem install semantics (stop processes, atomic swap, shortcuts, uninstall entry,
rem optional autostart). This script holds no logic of its own on purpose: two
rem implementations of "how to install" is exactly the drift we set out to remove.
rem
rem Unattended use (deployment scripts, CI): call setup.cmd with flags directly, or
rem set WB_SETUP_SILENT=1 (optionally with WB_SETUP_FLAGS="-autostart").
rem
rem ASCII only: .cmd files are read using the OEM code page, so non-ASCII text here
rem would render as garbage on non-UTF8 systems.

setlocal enabledelayedexpansion

set "EXTRA="

rem Silent / unattended mode. Two ways in:
rem   * pass flags directly:            setup.cmd -autostart
rem   * set WB_SETUP_SILENT=1 with optional WB_SETUP_FLAGS="-autostart"
rem The second form is what makes this usable from a deployment script or an
rem automated verification run: the self-extractor starts this script in a *new*
rem console, so piped stdin cannot answer the prompts below.
if not "%~1"=="" (
  set "EXTRA=%*"
  goto run
)
if defined WB_SETUP_SILENT (
  echo   Silent mode ^(WB_SETUP_SILENT set^)
  set "EXTRA=%WB_SETUP_FLAGS%"
  goto run
)

echo.
echo   WorkBuddy Gateway - Setup
echo   =========================
echo.
echo   Installs the gateway (one binary, admin panel included) plus the optional
echo   local agent. Account data and configuration are never touched by an install
echo   or an upgrade; they live in:
echo     %LOCALAPPDATA%\wb-gateway
echo.
echo   No administrator rights are required.
echo.

set "AUTOSTART="
set /p "AUTOSTART=Start the gateway automatically when you log on? [y/N]: "
if /i "%AUTOSTART%"=="y" set "EXTRA=!EXTRA! -autostart"

set "OPENPANEL="
set /p "OPENPANEL=Open the management panel when finished? [Y/n]: "
if /i not "%OPENPANEL%"=="n" set "LAUNCH=1"

:run
echo.
echo   Installing...
echo.
"%~dp0workbuddy-gateway.exe" install -src "%~dp0" !EXTRA!
set "RC=%ERRORLEVEL%"

echo.
if not "%RC%"=="0" (
  echo   Installation FAILED ^(exit code %RC%^). See the messages above.
  echo.
  if not defined WB_SETUP_SILENT pause
  exit /b %RC%
)

echo   Installation complete.
echo.

if defined LAUNCH (
  rem The launcher lives in the install directory, not in this temporary unpack
  rem folder, so hand the browser the panel URL directly instead of guessing where
  rem the install landed (the installer decides that, not this script).
  start "" "http://127.0.0.1:8317/panel/"
)

rem Silent mode must not block on a keypress.
if defined WB_SETUP_SILENT exit /b 0
if not "%~1"=="" exit /b 0
pause
exit /b 0
