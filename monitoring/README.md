# Monitoring PA AI (Prometheus + Grafana)

Pilar: **API & Agents** (M1), **Infrastruktur** (M2), **Keamanan** (M3),
**Alert email** (M3b), **Operasional & Kesehatan** (M4),
**Agent/LLM, Pengiriman & Worker** (M5).

## Akses

- Grafana: http://localhost:13001 (admin / `GRAFANA_ADMIN_PASSWORD` di `.env`)
- Prometheus: http://localhost:19090
- Dashboard ter-provision otomatis di folder **PA AI**.

## Arsitektur singkat

- **Gateway** berjalan NATIVE di host (port 4000). Prometheus men-scrape lewat
  `host.docker.internal:4000/metrics`.
- Dua datasource Grafana: **Prometheus** (metrik operasional) dan **PA Postgres**
  (telemetri agent yang sudah tercatat di `agent_executions` dkk).

## Menyalakan

```bash
docker compose up -d prometheus grafana \
  postgres-exporter redis-exporter cadvisor
```

Setelah mengubah `prometheus.yml`, muat ulang config:

```bash
curl -X POST http://localhost:19090/-/reload
```

## windows_exporter (WAJIB untuk CPU/RAM fisik host)

Host adalah Windows; CPU/RAM/disk fisik host **tidak** bisa dipantau container.
Pasang `windows_exporter` sebagai service Windows native:

1. Unduh MSI rilis dari https://github.com/prometheus-community/windows_exporter/releases
2. Pasang (default mendengarkan di `:9182`), aktifkan kolektor inti:
   ```powershell
   msiexec /i windows_exporter-x.y.z-amd64.msi ENABLED_COLLECTORS=cpu,cs,logical_disk,memory,net,os,system
   ```
3. Verifikasi: `curl http://localhost:9182/metrics`
4. Prometheus sudah punya job `windows-host` → target `host.docker.internal:9182`.
   Jika belum dipasang, target ini "down" tanpa mengganggu yang lain.

## cAdvisor

Metrik per-container. Di Docker Desktop/WSL2 sifatnya **best-effort**; bila
container `cadvisor` gagal start, cukup hentikan — dashboard host & DB tetap jalan.

## Alerting via email (Fase M3b)

Alur: Prometheus → Alertmanager → webhook gateway `/internal/alerts` → email (MS Graph)
ke `ALERT_EMAIL_TO` (default `yeremia.yosefan@hypernet.co.id`).

Aktifkan:

1. Tetapkan token bersama (nilai bebas, rahasia) di **dua** tempat yang harus sama:
   - `.env`: `ALERT_WEBHOOK_TOKEN=<token>` (opsional `ALERT_EMAIL_TO=<tujuan>`)
   - file token Alertmanager (ter-gitignore):
     ```bash
     printf '%s' '<token>' > monitoring/alertmanager/webhook_token
     ```

   Jika `ALERT_WEBHOOK_TOKEN` kosong, endpoint `/internal/alerts` **nonaktif** (503).
2. Rebuild+restart gateway native (agar endpoint & token terbaca).
3. Nyalakan Alertmanager + reload Prometheus:
   ```bash
   docker compose up -d alertmanager prometheus
   ```
4. Uji: `http://localhost:19093` (Alertmanager UI). Email terkirim saat ada alert firing;
   isi email di-escape (konten alert tak bisa menyuntik HTML).

Endpoint `/internal/alerts` dilindungi Bearer token (`Authorization: Bearer <token>`,
perbandingan constant-time). Email dikirim asinkron agar Alertmanager tak menunggu Graph.

## Operasional & Kesehatan (Fase M4)

Dashboard **PA AI — Operasional & Kesehatan** (`pa-operations`) memantau keadaan
LIVE sistem: uptime gateway, status target scrape (up/down), **status sesi WhatsApp
(WAHA)**, jumlah tugas terjadwal per status, dan approval yang menunggu SU.

Gateway mengekspos gauge baru (di-refresh kolektor tiap 30 dtk, bukan per-request):

- `gateway_waha_session_up` — 1 bila sesi WAHA `WORKING`, 0 selain itu.
- `gateway_waha_reachable` — 1 bila endpoint WAHA terhubung, 0 bila tak terjangkau.
- `gateway_scheduled_tasks{status}` — jumlah tugas terjadwal per status
  (pending/fired/error/cancelled).
- `gateway_pending_approvals` — jumlah pesan keluar yang menunggu keputusan SU.

Uptime memakai `process_start_time_seconds` bawaan client_golang
(`time() - process_start_time_seconds{job="api-gateway"}`).

