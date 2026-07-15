# Deployment "brain" (Opsi A) — OpenClaw + api-gateway dalam satu container

Bundel ini men-*dockerize* **OpenClaw milik Anda sendiri** (terisolasi dari OpenClaw
base yang sudah ada di server) berikut **api-gateway** (Go), dalam **satu container
"brain"**, lalu menyatukannya dengan stack utama (Postgres, WAHA, monitoring).

## Kenapa satu container (bukan dua)?

api-gateway memanggil OpenClaw sebagai **subprocess CLI lokal**
(`openclaw agent --agent … --session-key … --message … --json`, lihat
[client.go](../../services/api-gateway/src/openclaw/client.go#L297-L312)) — **bukan**
lewat HTTP/WS. `OPENCLAW_URL` di config bersifat *legacy/tak dipakai*. Karena itu
binary `openclaw` **harus** berada di tempat proses Go berjalan. Colocate = jalur
paling kokoh & paling sederhana. `openclaw gateway` tetap dijalankan (loopback) sbg
server sesi persisten yang di-*inject* oleh CLI.

```
                    host:4000  (drop-in, dulu gateway native)
                         │
   WAHA ───hook────────▶ │           ┌─────────── container: pa_ai_brain ───────────┐
   (pa_ai_waha)          └──────────▶│  api-gateway (:4000)                          │
                                     │        │  exec: openclaw agent (subprocess)   │
   Prometheus ─scrape──▶ host:4000 ──│        ▼                                      │
   (host.docker.internal)            │  openclaw gateway (127.0.0.1:18789, loopback) │
                                     │        │                                      │
                                     │  volume: /home/claw/.openclaw  (state persist)│
                                     └───────────────────────────────────────────────┘
        brain → postgres:5432 · redis:6379 · waha:3000  (nama service, jaringan compose)
```

## Peta file

| File                         | Guna                                                                                       |
| ---------------------------- | ------------------------------------------------------------------------------------------ |
| `Dockerfile`               | Multi-stage: build Go → runtime Ubuntu + OpenClaw CLI + binary + entrypoint               |
| `entrypoint.sh`            | Semai state → jalankan`openclaw gateway` → tunggu siap → `api-gateway` (supervisor) |
| `docker-compose.brain.yml` | Overlay: menambah service`brain` ke stack utama                                          |
| `migrate-openclaw.sh`      | (Server) restore dump`~/.openclaw` + rewrite path + chown                                |
| `pack-openclaw.ps1`        | (Windows) kemas`~/.openclaw` → `openclaw-dump.tar.gz`                                 |

## Apa yang TETAP, BARU, dan DIGANTI di server

Deployment ini **menambah**, bukan mengganti struktur folder. Tak ada yang perlu dihapus.

| | Komponen | Keterangan |
|---|---|---|
| **TETAP** | OpenClaw **base** server (mis. `~/Personal-Assistant/`) | Tak disentuh. Brain terisolasi: image, volume, & jaringan compose sendiri. |
| **TETAP** | `docker-compose.yml`, `monitoring/`, `services/` | Tanpa edit. WAHA hook & Prometheus tetap menunjuk `host.docker.internal:4000`. |
| **TETAP** | `data/postgres` | Data DB. Bila sudah ada, jangan diapa-apakan. |
| **BARU** | `deploy/brain/` | Bundel ini. |
| **BARU** | `data/openclaw` | State OpenClaw hasil migrasi (lihat bawah). |
| **DIGANTI** | Proses gateway di `host:4000` | Dulu api-gateway native; kini container `brain`. **Hanya prosesnya**, bukan file. |

Satu-satunya yang perlu dimatikan: gateway native lama di port 4000 (bila jalan), agar
brain bisa mengambil alih port itu. OpenClaw base di port lain tidak diganggu.

## Prasyarat

- Repo ini ter-*clone* di server (mis. `~/Personal-Assistant/Openclaw_PA`).
- **Akses Docker tanpa sudo**: `sudo usermod -aG docker $USER && newgrp docker`.
  (Tanpa ini: `permission denied ... /var/run/docker.sock`. Alternatif: awali semua
  perintah dengan `sudo` — konsisten satu cara, jangan campur.)
- `.env` root sudah terisi. **Salin dari template**: `cp .env.example .env` lalu isi.
  `.env.example` adalah daftar var yang otoritatif (selaras dengan `config.go`).
- Versi Go builder di `Dockerfile` (`GO_IMAGE`) ≥ toolchain `go.mod` (`go 1.26.2`).

## Postgres: skema otomatis, data tidak

- **Skema** dibuat otomatis. [`main.go`](../../services/api-gateway/main.go#L33) memanggil
  `store.Migrate(ctx)` saat start, dan
  [`postgres.go`](../../services/api-gateway/src/db/postgres.go#L24) memuat `CREATE TABLE
  IF NOT EXISTS` untuk semua tabel. Postgres kosong → skema terbentuk sendiri saat brain
  menyala. (Berkas `sql/*.sql` adalah peninggalan lama, **tidak** dipakai.)
- **Data tidak ikut.** DB baru = whitelist kosong, tanpa kontak SU/Nova, tanpa riwayat
  meeting/profil. Untuk membawa data dari mesin lama, restore **sebelum** brain menyala
  agar tak bentrok dengan auto-migrate:
  ```bash
  # di mesin lama:
  docker exec pa_ai_postgres pg_dump -U pa_ai -d pa_ai --clean --if-exists > pa_ai_dump.sql
  # di server:
  docker compose up -d postgres                       # postgres SAJA dulu
  cat pa_ai_dump.sql | docker exec -i pa_ai_postgres psql -U pa_ai -d pa_ai
  docker compose -f docker-compose.yml -f deploy/brain/docker-compose.brain.yml up -d --build
  ```

> `DB_PASS` **terkunci** saat volume DB pertama kali dibuat. Pastikan benar sejak awal;
> mengubahnya kemudian tidak berpengaruh tanpa reset volume.

## Menjalankan

Selalu **dari root repo**:

```bash
docker compose -f docker-compose.yml -f deploy/brain/docker-compose.brain.yml up -d --build
```

Untuk produksi, **pin versi OpenClaw** (hindari upgrade diam-diam yang bisa mengubah
skema `state/openclaw.sqlite`): buka `docker-compose.brain.yml`, aktifkan
`build.args.OPENCLAW_VERSION`.

### Kenapa tidak ada perubahan pada file lain (nol-inkonsistensi)

`brain` menerbitkan `4000:4000` → menjadi **drop-in** pengganti gateway native di
`host:4000`. WAHA hook (`WHATSAPP_HOOK_URL=http://host.docker.internal:4000/...`) dan
target scrape Prometheus (`host.docker.internal:4000`) **tetap** valid tanpa diedit.
Alert `GatewayDown` (`up{job="api-gateway"}==0`) juga tetap akurat.

> Jika hairpin `host.docker.internal → host:4000` bermasalah di server Anda, alternatif
> lebih langsung: set WAHA hook ke `http://brain:4000/webhook/waha` dan target Prometheus
> ke `brain:4000` (jaringan internal). Keduanya opsional; default drop-in sudah jalan.

## Override environment (host → dalam container)

`.env` root tetap **satu-satunya** sumber rahasia. Overlay hanya menimpa nilai
**jaringan** (bukan rahasia), karena `localhost` di dalam container ≠ host:

| Var            | `.env` (host, native)    | `brain` (dalam container) |
| -------------- | -------------------------- | --------------------------- |
| `DB_HOST`    | `localhost`              | `postgres`                |
| `DB_PORT`    | `25432`                  | `5432`                    |
| `WAHA_URL`   | `http://localhost:13000` | `http://waha:3000`        |
| `REDIS_ADDR` | `localhost:6379`         | `redis:6379`              |

Sisanya (rahasia + `SU_PHONE`, `WHITELIST_MODE`, `MS_GRAPH_*`, `ALERT_*`, dll.) dibaca
langsung dari `.env`. Precedence Compose: `environment` > `env_file`.

`ANTHROPIC_API_KEY` **tidak** dibutuhkan: model dilayani OpenClaw via `claude-cli`
(OAuth), bukan API key langsung.

## Migrasi state OpenClaw (Windows → server)

State yang dipindah: `openclaw.json`, `identity/`, `devices/`, `state/openclaw.sqlite*`
(sesi), `agents/`, `memory/`, `workspaces/` (orchestrator, pa_communicator, support),
`workspace/`, `skills/`, `plugin-skills/`. Binary Linux **tidak** ikut — datang dari image.

1. **Di Windows** — hentikan `openclaw gateway` lebih dulu (agar WAL ter-checkpoint), lalu:
   ```powershell
   ./deploy/brain/pack-openclaw.ps1        # → openclaw-dump.tar.gz
   ```
2. **Salin** `openclaw-dump.tar.gz` ke server, lalu ekstrak di root repo:
   ```bash
   tar -xzf openclaw-dump.tar.gz            # → folder .openclaw/
   ```
3. **Restore + rewrite path + chown** (script mengubah path absolut Windows
   `C:\Users\…\.openclaw` → `/home/claw/.openclaw` di `openclaw.json`, lalu chown uid 1000):
   ```bash
   ./deploy/brain/migrate-openclaw.sh .openclaw ./data/openclaw
   ```
4. **Jalankan** stack (perintah di atas), lalu **verifikasi auth** (lihat bawah).

Ganti lokasi state via `OPENCLAW_STATE_DIR` di `.env` (mis. path absolut di luar repo).

## Auth OpenClaw (claude-cli / OAuth) — WAJIB diverifikasi

Provider model = `claude-cli` mode `oauth`. Token OAuth mungkin **tidak** berpindah
mulus antar-mesin. Setelah migrasi, uji satu turn:

```bash
docker exec -it pa_ai_brain \
  openclaw agent --agent pa_communicator --session-key migrasi:test --message 'ping' --json
```

Jika **error auth**, login ulang di dalam container (sesi tersimpan ke volume, jadi
sekali saja):

```bash
docker exec -it pa_ai_brain bash
# di dalam: ikuti perintah login OpenClaw/claude-cli Anda (device-code/OAuth), lalu keluar
```

## Operasi WAHA: login QR & mendapatkan LID

Session WhatsApp **tidak ikut migrasi** (`.waha` tak dibawa) — di server perlu **scan QR
ulang**. Session tersimpan persisten di `./.waha`, jadi cukup sekali.

**Login via dashboard** (termudah): buka `http://SERVER_IP:13000/dashboard`, login dengan
`WAHA_DASHBOARD_USERNAME`/`WAHA_DASHBOARD_PASSWORD`, buka session `default` → scan QR dari
WhatsApp → **Perangkat Tertaut → Tautkan Perangkat**.

**Login via CLI** (tanpa browser):
```bash
curl -X POST http://SERVER_IP:13000/api/sessions/default/start -H "X-Api-Key: $WAHA_API_KEY"
curl -s http://SERVER_IP:13000/api/sessions/default -H "X-Api-Key: $WAHA_API_KEY"   # tunggu SCAN_QR_CODE
curl -s "http://SERVER_IP:13000/api/default/auth/qr?format=image" \
  -H "X-Api-Key: $WAHA_API_KEY" --output qr.png                                     # salin & scan
```
Status berubah jadi `WORKING` bila tersambung.

**Mendapatkan LID.** Pesan masuk bisa datang sbg `xxxx@lid` (ID privasi), bukan nomor asli.
Minta kontak mengirim satu pesan, lalu baca log:
```bash
docker logs --tail 100 pa_ai_brain | grep -iE "lid|from|webhook"
```
Gateway otomatis me-*resolve* `@lid` → nomor via WAHA GOWS lalu mencocokkannya dengan
whitelist by-phone, jadi cukup baca hasilnya di log. Nilai LID untuk SU/Nova diisikan ke
`SU_LID`/`NOVA_LID` di `.env`.

## Log & verifikasi

```bash
docker logs -f pa_ai_brain      # gateway OpenClaw siap → api-gateway :4000 → Migrate OK
docker logs -f pa_ai_waha       # koneksi WhatsApp
curl -s localhost:4000/metrics | head
```

## Volume & seeding (jebakan "volume menutupi binary")

Install OpenClaw menaruh berkas di `~/.openclaw`; bila volume kosong di-mount ke sana,
ia bisa menutupi binary. Solusinya (di image): hasil install disnapshot ke
`/opt/openclaw-skel`. Saat boot, `entrypoint.sh`:

- **volume kosong** → semai penuh dari skeleton;
- **ada state migrasi** → pakai apa adanya, hanya segarkan dir **runtime** dari skeleton
  (sehingga state Windows yang tanpa binary Linux tetap jalan, dan upgrade image tetap
  terpakai).

Dir runtime yang disegarkan: **`bin/` + `tools/` + `node_modules/`**.

> **`tools/` wajib ikut.** `bin/openclaw` hanyalah _shim_ yang meng-exec
> `tools/node/bin/node` (Node.js bundled). Dump dari Windows **tidak** memuat `tools/`
> versi Linux, jadi bila ia tak disalin dari skeleton, OpenClaw mati dengan
> `tools/node/bin/node: No such file or directory` → `openclaw gateway` gagal start →
> container masuk restart loop. Entrypoint memasang jaring pengaman: `openclaw --version`
> diuji lebih dulu agar gagal cepat dengan pesan jelas.
>
> Catatan uji: **named volume** kosong diisi otomatis oleh Docker dari isi image sehingga
> **menyembunyikan** masalah ini. **Bind-mount** (yang dipakai di sini) tidak. Jangan
> menguji dengan named volume lalu menyimpulkan aman.

### Drift versi OpenClaw

Tanpa `OPENCLAW_VERSION` di-pin, image selalu menarik versi **terbaru** (build terakhir:
`2026.7.1`). Bila state yang dimigrasikan berasal dari versi lebih lama (mis. `2026.6.8`),
`state/openclaw.sqlite` dibuka oleh versi lebih baru dan berpotensi ter-migrasi **satu
arah**. Untuk produksi: pin `build.args.OPENCLAW_VERSION` di `docker-compose.brain.yml`
dan backup `data/openclaw` sebelum upgrade.

## Keamanan

- OpenClaw gateway **loopback-only** (`127.0.0.1:18789`) & **tak di-EXPOSE** — hanya
  api-gateway sekontainer yang mengaksesnya. Lebih ketat dari rencana awal (yang
  menerbitkan 8180/18780).
- `openclaw.json` memuat **rahasia** (`gateway.auth.token`) → berada di volume state,
  **jangan** commit ke git.
- OpenClaw **tanpa** blok `channels` → tidak konek WhatsApp langsung (WA murni via Go
  gateway + WAHA), sesuai aturan isolasi.
- Rahasia hanya lewat `.env` (di-`env_file`), tak pernah di image/compose. Container
  jalan sbg non-root (uid 1000).
- Isolasi dari OpenClaw base server: image + volume + jaringan compose sendiri; gateway
  internal tak berebut port host.

## Monitoring

Tanpa perubahan. Prometheus tetap men-scrape `host.docker.internal:4000` (kini = brain);
`gateway_*` metrics, dashboard, & alert M1–M5 + keamanan berlaku apa adanya. `/internal/alerts`
tetap butuh `ALERT_WEBHOOK_TOKEN` di `.env`.

## Backup

- **Postgres** — `./data/postgres` (atau `pg_dump`). Komponen paling kritis.
- **State OpenClaw** — kini stateful: backup `./data/openclaw` (khususnya
  `state/openclaw.sqlite` = seluruh sesi). Hentikan brain / checkpoint WAL sebelum salin.

## Rollback ke gateway native

Matikan brain, jalankan gateway seperti semula di host:

```bash
docker compose -f docker-compose.yml -f deploy/brain/docker-compose.brain.yml stop brain
# lalu jalankan api-gateway native (run.ps1) — WAHA hook & Prometheus host:4000 tak berubah.
```

## Troubleshooting

| Gejala                        | Kemungkinan                                                                        |
| ----------------------------- | ---------------------------------------------------------------------------------- |
| `permission denied ... /var/run/docker.sock` | User belum masuk grup docker → `sudo usermod -aG docker $USER && newgrp docker` |
| `bash: ./migrate-openclaw.sh: Permission denied` | Bit exec hilang saat clone → jalankan `bash ./deploy/brain/migrate-openclaw.sh ...` atau `chmod +x deploy/brain/*.sh` |
| `unable to prepare context: path ... not found` | Compose menunjuk folder build yang tak ada — pastikan repo sudah `git pull` terbaru (service mati pra-Fase 9 telah dihapus) |
| `COPY services/api-gateway/go.mod: not found` saat build | `go.mod`/`go.sum` tak ter-track di git — pastikan `.gitignore` tidak mengabaikannya |
| `tar: Ignoring unknown extended header 'SCHILY.fflags'` | **Aman diabaikan** — metadata BSD-tar Windows yang tak dikenal GNU tar; isi arsip tetap utuh |
| Healthcheck brain unhealthy   | api-gateway gagal start — cek`docker logs pa_ai_brain` (DB/WAHA/env)            |
| `parse_error`/turn kosong   | Sesi OpenClaw terkontaminasi — reset session-key (mekanisme epoch bawaan gateway) |
| Error auth model              | Login ulang claude-cli/OAuth di dalam container (lihat "Auth OpenClaw")            |
| OpenClaw tak bisa tulis state | Bind-mount bukan milik uid 1000 →`sudo chown -R 1000:1000 ./data/openclaw`      |
| Agent "workspace tak ada"     | `openclaw.json` belum di-rewrite path → jalankan ulang `migrate-openclaw.sh`  |
