@echo off
rem Uninstaller. Add/Remove Programs points here (the registry value is written by
rem the Go installer, which names this file as the UninstallString).
rem
rem Like setup.cmd, this holds no logic: it forwards to the gateway's own
rem `uninstall` subcommand. That command stops the running instance, removes the
rem A/R entry, shortcuts and autostart, deletes the install directory, and keeps
rem the data directory (credentials) unless -drop-data is passed.
rem
rem The uninstall then completes in a detached copy of the gateway: on Windows a
rem running executable cannot delete the directory it lives in, so the command
rem relaunches itself from %TEMP% and finishes there. That is why the window may
rem close before the directory is gone -- it is still being removed.
rem
rem ASCII only (.cmd files are read using the OEM code page).

setlocal
set "WB_DIR=%~dp0"
rem Strip the trailing backslash that %~dp0 always carries.
if "%WB_DIR:~-1%"=="\" set "WB_DIR=%WB_DIR:~0,-1%"

if not exist "%WB_DIR%\bin\workbuddy-gateway.exe" (
  echo Cannot find bin\workbuddy-gateway.exe under "%WB_DIR%".
  echo The installation looks incomplete; nothing to uninstall.
  pause
  exit /b 1
)

"%WB_DIR%\bin\workbuddy-gateway.exe" uninstall -dir "%WB_DIR%" %*
exit /b %ERRORLEVEL%
