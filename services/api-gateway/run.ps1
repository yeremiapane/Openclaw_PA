$ErrorActionPreference = "Stop"
$dir = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $dir

$existing = Get-NetTCPConnection -LocalPort 4000 -ErrorAction SilentlyContinue
if ($existing) {
    $pids = $existing.OwningProcess | Select-Object -Unique
    foreach ($procId in $pids) {
        Write-Output "Stopping process on port 4000 (PID $procId)"
        Stop-Process -Id $procId -Force -ErrorAction SilentlyContinue
    }
    Start-Sleep -Seconds 1
}

Write-Output "Building..."
go build -o bin\api-gateway.exe .
if (-not $?) { Write-Output "BUILD FAILED"; exit 1 }

Remove-Item d:\tmp\gw_live.log -Force -ErrorAction SilentlyContinue
Remove-Item d:\tmp\gw_live_err.log -Force -ErrorAction SilentlyContinue
$p = Start-Process -FilePath ".\bin\api-gateway.exe" -WorkingDirectory $dir -RedirectStandardOutput "d:\tmp\gw_live.log" -RedirectStandardError "d:\tmp\gw_live_err.log" -NoNewWindow -PassThru
Write-Output "Started PID: $($p.Id)"

Start-Sleep -Seconds 3
Write-Output "---health---"
try { Invoke-RestMethod -Uri "http://localhost:4000/health" -TimeoutSec 5 } catch { Write-Output "health check failed: $_" }
Write-Output "---stderr (tail)---"
Get-Content d:\tmp\gw_live_err.log -Tail 10 -ErrorAction SilentlyContinue
