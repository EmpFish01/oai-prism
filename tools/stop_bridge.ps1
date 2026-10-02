# ============================================================
# OAIprism 桥一键停止（兜底清理）
#
# 正常情况请直接在启动窗口按 Q+Enter / Ctrl+C（会自动还原 Codex 配置）。
# 本脚本用于窗口被直接关掉后的兜底：杀掉残留的网关/sidecar/自动化
# Chrome，并还原 Codex 配置。
# ============================================================
$ErrorActionPreference = "Continue"

$Repo = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path

function Stop-ByPort([int]$Port, [string]$Name) {
    $conns = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
    if (-not $conns) { Write-Host "  $Name ($Port): 未运行"; return }
    foreach ($c in ($conns | Select-Object -ExpandProperty OwningProcess -Unique)) {
        taskkill /F /T /PID $c 2>$null | Out-Null
        Write-Host "  $Name ($Port): 已停止 PID $c"
    }
}

Write-Host "停止 OAIprism 桥 ..."
Stop-ByPort 8787 "网关"
Stop-ByPort 8790 "sidecar"

# 清理自动化 Chrome（sidecar 拉起的；按特征识别，不动日常浏览器）
$killed = 0
Get-CimInstance Win32_Process -Filter "name='chrome.exe'" -ErrorAction SilentlyContinue | ForEach-Object {
    $cl = $_.CommandLine
    if ($cl -and ($cl -match 'remote-debugging-pipe' -or $cl -match 'playwright')) {
        Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
        $killed++
    }
}
Write-Host "  自动化 Chrome: 清理 $killed 个"

# 还原 Codex 配置（临时 provider 与环境变量）
& powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $Repo "tools\setup_codex_config.ps1") -Undo

Write-Host "完成。下次使用请运行 tools\start_bridge.cmd（或 start-bridge-desktop.cmd）。"
