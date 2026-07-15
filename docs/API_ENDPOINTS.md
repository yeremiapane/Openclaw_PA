# Dokumentasi Endpoint API Gateway

Layanan **api-gateway** (Go + Gin) adalah satu-satunya komponen yang mengekspos HTTP.
Semua trafik WhatsApp masuk lewat sini, lalu diteruskan ke agent OpenClaw, dan seluruh
operasi pengelolaan/observasi dilakukan lewat Admin API.

- **Base URL (dari host):** `http://localhost:4000`
- **Port:** env `GATEWAY_PORT` (default `4000`)
- **Format:** seluruh request/response berbentuk JSON (`Content-Type: application/json`)
- **Konvensi balasan webhook:** endpoint `/webhook/*` SELALU membalas `200 OK` walau pesan
  ditolak (blocked/rate-limited/injection). Ini disengaja agar WAHA tidak retry. Status
  sebenarnya ada di field `status` pada body.

> Catatan port: WAHA diakses gateway di `:13000` (host), bukan `:3000` (ter-reserve WinNAT).
> Postgres host `:25432`. Nilai-nilai ini ada di `.env`, bukan di kode.

---

## Ringkasan Semua Endpoint

| Method | Path | Auth | Fungsi |
|--------|------|------|--------|
| GET | `/health` | — | Health check + status sesi WAHA |
| POST | `/webhook/waha` | chain WA | Pesan masuk dari WAHA (jalur utama) |
| POST | `/webhook/openclaw-output` | — (internal) | Push balasan agent ke WhatsApp |
| GET | `/admin/contacts` | X-Admin-Key | Daftar kontak whitelist |
| POST | `/admin/contacts` | X-Admin-Key | Tambah kontak whitelist |
| PATCH | `/admin/contacts/:phone` | X-Admin-Key | Ubah kontak whitelist |
| DELETE | `/admin/contacts/:phone` | X-Admin-Key | Soft-delete kontak whitelist |
| GET | `/admin/external` | X-Admin-Key | Daftar kontak external (belum whitelist) |
| GET | `/admin/external/:identifier` | X-Admin-Key | Detail external + riwayat akses |
| PATCH | `/admin/external/:identifier` | X-Admin-Key | Perkaya profil external |
| DELETE | `/admin/external/:identifier` | X-Admin-Key | Soft-delete external |
| POST | `/admin/external/:identifier/block` | X-Admin-Key | Blokir external |
| POST | `/admin/external/:identifier/promote` | X-Admin-Key | Promosi external → whitelist |
| GET | `/admin/logs` | X-Admin-Key | Audit log akses |
| GET | `/admin/approvals` | X-Admin-Key | Daftar approval gate |
| POST | `/admin/approvals/:id/approve` | X-Admin-Key | Setujui approval (kirim pesan tertahan) |
| POST | `/admin/approvals/:id/reject` | X-Admin-Key | Tolak approval |
| GET | `/admin/executions` | X-Admin-Key | Trace eksekusi agent (token/model/durasi) |
| GET | `/admin/usage` | X-Admin-Key | Agregat token per conversation |
| GET | `/admin/outbound` | X-Admin-Key | Jejak pesan keluar (terkirim/tertahan) |
| GET | `/admin/meetings` | X-Admin-Key | Daftar meeting |
| GET | `/admin/meetings/:id/history` | X-Admin-Key | Riwayat perubahan satu meeting |
| POST | `/admin/meetings/:id/resend-rsvp` | X-Admin-Key | Kirim ulang email undangan (RSVP) tanpa buat ulang event |

**Auth:** semua route `/admin/*` butuh header `X-Admin-Key: <ADMIN_API_KEY>`
(perbandingan constant-time). Bila `ADMIN_API_KEY` kosong di env, seluruh Admin API
mengembalikan `503` (dinonaktifkan total).

---

## 1. Health & Webhook

### `GET /health`
Cek hidup-tidaknya gateway sekaligus status sesi WAHA. Tanpa auth.

```powershell
Invoke-RestMethod http://localhost:4000/health
```
Respons:
```json
{ "status": "ok", "service": "api-gateway", "waha_session": "WORKING" }
```
`waha_session` = `"unreachable"` bila gateway tak bisa menghubungi WAHA.

