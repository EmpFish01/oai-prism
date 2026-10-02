# Configure Codex (CLI + Desktop) to route through the local OAIprism bridge.
#
#   powershell -ExecutionPolicy Bypass -File tools\setup_codex_config.ps1          # apply
#   powershell -ExecutionPolicy Bypass -File tools\setup_codex_config.ps1 -Undo    # revert
#
# What "apply" does (idempotent, safe to re-run):
#   1. Append a [model_providers.oaiprism] block to ~\.codex\config.toml.
#   2. Set the top-level model_provider = "oaiprism" so BOTH the Codex CLI and
#      the Codex desktop app (which cannot take -c overrides) use the bridge.
#      TOML requires top-level keys before the first [table], so the key is
#      inserted there, not appended.
#   3. Write the API key to the USER environment variable OAI_PRISM_API_KEY
#      (the provider's env_key). Desktop apps only see it after a restart.
#
# "Undo" surgically removes exactly those two edits plus the env variable.
# ASCII only in this file: Windows PowerShell 5.1 reads BOM-less .ps1 as ANSI.
param([switch]$Undo)
$ErrorActionPreference = "Stop"

$CodexHome = Join-Path $env:USERPROFILE ".codex"
$Config    = Join-Path $CodexHome "config.toml"
$Repo      = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$KeyFile   = Join-Path $Repo "secrets\api-key.txt"

if (-not (Test-Path $Config)) {
    Write-Host "    ~/.codex/config.toml not found - Codex not installed, skipping Codex setup."
    exit 0
}
if (-not (Test-Path $KeyFile)) {
    Write-Host "    secrets\api-key.txt missing - run packaging\windows\start.ps1 first. Skipping Codex setup."
    exit 0
}
$key = (Get-Content $KeyFile -Raw).Trim()

$ProviderHeader = '[model_providers.oaiprism]'
$ProviderBlock = @"

[model_providers.oaiprism]
name = "OAIprism"
base_url = "http://127.0.0.1:8787/v1"
wire_api = "responses"
env_key = "OAI_PRISM_API_KEY"
"@
$ProviderLine = 'model_provider = "oaiprism"'

if ($Undo) {
    $lines = [System.IO.File]::ReadAllLines($Config)
    $out = New-Object System.Collections.Generic.List[string]
    $inBlock = $false
    foreach ($line in $lines) {
        if ($line -eq $ProviderHeader) { $inBlock = $true; continue }
        if ($inBlock) {
            if ($line.StartsWith('[')) { $inBlock = $false } else { continue }
        }
        if ($line -match '^\s*model_provider\s*=') { continue }
        $out.Add($line)
    }
    [System.IO.File]::WriteAllLines($Config, $out)
    [Environment]::SetEnvironmentVariable('OAI_PRISM_API_KEY', $null, 'User')
    Write-Host "    Reverted: provider block + model_provider removed from config.toml, env var cleared."
    exit 0
}

# --- 1) provider block (append; [tables] may legally sit at the end) ---
$content = [System.IO.File]::ReadAllText($Config)
if ($content -notmatch 'model_providers\.oaiprism') {
    [System.IO.File]::AppendAllText($Config, $ProviderBlock)
    Write-Host "    config.toml: appended [model_providers.oaiprism] block."
} else {
    Write-Host "    config.toml: provider block already present, skipped."
}

# --- 2) top-level model_provider (must precede the first [table]) ---
$lines = [System.IO.File]::ReadAllLines($Config)
$existing = -1
for ($i = 0; $i -lt $lines.Count; $i++) {
    if ($lines[$i] -match '^\s*model_provider\s*=') { $existing = $i; break }
}
if ($existing -ge 0) {
    if ($lines[$existing] -ne $ProviderLine) {
        $lines[$existing] = $ProviderLine
        [System.IO.File]::WriteAllLines($Config, $lines)
        Write-Host "    config.toml: replaced existing model_provider line with oaiprism."
    } else {
        Write-Host "    config.toml: model_provider already oaiprism, skipped."
    }
} else {
    $insertAt = $lines.Count
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i].StartsWith('[')) { $insertAt = $i; break }
    }
    $out = New-Object System.Collections.Generic.List[string]
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($i -eq $insertAt) { $out.Add($ProviderLine) }
        $out.Add($lines[$i])
    }
    if ($insertAt -eq $lines.Count) { $out.Add($ProviderLine) }
    [System.IO.File]::WriteAllLines($Config, $out)
    Write-Host "    config.toml: set top-level model_provider = oaiprism (before first table)."
}

# --- 3) user env var (desktop apps read it after restart) ---
$currentUser = [Environment]::GetEnvironmentVariable('OAI_PRISM_API_KEY', 'User')
if ($currentUser -ne $key) {
    [Environment]::SetEnvironmentVariable('OAI_PRISM_API_KEY', $key, 'User')
    Write-Host "    User env var OAI_PRISM_API_KEY written (restart the Codex desktop app to pick it up)."
} else {
    Write-Host "    User env var OAI_PRISM_API_KEY already current, skipped."
}