Alert operasional (grup `operasional` di `alerts.yml`): **SesiWahaDown**,
**WahaTakTerjangkau** (keduanya critical, 3 mnt), **TugasTerjadwalError** (warning),
**ApprovalMenumpuk** (>10 selama >2 jam). Semua diteruskan ke email lewat Alertmanager
(M3b) — tanpa konfigurasi tambahan.

Tidak perlu langkah deploy khusus: rebuild gateway native (agar gauge & kolektor
aktif) lalu reload Prometheus untuk `alerts.yml`; dashboard ter-provision otomatis.

## Agent/LLM, Pengiriman & Worker (Fase M5)

Panel ditambahkan ke dashboard **PA AI — API & Agents** (`pa-api-agents`), dua baris baru:
*Agent — Kualitas & Keandalan* dan *Pengiriman Keluar, Worker & Pool DB*.

Metrik Prometheus baru (dicatat dari satu titik tunggal tiap alur):

- `gateway_agent_calls_total{agent,outcome}` — giliran agent per hasil
  (ok/no_reply/empty/parse_error/error). Sumber deteksi **kontaminasi sesi OpenClaw**.
- `gateway_agent_duration_seconds{agent}` — histogram durasi giliran agent (untuk p95).
- `gateway_agent_tokens_total{type}` — token per jenis (input/output/cache_read/cache_write).
- `gateway_agent_fallbacks_total`, `gateway_agent_refusals_total` — flag fallback & refusal.
- `gateway_outbound_messages_total{kind,status}` — pesan keluar per jenis & status
  (sent/failed/held).
- `gateway_worker_last_run_timestamp_seconds{worker}` — detak worker latar
  (scheduler / email_watcher / health_collector); basi = worker mati.
- `gateway_db_pool_connections{state}` — pool koneksi pgx (acquired/idle/total/max).

Alert baru (grup `agent` + tambahan di `operasional`): **LonjakanParseError**,
**LonjakanFallbackModel**, **AgentLatensiTinggi**, **PemakaianTokenHarianTinggi**,
**PengirimanKeluarGagal**, **WorkerLatarBasi**. Semua diteruskan ke email (M3b).
Ambang token harian bersifat indikatif — sesuaikan di `alerts.yml` dengan anggaran nyata.

Aktivasi: rebuild gateway native + reload Prometheus (`docker compose up -d prometheus`).
Dashboard ter-provision otomatis.

## Mode whitelist WhatsApp (strict / open)

Diatur `WHITELIST_MODE` di `.env` (default `strict`):

- **`strict`** — hanya kontak whitelist yang dilayani; nomor lain diblokir.
- **`open`** — semua penelepon otomatis di-whitelist sbg `external` & dilayani
  `pa_communicator`. SU & Nova tetap dari `SU_PHONE`/`NOVA_PHONE` (di-seed).
  Nomor yang diblokir admin (`external_contacts.status='blocked'`) tetap ditolak.

Perubahan mode butuh rebuild+restart gateway native.

### Block / unblock cepat (mode open)

Endpoint admin (butuh header `X-Admin-Key`):

```bash
# Blokir sebuah nomor (cabut whitelist + tandai blocked utk @c.us dan @lid sekaligus)
curl -X POST http://localhost:4000/admin/block \
  -H "X-Admin-Key: $ADMIN_API_KEY" -H 'Content-Type: application/json' \
  -d '{"phone":"628123456789","notes":"spam"}'

# Batalkan blokir (mode open: pesan berikutnya akan auto-whitelist ulang)
curl -X POST http://localhost:4000/admin/unblock \
  -H "X-Admin-Key: $ADMIN_API_KEY" -H 'Content-Type: application/json' \
  -d '{"phone":"628123456789"}'
```

Tersedia juga `POST /admin/external/:identifier/block` dan `.../unblock` bila ingin
menarget satu identifier mentah (mis. `628123456789@c.us`).

### Panel & alert

Dashboard **PA AI — Keamanan** baris *Auto-whitelist & Whitelist (Mode Open)*:
jumlah auto-whitelist (rentang & laju), total whitelist aktif, total terblokir,
serta **tabel kontak whitelist** dan **tabel nomor terblokir**. Metrik pendukung:
`gateway_security_events_total{kind="auto_whitelisted"}`. Alert **LonjakanAutoWhitelist**
(grup `keamanan`) menyala bila >30 auto-whitelist / 15 mnt (indikasi spam).

## Catatan keamanan

- `/metrics`, `/internal/alerts`, dan port exporter **jangan** diekspos ke internet
  (hanya jaringan lokal / Prometheus/Alertmanager). Exporter di compose sengaja
  **tidak** mem-publish port ke host.
- Token webhook & password DB/Grafana lewat `.env`/file ter-gitignore, bukan hardcode.
