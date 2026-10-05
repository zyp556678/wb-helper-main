@echo off
rem Launcher used by the Start menu and desktop shortcuts.
rem
rem It starts the gateway in its own window (so the log stays visible: for a
rem gateway, seeing what it is doing is a feature) and then opens the panel in the
rem default browser. The gateway needs around 30 seconds before it starts listening
rem (it pulls the model catalog first), so the browser is opened after a short wait
rem rather than immediately.
rem
rem ASCII only: .cmd files are read using the OEM code page.

setlocal
set "DIR=%~dp0"
set "DATA=%LOCALAPPDATA%\wb-gateway"
set "PORT=8317"
set "GATEWAY=%DIR%workbuddy-gateway.exe"

if not exist "%GATEWAY%" (
  echo Cannot find workbuddy-gateway.exe next to this launcher.
  pause
  exit /b 1
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

start "WorkBuddy Gateway" "%GATEWAY%" serve --port %PORT% --auth-dir "%DATA%"

rem Wait for it to come up, then open the panel.
timeout /t 6 /nobreak >nul
start "" "http://127.0.0.1:%PORT%/panel/"

exit /b 0
