@echo off
setlocal
cd /d "%~dp0"

REM ===================================================================
REM  WorkBuddy2API 本地启动脚本
REM
REM  修正记录（2026-09-19）：
REM   原 start.bat 直接 `start "" /B wb2api.exe` 启动，存在两个问题：
REM   1) 进程挂在当前 cmd 会话上，窗口关闭 / 父进程退出时网关被一并回收；
REM   2) 未处理系统代理——Windows 若开了「Internet 选项 → 代理服务器」，
REM      网关出站会误走该代理。当代理只服务浏览器、不转发 Go 的 HTTPS
REM      出站时，表现为全部上游调用失败：
REM        upstream connect failed: 由于目标计算机积极拒绝，无法连接。(os error 10061)
REM      本脚本显式设置 NO_PROXY 放行本地回环，避免面板/健康检查被代理劫持。
REM ===================================================================

if not exist wb2api.exe (
  echo [ERROR] wb2api.exe not found. Build it first:
  echo         go build -trimpath -ldflags="-s -w" -o wb2api.exe ./cmd/server
  pause
  exit /b 1
)

REM 本地回环不走代理（面板、healthz、curl 自检）
set "NO_PROXY=127.0.0.1,localhost,::1"
set "no_proxy=127.0.0.1,localhost,::1"

echo [1/2] Starting wb2api gateway on :7863 ...
REM 以独立窗口启动，脱离本脚本所在会话，脚本退出后网关继续存活
start "wb2api" /MIN cmd /c "wb2api.exe -config config.json 1>>wb2api.log 2>>wb2api.err.log"

REM 等待端口就绪（最多 ~15s）
set /a _tries=0
:waitloop
set /a _tries+=1
timeout /t 1 /nobreak >nul 2>&1
powershell -NoProfile -Command "try{(Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 'http://127.0.0.1:7863/healthz' -Proxy $null).StatusCode|Out-Null; exit 0}catch{exit 1}" >nul 2>&1
if %errorlevel% neq 0 (
  if %_tries% lss 15 goto waitloop
  echo [WARN] healthz did not respond within 15s. See wb2api.err.log
  goto :afterapp
)
echo [OK]  Gateway is up:  http://127.0.0.1:7863/healthz

:afterapp
echo [2/2] Panel:  http://127.0.0.1:7863/panel/
echo.
echo Logs:  wb2api.log / wb2api.err.log
echo Stop:  taskkill /IM wb2api.exe /F
echo.
echo To expose publicly, run start-public.bat (adds a Cloudflare quick tunnel).
pause
