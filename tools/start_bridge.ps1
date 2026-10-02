# Thin wrapper: same as double-clicking tools\start_bridge.cmd.
# The session logic lives in tools\bridge_session.ps1 (window stays open =
# proxy stays up; Q / Ctrl+C / closing the window stops everything and
# restores the Codex config).
param([switch]$Desktop)
& powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot "bridge_session.ps1") @PSBoundParameters
