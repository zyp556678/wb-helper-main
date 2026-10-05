@echo off
rem Entry point launched by the self-extracting setup.exe after it unpacks the
rem payload. Asks the two choices that cannot be expressed by double-clicking, then
rem hands off to install.ps1 (which does all the real work).
rem
rem Unattended use (deployment scripts, CI): call setup.cmd with flags directly, or
rem set WB_SETUP_SILENT=1 (optionally with WB_SETUP_FLAGS="-AutoStart -Launch").
rem
rem ASCII only: .cmd files are read using the OEM code page, so non-ASCII text here
rem would render as garbage on non-UTF8 systems.

setlocal enabledelayedexpansion

set "EXTRA="

rem Silent / unattended mode. Two ways in:
rem   * pass flags directly:            setup.cmd -AutoStart -Launch
rem   * set WB_SETUP_SILENT=1 with optional WB_SETUP_FLAGS="-AutoStart"
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
echo   local agent into:
echo     %LOCALAPPDATA%\Programs\workbuddy-gateway
echo.
echo   Account data and configuration live in:
echo     %LOCALAPPDATA%\wb-gateway
echo.
echo   No administrator rights are required.
echo.

set "AUTOSTART="
set /p "AUTOSTART=Start the gateway automatically when you log on? [y/N]: "
if /i "%AUTOSTART%"=="y" set "EXTRA=!EXTRA! -AutoStart"

set "OPENPANEL="
set /p "OPENPANEL=Open the management panel when finished? [Y/n]: "
if /i not "%OPENPANEL%"=="n" set "EXTRA=!EXTRA! -Launch"

:run
echo.
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" !EXTRA!
set "RC=%ERRORLEVEL%"

echo.
if not "%RC%"=="0" (
  echo   Installation FAILED ^(exit code %RC%^). See the messages above.
  echo.
  pause
  exit /b %RC%
)

echo   Installation complete.
echo.

rem Silent mode must not block on a keypress.
if defined WB_SETUP_SILENT exit /b 0
if not "%~1"=="" exit /b 0
pause
exit /b 0
