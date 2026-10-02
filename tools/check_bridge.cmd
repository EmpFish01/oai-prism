@echo off
REM ============================================================
REM OAIprism 桥一键诊断：codex 报错时先跑这个
REM ============================================================
setlocal
pushd "%~dp0.."
set REPO=%CD%
popd
set FIX=0

echo === 1) codex 配置 ===
findstr /C:"base_url = \"http://localhost:8787/v1\"" "%USERPROFILE%\.codex\config.toml" >nul 2>&1
if %errorlevel%==0 (echo    OK: model_provider 指向 8787) else (echo    异常: 检查 %%USERPROFILE%%\.codex\config.toml 的 base_url & set FIX=1)
findstr /C:"model = \"gpt-6.1-sol\"" "%USERPROFILE%\.codex\config.toml" >nul 2>&1
if %errorlevel%==0 (echo    OK: model = gpt-6.1-sol) else (echo    注意: 当前 model 不是 gpt-6.1-sol)

echo.
echo === 2) OAIprism 网关 (8787) ===
curl -s --max-time 4 http://127.0.0.1:8787/healthz
if %errorlevel%==0 (echo  OK) else (echo  未运行 -^> 跑 tools\start_bridge.cmd & set FIX=1)

echo.
echo === 3) 浏览器通道 sidecar (8790) ===
curl -s --max-time 6 -o nul -w "HTTP %%{http_code}" -X POST http://127.0.0.1:8790/api/maintenance
if %errorlevel%==0 (echo   OK -^> 405 属正常，能连上即可) else (echo  未运行 -^> 跑 tools\start_bridge.cmd & set FIX=1)

echo.
echo === 4) 上游连通性（经 sidecar 建临时项目）===
curl -s --max-time 30 -X POST -H "Content-Type: application/json" -d "{\"project_uuid\":\"00000000-0000-4000-8000-000000000000\",\"title\":\"bridge-healthcheck\"}" http://127.0.0.1:8790/api/projects
echo.
echo    （返回 JSON 项目对象 = 通；返回 401/403 = sidecar 的登录态失效，
echo      重启 sidecar 即可自愈 —— 重新跑 tools\start_bridge.cmd）

echo.
if %FIX%==1 (echo 结论: 有服务未就绪，请运行 tools\start_bridge.cmd) else (echo 结论: 链路正常，codex 可以直接用)
endlocal
