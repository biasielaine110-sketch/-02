@echo off
setlocal
cd /d "%~dp0"

REM ===================================================================
REM  Export the local account pool into a portable zip.
REM
REM  Packs: auths\  data\  config.json  (and backups\MIGRATE.txt)
REM  Output: backups\pool-<timestamp>.zip
REM
REM  The zip holds PLAINTEXT tokens: move it offline only.
REM ===================================================================

if not exist scripts\pool-transfer.ps1 (
  echo [ERROR] scripts\pool-transfer.ps1 not found.
  pause
  exit /b 1
)

where powershell >nul 2>&1
if errorlevel 1 (
  echo [ERROR] PowerShell not found. This script needs it.
  pause
  exit /b 1
)

powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\pool-transfer.ps1" -Mode export
set "RC=%errorlevel%"

echo.
if not "%RC%"=="0" echo [ERROR] export failed, see messages above.
pause
exit /b %RC%
