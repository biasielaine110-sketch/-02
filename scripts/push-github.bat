@echo off
REM Push this repo to GitHub.
REM Workaround for: git push silently failing (exit 128, no output) because
REM git spawns the credential helper through a broken shell in this environment.
REM Usage: double-click, or  push-github.bat "commit message"
setlocal
cd /d "%~dp0.."
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0push-github.ps1" %*
echo.
pause