---

### `POST /webhook/waha` — jalur utama pesan masuk
Dipanggil oleh **WAHA** setiap ada event WhatsApp. **Tidak dipanggil manual**; ini
target webhook yang dikonfigurasi di WAHA. Melewati security chain Fase 4:

```
Auth (whitelist + catat external) → RateLimit (Redis) → Sanitize (anti prompt-injection + potong panjang) → WahaInbound
```

Body yang diharapkan (subset penting dari payload WAHA):
```json
{
  "event": "message",
  "session": "default",
  "payload": {
    "id": "false_628xxx@c.us_3EB0...",
    "timestamp": 1719628800,
    "from": "628970258733@c.us",
    "fromMe": false,
    "body": "Tolong atur meeting dengan Rakha besok jam 10",
    "_data": { "key": { "remoteJidAlt": "628970258733@s.whatsapp.net" } }
  }
}
```

Perilaku middleware (semua tetap balas `200`):
- `event` ≠ `message` atau `fromMe: true` → `{"status":"ignored"}`
- pengirim tak ada di whitelist → dicatat sebagai external, `{"status":"blocked"}`
- external berstatus `blocked` → `{"status":"blocked"}` (reason `external_blocked`)
- melebihi rate limit → `{"status":"rate_limited"}`
- terdeteksi prompt injection → `{"status":"blocked_injection"}`
- lolos → diproses agent, `{"status":"received"}` (atau `{"status":"approval_command"}`
  bila body berupa perintah `SETUJU`/`TOLAK` dari SU)

Penanganan `@lid`: bila `from` berbentuk `@lid` (privacy ID), gateway memetakannya ke
nomor asli via `remoteJidAlt` dan menyimpan `lid` ke kontak agar lookup berikutnya cocok.

---

### `POST /webhook/openclaw-output` — jalur internal
Jalur alternatif untuk mem-*push* balasan agent ke WhatsApp (tanpa security chain WA).
Jalur utama Fase 6 sebenarnya shell-out sinkron di dalam `WahaInbound`; endpoint ini
disediakan bila ada komponen lain yang ingin mengirim hasil agent.

Body:
```json
{
  "agentId": "pa_communicator",
  "conversationId": "agent:pa_communicator:628xxxx",
  "targetContact": "628780327623@c.us",
  "response": "Halo Pak Rakha, ..."
}
```
`targetContact` & `response` **wajib**. Respons sukses `{"status":"sent"}`; gagal kirim ke
WAHA → `502 {"error":"gagal kirim ke WhatsApp"}`.

> Keamanan: endpoint ini tanpa auth dan langsung mengirim WA — pastikan tidak terekspos
> ke jaringan publik (hanya untuk pemanggil internal di host yang sama).

---

## 2. Admin API — Whitelist Contacts

Semua butuh header `X-Admin-Key`. Contoh variabel header (PowerShell):
```powershell
$h = @{ "X-Admin-Key" = "pa_ai_dev_admin_key_change_me" }
```

### `GET /admin/contacts`
Daftar kontak whitelist. Query: `include_deleted=true` untuk ikut menampilkan yang
sudah soft-delete.
```powershell
Invoke-RestMethod "http://localhost:4000/admin/contacts" -Headers $h
Invoke-RestMethod "http://localhost:4000/admin/contacts?include_deleted=true" -Headers $h
```

### `POST /admin/contacts`
Tambah kontak ke whitelist. Body (`ContactInput`):

| Field | Wajib | Keterangan |
|-------|-------|------------|
| `phone` | ✅ | MSISDN tanpa `+`/`@c.us`, mis. `628970258733` |
| `lid` | — | Privacy ID WhatsApp (isi bila tahu, agar lookup `@lid` cocok) |
| `name` | — | Nama tampilan |
| `company` | — | Perusahaan |
| `email` | — | Email (dipakai undangan kalender) |
| `address` | — | Alamat |
| `trust_level` | — | `su` \| `semi_trusted` \| `external` (default `external`) |
| `tags` | — | array string |
| `profile` | — | objek JSON bebas |
| `notes` | — | catatan |

`trust_level` menentukan agent yang menangani: `su`→orchestrator, `semi_trusted`→support,
`external`→pa_communicator.

