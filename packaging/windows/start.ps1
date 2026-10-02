# Start OAIprism locally (loopback only) in the background.
#
#   powershell -ExecutionPolicy Bypass -File packaging\windows\start.ps1 [-Port 8787]
#
# The API key is read from secrets\api-key.txt (created on first run) and handed
# to the service through OAI_PRISM_API_KEYS_FILE, so it never appears in the
# config file or on a command line. Logs go to logs\oaiprism.log.
param([int]$Port = 8787)
$ErrorActionPreference = "Stop"

$Root    = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$Exe     = Join-Path $Root "bin\oaiprism.exe"
$Config  = Join-Path $Root "configs\config.yaml"
$KeyFile = Join-Path $Root "secrets\api-key.txt"
$LogDir  = Join-Path $Root "logs"

if (Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue) {
    Write-Host "Port $Port is already in use (OAIprism may already be running)."
    exit 0
}

if (-not (Test-Path $Exe)) {
    $go = Get-Command go -ErrorAction SilentlyContinue
    $goExe = if ($go) { $go.Source } else { Join-Path $Root "..\.toolchain\go\bin\go.exe" }
    if (-not (Test-Path $goExe)) { throw "bin\oaiprism.exe is missing and no Go toolchain was found." }
    Write-Host "Building bin\oaiprism.exe ..."
    Push-Location $Root
    try { & $goExe build -trimpath -ldflags "-s -w" -o $Exe ./cmd/oaiprism } finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
}

if (-not (Test-Path $Config)) {
    throw "configs\config.yaml is missing. Copy configs\config.example.yaml and set server.host to 127.0.0.1."
}

if (-not (Test-Path $KeyFile)) {
    New-Item -ItemType Directory -Force (Split-Path $KeyFile) | Out-Null
    $bytes = New-Object byte[] 24
    [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    $key = "sk-oaiprism-" + (($bytes | ForEach-Object { $_.ToString("x2") }) -join "")
    [IO.File]::WriteAllText($KeyFile, $key + "`n")
    Write-Host "Generated a new API key in secrets\api-key.txt"
}

New-Item -ItemType Directory -Force $LogDir | Out-Null
$env:OAI_PRISM_API_KEYS_FILE = $KeyFile

# Working directory must be the repo root: the dashboard is served from web\dist.
$proc = Start-Process -FilePath $Exe `
    -ArgumentList @("serve", "-config", $Config, "-port", $Port) `
    -WorkingDirectory $Root `
    -RedirectStandardOutput (Join-Path $LogDir "oaiprism.log") `
    -RedirectStandardError (Join-Path $LogDir "oaiprism.err.log") `
    -WindowStyle Hidden -PassThru

$ok = $false
for ($i = 0; $i -lt 20; $i++) {
    Start-Sleep -Milliseconds 500
    if ($proc.HasExited) { break }
    try {
        $ok = (Invoke-WebRequest -Uri "http://127.0.0.1:$Port/healthz" -TimeoutSec 2 -UseBasicParsing).StatusCode -eq 200
    } catch {}
    if ($ok) { break }
}

if (-not $ok) {
    Write-Host "OAIprism did not become healthy. See logs\oaiprism.log and logs\oaiprism.err.log"
    exit 1
}
Write-Host "OAIprism is running (PID $($proc.Id))"
Write-Host "  API:       http://127.0.0.1:$Port/v1   (Bearer key: secrets\api-key.txt)"
Write-Host "  Dashboard: http://127.0.0.1:$Port/dashboard/"
Write-Host "  Ready:     http://127.0.0.1:$Port/readyz  (503 until an account is imported)"
