# OAIprism bridge SESSION launcher (mirrors excel-codex-bridge's design):
# the proxy lives inside THIS console window - close the window / Ctrl+C / Q
# to stop everything. Nothing is left running in the background.
#
#   tools\start_bridge.cmd            -> CLI session (this script)
#   tools\start-bridge-desktop.cmd    -> desktop session (-Desktop, launches the Codex app)
#
# On start: applies the temporary Codex config (provider + env var), starts the
# gateway (8787) and the headless sidecar (8790) as children of this console.
# On stop (Q / Ctrl+C / gateway exit): kills the children and RESTORES the
# original Codex config, so Codex falls back to official auth when the bridge
# is off.
param([switch]$Desktop)
$ErrorActionPreference = "Continue"

$Repo   = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$LogDir = Join-Path $Repo "logs"
New-Item -ItemType Directory -Force $LogDir | Out-Null

function Test-Port([int]$Port) {
    $c = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
    return [bool]$c
}
function Stop-Stale {
    # Kill leftovers from a previous session (window was hard-closed, etc.)
    foreach ($port in 8787, 8790) {
        $conns = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue
        foreach ($c in ($conns | Select-Object -ExpandProperty OwningProcess -Unique)) {
            taskkill /F /T /PID $c 2>$null | Out-Null
            Write-Host ("    killed stale PID " + $c + " on port " + $port)
        }
    }
}

Write-Host "============================================================"
Write-Host " OAIprism bridge session" $(if ($Desktop) { "(desktop mode)" } else { "(CLI mode)" })
Write-Host "   gateway : 127.0.0.1:8787   (OpenAI/Anthropic compatible)"
Write-Host "   sidecar : 127.0.0.1:8790   (headless Chrome -> prism.openai.com)"
Write-Host "   stop    : press Q + Enter here, or Ctrl+C, or close this window"
Write-Host "============================================================"

Stop-Stale

Write-Host ""
Write-Host "[1/3] Applying temporary Codex config (provider + env var)..."
& powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $Repo "tools\setup_codex_config.ps1")

Write-Host "[2/3] Starting sidecar (headless Chrome, warms up ~30s)..."
# -NoNewWindow: sidecar shares THIS console, so closing the window / Ctrl+C
# takes it down together with the gateway - nothing survives in background.
$sidecar = Start-Process -FilePath "node" `
    -ArgumentList @((Join-Path $Repo "tools\browser_sidecar.js"), "auto", "8790") `
    -WorkingDirectory $Repo `
    -PassThru -NoNewWindow

Write-Host "[3/3] Starting gateway (foreground logs follow)..."
# Same auth posture as packaging\windows\start.ps1: the key file is the only
# valid credential source, and its value is what setup_codex_config.ps1 hands
# to Codex via the user env var - the two always match.
$env:OAI_PRISM_API_KEYS_FILE = Join-Path $Repo "secrets\api-key.txt"
$gw = Start-Process -FilePath (Join-Path $Repo "bin\oaiprism.exe") `
    -ArgumentList @("serve", "-config", (Join-Path $Repo "configs\config.yaml")) `
    -WorkingDirectory $Repo `
    -PassThru -NoNewWindow

$ready = $false
for ($i = 0; $i -lt 20 -and -not $gw.HasExited; $i++) {
    Start-Sleep -Milliseconds 500
    try { $ready = (Invoke-WebRequest -Uri "http://127.0.0.1:8787/healthz" -TimeoutSec 2 -UseBasicParsing).StatusCode -eq 200 } catch {}
    if ($ready) { break }
}
if ($ready) { Write-Host "" ; Write-Host "Gateway is UP (8787). Sidecar follows on 8790." }
else { Write-Host "WARNING: gateway not healthy yet - see output above / logs\oaiprism.err.log" }

if ($Desktop) {
    # The genuine Codex desktop app is the Microsoft Store package OpenAI.Codex
    # (Store-signed by OpenAI; its executable is ChatGPT.exe under WindowsApps).
    # Store apps cannot be started by exe path, only through their app ID.
    # Never fall back to look-alikes such as CodexX (unsigned, not OpenAI's).
    $pkg = Get-AppxPackage -Name OpenAI.Codex -ErrorAction SilentlyContinue |
        Where-Object { $_.SignatureKind -eq 'Store' } | Select-Object -First 1
    $appId = $null
    if ($pkg) {
        $appId = (Get-AppxPackageManifest $pkg).Package.Applications.Application |
            Where-Object { $_.Executable -like '*ChatGPT.exe' } |
            Select-Object -First 1 -ExpandProperty Id
    }
    if (-not $appId) {
        Write-Host "Codex desktop app (Store package OpenAI.Codex) not found - start it manually."
    } else {
        # It reads config.toml and OAI_PRISM_API_KEY only on startup, so an
        # instance that was already open keeps talking to the official backend.
        $running = Get-Process -Name ChatGPT -ErrorAction SilentlyContinue | Where-Object {
            $_.Path -and $_.Path.StartsWith($pkg.InstallLocation, [StringComparison]::OrdinalIgnoreCase)
        }
        if ($running) {
            Write-Host "Codex desktop app is already running and will NOT use the bridge until"
            Write-Host "it restarts: quit it completely (tray icon > Quit), then open it again."
        } else {
            Write-Host ("Launching Codex desktop app " + $pkg.Version + " ...")
            Start-Process ("shell:AppsFolder\" + $pkg.PackageFamilyName + "!" + $appId)
        }
    }
}

Write-Host ""
Write-Host "Keep this window OPEN. Stopping (Q+Enter / Ctrl+C / close window) will"
Write-Host "stop the proxy AND restore your original Codex config."

$saidReady = $false
try {
    while ($true) {
        if ($gw.HasExited) { break }
        # Q + Enter quits cleanly; drains one key when one is buffered.
        try {
            if ([Console]::KeyAvailable) {
                $k = [Console]::ReadKey($true)
                if ($k.Key -eq 'Q') { Write-Host "Q pressed - stopping session..."; break }
            }
        } catch { }
        if (-not $saidReady -and (Test-Port 8790)) {
            $saidReady = $true
            Write-Host "Sidecar is UP (8790). The bridge is ready - run 'codex' or use the desktop app."
        }
        Start-Sleep -Milliseconds 800
    }
} finally {
    Write-Host ""
    Write-Host "Stopping bridge session..."
    foreach ($p in @($gw, $sidecar)) {
        if ($p -and -not $p.HasExited) { taskkill /F /T /PID $p.Id 2>$null | Out-Null }
    }
    & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $Repo "tools\setup_codex_config.ps1") -Undo
    Write-Host "Bridge stopped. Codex config restored. Goodbye."
}
