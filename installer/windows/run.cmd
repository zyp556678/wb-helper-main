@echo off
rem Launcher used by the Start menu and desktop shortcuts.
rem
rem It starts the gateway in its own window (so the log stays visible: for a
rem gateway, seeing what it is doing is a feature) and then opens the panel in the
rem default browser. The gateway needs around 30 seconds before it starts listening
rem (it pulls the model catalog first), so the browser is opened after a short wait
rem rather than immediately.
rem
rem Port and API key come from launch.txt next to this file (written by install.ps1).
rem Why a file rather than arguments baked into the shortcut: the logon task uses the
rem same settings, and the two must not be able to disagree -- a shortcut hardcoding
rem 8317 while the task runs on 9000 means two instances fighting over one data dir.
rem
rem ASCII only: .cmd files are read using the OEM code page.

setlocal
set "DIR=%~dp0"
set "DATA=%LOCALAPPDATA%\wb-gateway"
set "PORT=8317"
set "APIKEY="
set "GATEWAY=%DIR%workbuddy-gateway.exe"

if not exist "%GATEWAY%" (
  echo Cannot find workbuddy-gateway.exe next to this launcher.
  pause
  exit /b 1
)

rem Parse launch.txt: one key=value per line. Unknown keys are ignored, so a file
rem written by a newer installer does not break an older launcher.
if exist "%DIR%launch.txt" (
  for /f "usebackq tokens=1,* delims==" %%A in ("%DIR%launch.txt") do (
    if /i "%%A"=="port" set "PORT=%%B"
    if /i "%%A"=="api_key" set "APIKEY=%%B"
  )
)

rem Do not start a second instance if one is already listening.
netstat -ano | findstr /r /c:"LISTENING" | findstr /c:":%PORT% " >nul 2>&1
if not errorlevel 1 (
  echo The gateway already appears to be running on port %PORT%.
  start "" "http://127.0.0.1:%PORT%/panel/"
  exit /b 0
)

echo Starting WorkBuddy Gateway on port %PORT% ...
echo   data dir: %DATA%
echo.

set "ARGS=serve --port %PORT% --auth-dir "%DATA%""
if defined APIKEY set "ARGS=%ARGS% --api-key "%APIKEY%""

rem Declare the install form: the updater only picks a command-line package when it
rem knows it is not the desktop build. See internal/update/update.go detectForm.
set "WB_GATEWAY_HOST=cli"

start "WorkBuddy Gateway" "%GATEWAY%" %ARGS%

rem Wait for it to come up, then open the panel.
timeout /t 6 /nobreak >nul
start "" "http://127.0.0.1:%PORT%/panel/"

exit /b 0
