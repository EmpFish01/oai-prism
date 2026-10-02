# Stop the local OAIprism started by start.ps1.
# Only stops a listener on the port whose process is oaiprism.exe.
param([int]$Port = 8787)

$conns = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
if (-not $conns) { Write-Host "Nothing is listening on port $Port."; exit 0 }

foreach ($id in ($conns | Select-Object -ExpandProperty OwningProcess -Unique)) {
    $p = Get-Process -Id $id -ErrorAction SilentlyContinue
    if ($p -and $p.ProcessName -eq "oaiprism") {
        Stop-Process -Id $id -Force
        Write-Host "Stopped oaiprism (PID $id)."
    } elseif ($p) {
        Write-Host "Port $Port belongs to $($p.ProcessName) (PID $id), not oaiprism; left alone."
    }
}
