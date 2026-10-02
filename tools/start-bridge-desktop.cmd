@echo off
chcp 65001 >nul
REM ============================================================
REM OAIprism 桥 - 桌面版会话（双击运行）
REM
REM 与 start_bridge.cmd 相同，但会拉起 Codex 桌面应用。
REM 窗口开着 = 代理开着；Q+Enter / Ctrl+C / 关闭窗口即全部停止，
REM 并自动还原 Codex 配置（桌面版需重启才会切回官方登录）。
REM ============================================================
setlocal
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0bridge_session.ps1" -Desktop
endlocal
