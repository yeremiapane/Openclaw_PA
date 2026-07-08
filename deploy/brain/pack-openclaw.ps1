# pack-openclaw.ps1 — Jalankan DI WINDOWS (mesin dev) untuk mengemas ~/.openclaw
# menjadi openclaw-dump.tar.gz sebelum dimigrasi ke server.
#
# PENTING: hentikan dulu 'openclaw gateway' (dan proses openclaw lain) agar SQLite
# WAL (state/openclaw.sqlite-wal) ter-checkpoint & salinan konsisten.
#
# Setelah selesai: salin openclaw-dump.tar.gz ke server, lalu di server:
#   tar -xzf openclaw-dump.tar.gz          # menghasilkan folder .openclaw/
#   ./deploy/brain/migrate-openclaw.sh .openclaw ./data/openclaw

$ErrorActionPreference = "Stop"

$src = Join-Path $env:USERPROFILE ".openclaw"
$out = Join-Path (Get-Location) "openclaw-dump.tar.gz"

if (-not (Test-Path $src)) {
  Write-Error "Tidak menemukan $src"
}

$exclude = @(
  "--exclude=.openclaw/bin",
  "--exclude=.openclaw/node_modules",
  "--exclude=.openclaw/logs",
  "--exclude=.openclaw/tui",
  "--exclude=.openclaw/update-check.json",
  "--exclude=.openclaw/openclaw.json.bak",
  "--exclude=.openclaw/openclaw.json.bak.*"
)

Write-Host "Mengemas $src → $out (pastikan 'openclaw gateway' sudah berhenti)..."
# tar bawaan Windows 10/11. -C ke home agar arsip berisi folder '.openclaw/...'.
& tar -czf $out -C $env:USERPROFILE @exclude ".openclaw"

if ($LASTEXITCODE -ne 0) { Write-Error "tar gagal (exit $LASTEXITCODE)" }
Write-Host "Selesai: $out"
Write-Host "Salin ke server, ekstrak, lalu jalankan migrate-openclaw.sh (lihat header)."