```powershell
$body = @{ phone="628900000002"; name="Budi"; company="PT Contoh"; trust_level="external" } | ConvertTo-Json
Invoke-RestMethod -Method Post "http://localhost:4000/admin/contacts" -Headers $h -ContentType "application/json" -Body $body
```
Sukses `201 {"contact": {...}}`. Bila phone/lid sudah ada → `409 {"error":"phone/lid sudah terdaftar"}`.

### `PATCH /admin/contacts/:phone`
Ubah kontak berdasarkan nomor di path. Body sama dengan `ContactInput` (field yang diisi
saja yang diubah).
```powershell
$body = @{ name="Budi Santoso"; lid="58299986788591" } | ConvertTo-Json
Invoke-RestMethod -Method Patch "http://localhost:4000/admin/contacts/628900000002" -Headers $h -ContentType "application/json" -Body $body
```

### `DELETE /admin/contacts/:phone`
Soft-delete (data tetap tersimpan untuk audit — Postgres tak boleh kehilangan data).
```powershell
Invoke-RestMethod -Method Delete "http://localhost:4000/admin/contacts/628900000002" -Headers $h
```
Respons `{"status":"soft_deleted","phone":"628900000002"}`.

---

## 3. Admin API — External Contacts

Kontak yang pernah mengirim pesan tetapi belum di-whitelist otomatis tercatat di sini
(untuk pemantauan/mitigasi).

### `GET /admin/external`
Query: `status` (mis. `pending`, `blocked`), `limit` (default `100`).
```powershell
Invoke-RestMethod "http://localhost:4000/admin/external?status=pending&limit=50" -Headers $h
```

### `GET /admin/external/:identifier`
Detail satu external + 50 access log terakhirnya. `:identifier` boleh nomor atau `@lid`.
```powershell
Invoke-RestMethod "http://localhost:4000/admin/external/628900000099" -Headers $h
```
Respons: `{"contact": {...}, "access_logs": [...]}`.

### `PATCH /admin/external/:identifier`
Perkaya profil external (`ExternalInput`): `display_name`, `phone`, `email`, `company`,
`address`, `status`, `risk_score` (integer), `tags`, `profile`, `notes`.

### `DELETE /admin/external/:identifier`
Soft-delete external.

### `POST /admin/external/:identifier/block`
Blokir external (pesan berikutnya ditolak `external_blocked`). Body opsional `{"notes":"..."}`.
```powershell
Invoke-RestMethod -Method Post "http://localhost:4000/admin/external/628900000099/block" -Headers $h -ContentType "application/json" -Body '{"notes":"spam"}'
```

### `POST /admin/external/:identifier/promote`
Pindahkan external ke whitelist. Body opsional `ContactInput` untuk melengkapi
`name`/`trust_level`/dll saat promosi.
```powershell
$body = @{ name="Rakha"; trust_level="external" } | ConvertTo-Json
Invoke-RestMethod -Method Post "http://localhost:4000/admin/external/628780327623/promote" -Headers $h -ContentType "application/json" -Body $body
```
Respons `{"status":"promoted","contact":{...}}`.

---

## 4. Admin API — Audit Log

### `GET /admin/logs`
Query: `decision` (mis. `blocked`, `rate_limited`, `injection_blocked`, `allowed`),
`identifier`, `limit` (default `100`).
```powershell
Invoke-RestMethod "http://localhost:4000/admin/logs?decision=blocked&limit=100" -Headers $h
```

---

## 5. Admin API — Approval Gate (Fase 8)

Pesan keluar ke pihak eksternal yang butuh persetujuan SU ditahan sebagai *approval*.
Biasanya SU menyetujui via WhatsApp (`SETUJU N`), tetapi endpoint ini menyediakan jalur
admin/HTTP yang setara.

### `GET /admin/approvals`
Query: `status` (mis. `pending`), `limit` (default `50`).
```powershell
Invoke-RestMethod "http://localhost:4000/admin/approvals?status=pending" -Headers $h
```

