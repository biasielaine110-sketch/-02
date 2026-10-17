@echo off
setlocal
cd /d "%~dp0"

REM ===================================================================
REM  Public tunnel for the wb2api gateway (127.0.0.1:7863)
REM
REM  Fix log (2026-10-18):
REM   1) Route cloudflared through the local proxy 127.0.0.1:7897 --
REM      the quick-tunnel API (api.trycloudflare.com) times out when
REM      reached directly from this network.
REM   2) Pin --protocol http2 -- QUIC is partially blocked here.
REM   3) Start detached via start /MIN cmd /c so closing this window
REM      does NOT kill the tunnel. Output goes to tunnel.log.
REM   4) Assumes the gateway is already running (start.bat).
REM      NOTE: the trycloudflare URL is NEW on every run of this script.
REM ===================================================================

set "HTTPS_PROXY=http://127.0.0.1:7897"
set "HTTP_PROXY=http://127.0.0.1:7897"

if not exist "C:\Program Files (x86)\cloudflared\cloudflared.exe" (
  echo [ERROR] cloudflared.exe not found at the default install path.
  pause
  exit /b 1
)

REM replace any old tunnel instance, then start detached with log capture
taskkill /IM cloudflared.exe /F >nul 2>&1
del tunnel.log 2>nul
start "wb2api-tunnel" /MIN cmd /c ""C:\Program Files (x86)\cloudflared\cloudflared.exe" tunnel --url http://127.0.0.1:7863 --no-autoupdate --protocol http2 >tunnel.log 2>&1"

echo [1/2] Tunnel starting, waiting for the public URL ...
set /a tries=0
:waitloop
set /a tries+=1
timeout /t 2 /nobreak >nul 2>&1
findstr /C:"trycloudflare.com" tunnel.log >nul 2>&1
if errorlevel 1 (
  if %tries% lss 20 goto waitloop
  echo [WARN] No URL within 40s. Check tunnel.log for details.
  pause
  exit /b 1
)

for /f %%u in ('powershell -NoProfile -Command "[regex]::Match((Get-Content tunnel.log -Raw),'https://[a-z0-9\-]+\.trycloudflare\.com').Value"') do set "PUBURL=%%u"

echo [OK]  Public base:  %PUBURL%
echo.
echo       API base:     %PUBURL%/v1
echo       Panel:        %PUBURL%/panel/
echo       Auth:         Bearer key = config.json api_key (panel Config page)
echo       Stop tunnel:  taskkill /IM cloudflared.exe /F
echo       IMPORTANT:    this URL changes every time you rerun this script.
echo       Log:          tunnel.log
echo.
pause
