@echo off
cd /d "%~dp0"
if exist wb2api.exe (
  start "" /B wb2api.exe -config config.json
) else (
  echo wb2api.exe not found. Build first.
  pause
)
