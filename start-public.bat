@echo off
cd /d "%~dp0"
if not exist wb2api.exe (
  echo wb2api.exe not found.
  pause
  exit /b 1
)
start "" /B wb2api.exe -config config.json
start "" /B "C:\Program Files (x86)\cloudflared\cloudflared.exe" tunnel --url http://127.0.0.1:7863 --no-autoupdate
echo Started wb2api and Cloudflare tunnel.
echo Check cloudflared.err.log for the public https URL.
