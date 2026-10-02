@echo off
chcp 65001 >nul
REM ============================================================
REM OAIprism 桥 - CLI 会话（双击运行）
REM
REM 窗口开着 = 代理开着；按 Q+Enter / Ctrl+C / 关闭窗口即全部停止，
REM 并自动还原 Codex 配置。与 excel-codex-bridge 的使用方式一致：
REM 另开终端直接运行 codex，或在桌面版里使用。
REM ============================================================
setlocal
pushd "%~dp0.."
set REPO=%CD%
popd

if not exist "%REPO%\bin\oaiprism.exe" (
    echo bin\oaiprism.exe 不存在，先构建...
    pushd "%REPO%"
    if exist ".toolchain\go\bin\go.exe" (
        set "PATH=%REPO%\.toolchain\go\bin;%PATH%"
    )
    go build -trimpath -ldflags "-s -w" -o bin\oaiprism.exe ./cmd/oaiprism || (echo 构建失败 & popd & exit /b 1)
    popd
)

powershell -NoProfile -ExecutionPolicy Bypass -File "%REPO%\tools\bridge_session.ps1"
endlocal
