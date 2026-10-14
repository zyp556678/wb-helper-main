@echo off
rem Entry point launched by the self-extracting setup.exe after it unpacks the
rem payload. Hands off to the gateway's own `install` subcommand, which owns every
rem bit of install semantics (stop processes, atomic swap, shortcuts, uninstall
rem entry, optional autostart). This script holds no logic of its own on purpose:
rem two implementations of "how to install" is exactly the drift we set out to remove.
rem
rem NOTHING IN HERE MAY PROMPT. The self-extractor launches this with an
rem INVISIBLE console -- no window, no way to type (measured: the spawned cmd.exe
rem has MainWindowHandle == 0). An earlier version asked two `set /p` questions
rem like the old installer did; every double-click then left a cmd.exe parked
rem forever on that prompt. That is what "I double-clicked and it just flashed"
rem actually was: the extraction window closed, and the real installer sat
rem waiting for input nobody could see or give.
rem
rem Choices are therefore passed in as flags, never asked:
rem   setup.cmd -autostart          (or set WB_SETUP_FLAGS to anything)
rem A plain double-click installs with the defaults below.
rem
rem Outcome is reported through a message box, which is the only channel guaranteed
rem to be visible when the console is not.
rem
rem ASCII only: .cmd files are read using the OEM code page, so non-ASCII text here
rem would render as garbage on non-UTF8 systems.

setlocal enabledelayedexpansion

set "LOG=%TEMP%\workbuddy-setup.log"
if exist "%LOG%" del "%LOG%" >nul 2>&1

set "EXTRA="
if not "%~1"=="" set "EXTRA=%*"
if defined WB_SETUP_FLAGS set "EXTRA=%WB_SETUP_FLAGS%"

echo.
echo   WorkBuddy Gateway - Setup
echo   =========================
echo.
echo   Installing...
echo.

rem %~dp0 ALWAYS ends with a backslash, and a quoted path ending in one is a trap:
rem `"C:\dir\"` makes the closing quote an ESCAPED quote in the Windows command
rem line, so the argument never terminates and swallows everything after it --
rem the installer received `C:\...\IXP000.TMP"` and failed to find its payload.
rem Strip the trailing backslash before quoting. (Verified by printing os.Args in
rem cmd.exe: the broken form yields a src path with a stray quote.)
set "SRC=%~dp0"
if "%SRC:~-1%"=="\" set "SRC=%SRC:~0,-1%"

"%~dp0workbuddy-gateway.exe" install -src "%SRC%" !EXTRA!
set "RC=%ERRORLEVEL%"

if not "%RC%"=="0" (
  rem Leave a record on disk too: a message box can be dismissed, and the exact
  rem command is what makes it reproducible.
  > "%LOG%" echo WorkBuddy Gateway setup failed with exit code %RC%.
  >> "%LOG%" echo Command: "%~dp0workbuddy-gateway.exe" install -src "%SRC%" !EXTRA!
  >> "%LOG%" echo.
  >> "%LOG%" echo Re-run the installer by hand to see the full output:
  >> "%LOG%" echo   "%SRC%\workbuddy-gateway.exe" install -src "%SRC%" !EXTRA!
  call :notify "WorkBuddy Gateway setup FAILED (exit code %RC%)." "Details: %LOG%"
  exit /b %RC%
)

rem Start what was just installed, so a successful double-click ends with something
rem the user can actually see. The gateway itself needs ~30s before it answers (it
rem pulls the model catalog first), so the launcher -- which waits -- is preferred
rem over opening a browser at an address that is not listening yet.
rem
rem Skipped when the caller passed -dir: the launcher below lives at the *default*
rem location, so starting it would launch a different installation than the one we
rem just wrote (or an older one). A caller that chooses its own directory can start
rem it itself.
set "CUSTOM="
echo !EXTRA! | findstr /i /c:"-dir" >nul 2>&1
if not errorlevel 1 set "CUSTOM=1"

if defined CUSTOM (
  call :notify "WorkBuddy Gateway installed." "Installed to the requested directory. Panel: http://127.0.0.1:8317/panel/"
  exit /b 0
)

set "APP=%LOCALAPPDATA%\WorkBuddy Gateway"
if exist "%APP%\workbuddy-gateway-desktop.exe" (
  start "" "%APP%\workbuddy-gateway-desktop.exe"
  call :notify "WorkBuddy Gateway installed." "The app is starting; the panel is at http://127.0.0.1:8317/panel/"
) else if exist "%APP%\run.cmd" (
  start "" "%APP%\run.cmd"
  call :notify "WorkBuddy Gateway installed." "It is starting now; the panel is at http://127.0.0.1:8317/panel/"
) else (
  call :notify "WorkBuddy Gateway installed." "Start it from the Start menu; the panel is at http://127.0.0.1:8317/panel/"
)
exit /b 0

:notify
rem %1 = title line, %2 = detail line. Shown as a GUI message box because the
rem console handed to us by the self-extractor has no window.
powershell -NoProfile -NonInteractive -WindowStyle Hidden -Command "Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.MessageBox]::Show('%~2','%~1','OK','Information')" >nul 2>&1
exit /b 0
