# Run Codex through the local OAIprism relay without touching ~/.codex/config.toml.
#
#   powershell -ExecutionPolicy Bypass -File packaging\windows\codex.ps1 [codex args...]
#   powershell -ExecutionPolicy Bypass -File packaging\windows\codex.ps1 exec "explain this repo"
#
# Everything is passed as `-c` overrides for this one Codex process, the same way
# excel-codex-bridge does it. The API key reaches Codex through the
# OAI_PRISM_API_KEY environment variable (provider env_key), scoped to this process.
# Override the defaults with OAI_PRISM_PORT / OAI_PRISM_CODEX_MODEL.
$ErrorActionPreference = "Stop"

$Root    = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$KeyFile = Join-Path $Root "secrets\api-key.txt"
$Port    = if ($env:OAI_PRISM_PORT) { $env:OAI_PRISM_PORT } else { "8787" }
$Model   = if ($env:OAI_PRISM_CODEX_MODEL) { $env:OAI_PRISM_CODEX_MODEL } else { "gpt-6.1-sol" }

$codex = Get-Command codex -ErrorAction SilentlyContinue
if (-not $codex) { throw "codex was not found on PATH." }
if (-not (Test-Path $KeyFile)) { throw "secrets\api-key.txt is missing. Run packaging\windows\start.ps1 first." }

try {
    Invoke-WebRequest -Uri "http://127.0.0.1:$Port/healthz" -TimeoutSec 2 -UseBasicParsing | Out-Null
} catch {
    throw "OAIprism is not running on port $Port. Run packaging\windows\start.ps1 first."
}

$env:OAI_PRISM_API_KEY = (Get-Content $KeyFile -Raw).Trim()

# Single-quoted TOML literals: Windows PowerShell 5.1 strips embedded double quotes
# from native-command arguments, but leaves single quotes alone.
$overrides = @(
    "-c", "model_provider='oaiprism'",
    "-c", "model_providers.oaiprism.name='OAIprism'",
    "-c", "model_providers.oaiprism.base_url='http://127.0.0.1:$Port/v1'",
    "-c", "model_providers.oaiprism.wire_api='responses'",
    "-c", "model_providers.oaiprism.env_key='OAI_PRISM_API_KEY'",
    "-c", "model='$Model'"
)

# Codex drops root-level -c values when a model subcommand gets its own, so put
# ours after the subcommand (same rule as excel-codex-bridge).
$sub = @("exec", "e", "review", "resume", "fork")
if ($args.Count -gt 0 -and $sub -contains $args[0]) {
    # Not `$rest = if (...) { ... }`: a statement's output unwraps a one-element
    # array into a bare string, and splatting a string passes one argument per
    # character (codex then fails with "unexpected argument 'e'" for "reply ...").
    $rest = @()
    if ($args.Count -gt 1) { $rest = @($args[1..($args.Count - 1)]) }
    & $codex.Source $args[0] @overrides @rest
} else {
    & $codex.Source @overrides @args
}
exit $LASTEXITCODE
