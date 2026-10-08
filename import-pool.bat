@echo off
setlocal
cd /d "%~dp0"

REM ===================================================================
REM  Import an account pool zip produced by export-pool.bat.
REM
REM  Usage:
REM    import-pool.bat                     use newest zip in backups\
REM    import-pool.bat path\to\pool.zip    use a specific package
REM
REM  Existing auths\ data\ config.json are first copied to
REM  backups\pre-import-<timestamp>\ so the import can be rolled back.
REM
REM  Stop the gateway before importing:  taskkill /IM wb2api.exe /F
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

if "%~1"=="" (
  powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\pool-transfer.ps1" -Mode import
) else (
  powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\pool-transfer.ps1" -Mode import -Package "%~1"
)
set "RC=%errorlevel%"

echo.
if not "%RC%"=="0" echo [ERROR] import failed, see messages above.
pause
exit /b %RC%