### `POST /admin/approvals/:id/approve`
Setujui approval `:id`. Ini menjalankan logika yang sama dengan SU menyetujui: mengirim
pesan tertahan ke pihak eksternal dan — bila approval tertaut meeting — membuat event
kalender + email undangan.
```powershell
Invoke-RestMethod -Method Post "http://localhost:4000/admin/approvals/12/approve" -Headers $h
```
Respons `{"status":"approved","message":"..."}`. ID tak ada → `404`.

### `POST /admin/approvals/:id/reject`
Tolak approval `:id` (pesan tertahan dibuang). Respons `{"status":"rejected","message":"..."}`.

---

## 6. Admin API — Observability & Evaluasi (Fase 8.5)

### `GET /admin/executions`
Trace per giliran agent: token, model, durasi, actions, outcome.
Query: `conversation` (id percakapan, mis. `agent:orchestrator:628970258733`), `limit` (50).
```powershell
Invoke-RestMethod "http://localhost:4000/admin/executions?conversation=agent:orchestrator:628970258733" -Headers $h
```

### `GET /admin/usage`
Agregat token per conversation (untuk evaluasi biaya/efisiensi model).
Query: `conversation`, `limit` (100).

### `GET /admin/outbound`
Jejak pesan yang dikirim/ditahan bot. Query: `conversation`, `limit` (50).

### `GET /admin/meetings`
Daftar meeting. Query: `status` (mis. `pending`, `scheduled`, `cancelled`,
`superseded`, `completed`, `rejected`), `limit` (50).
```powershell
Invoke-RestMethod "http://localhost:4000/admin/meetings?status=pending" -Headers $h
```

### `GET /admin/meetings/:id/history`
Riwayat perubahan satu meeting (audit reschedule/cancel/finalize).
```powershell
Invoke-RestMethod "http://localhost:4000/admin/meetings/7/history" -Headers $h
```

### `POST /admin/meetings/:id/resend-rsvp`
Kirim **ulang** email undangan (RSVP) ke peserta eksternal dari detail meeting yang
sudah tersimpan (`attendeeEmail`, `calendarLink`, `teamsLink`, waktu, venue). Bersifat
**idempoten untuk pemulihan**: tidak membuat ulang event kalender, tidak mengirim ulang
konfirmasi WhatsApp — hanya email undangannya. Berguna saat approval SU sukses membuat
event + konfirmasi WA, tetapi pengiriman email gagal (mis. error render template).

```powershell
Invoke-RestMethod -Method Post "http://localhost:4000/admin/meetings/10/resend-rsvp" -Headers $h
```
Respons sukses `{"status":"ok","meeting_id":10}`. Bila meeting tak punya
`attendeeEmail` tersimpan → `500 {"error":"..."}`; ID tak ada → `500`.

> Latar: template email hanya di-*parse* saat render (bukan saat build), sehingga
> kesalahan sintaks bisa lolos kompilasi dan baru muncul saat kirim nyata. Regresi
> semacam ini kini dijaga oleh `TestRenderAllTemplates`, dan endpoint ini menjadi jalur
> pemulihan bila sebuah email undangan terlanjur gagal terkirim.

---

## Kode Status Umum

| Kode | Arti |
|------|------|
| `200` | Sukses (juga dipakai webhook untuk pesan yang ditolak — lihat field `status`) |
| `201` | Kontak baru dibuat |
| `400` | Payload/parameter tidak valid |
| `401` | `X-Admin-Key` salah/absen |
| `404` | Resource tidak ditemukan (kontak/approval/meeting) |
| `409` | Konflik (phone/lid sudah terdaftar) |
| `500` | Error internal (DB dll) |
| `502` | Gagal meneruskan ke WAHA (openclaw-output) |
| `503` | Admin API dinonaktifkan (`ADMIN_API_KEY` kosong) |

---

## Catatan Keamanan

- **Jangan ekspos `/webhook/openclaw-output` & `/admin/*` ke publik.** Yang pertama tanpa
  auth dan langsung mengirim WhatsApp; yang kedua hanya dilindungi satu header statis.
- `ADMIN_API_KEY` saat ini masih nilai dev (`pa_ai_dev_admin_key_change_me`) —
  **wajib diganti sebelum produksi**.
- Semua kredensial (WAHA key, admin key, MS Graph secret, DB pass) hanya dari `.env` /
  environment, tidak boleh di-hardcode.
