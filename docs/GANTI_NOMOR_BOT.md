# Cara Mengganti Nomor Bot (Akun WhatsApp yang Men-scan QR)

Panduan ini menjelaskan cara mengganti **nomor WhatsApp bot** — yaitu akun yang
di-*pair* ke sistem lewat scan QR (saat ini `085277603027` / `6285277603027`).
Berbeda dari [Ganti Nomor SU & Nova](GANTI_NOMOR_SU_NOVA.md): SU/Nova adalah
kontak yang **diajak bicara** bot; nomor bot adalah **identitas bot itu sendiri**.

> Ringkas: nomor bot **tidak** disimpan di `.env` maupun di kode — ia hanya
> "siapa pun akun yang sedang ter-*pair* di WAHA". Jadi menggantinya = **logout
> sesi WAHA yang lama → scan QR dengan HP nomor baru**. Tidak perlu build ulang,
> tidak perlu ubah kode.

---

## 0. Yang PERLU & TIDAK perlu disiapkan

- ✅ **HP dengan nomor WhatsApp baru** yang akan jadi bot, siap untuk scan QR.
- ✅ Akses ke dashboard WAHA: `http://localhost:13000` (kredensial di `.env`,
  kunci `WAHA_DASHBOARD_USERNAME` / `WAHA_DASHBOARD_PASSWORD`).
- ❌ **Tidak** perlu mengubah `.env` (selain hal opsional di bagian 4).
- ❌ **Tidak** perlu build/deploy ulang `api-gateway.exe`.
- ❌ **Tidak** perlu mengubah `docker-compose.yml` — nama sesi tetap `default`,
  port tetap `13000`, hook tetap `.../webhook/waha`.

> ⚠️ Gateway mengabaikan pesan `fromMe` dan tidak pernah memakai nomor bot
> sebagai logika apa pun — jadi mengganti nomor bot tidak menyentuh whitelist,
> approval, maupun data meeting.

---

## 1. Konsep: di mana "identitas bot" tersimpan

- Autentikasi akun WhatsApp bot disimpan WAHA di volume **`./.waha`**
  (lihat `docker-compose.yml`: `- ./.waha:/app/.sessions`). Inilah yang membuat
  bot tetap ter-*pair* walau container restart (tak perlu scan QR tiap kali).
- Untuk berganti akun, autentikasi lama harus **di-logout** dulu, baru akun baru
  di-scan. Kalau langsung scan tanpa logout, sesi masih memegang akun lama.

---

## 2. Langkah mengganti nomor bot (via Dashboard — cara termudah)

### Langkah A — Buka dashboard WAHA
Buka `http://localhost:13000`, login dengan kredensial dari `.env`.

### Langkah B — Logout sesi `default` (lepas akun lama)
Pada sesi **`default`**, klik **Stop** lalu **Logout** (melepas pairing akun
lama). Setelah logout, status sesi menjadi tidak ter-*pair*.

> Ini **tidak** menghapus konfigurasi sesi, whitelist, atau database —
> hanya melepaskan akun WhatsApp yang lama.

### Langkah C — Start & scan QR dengan HP nomor baru
1. Klik **Start** pada sesi `default`; status akan menjadi **SCAN_QR_CODE**.
2. QR muncul di dashboard (auto-refresh — paling andal).
3. Di **HP nomor baru**: WhatsApp → **Perangkat Tertaut** → **Tautkan Perangkat**
   → scan QR di layar.
4. Tunggu status sesi menjadi **WORKING**.

### Langkah D — (Bila HP lama masih menampilkan bot sebagai perangkat tertaut)
Di HP **nomor lama**, buka WhatsApp → Perangkat Tertaut → hapus tautan perangkat
bot, supaya nomor lama benar-benar lepas.

---

## 2b. Alternatif: via API (tanpa dashboard)

> Catatan: nama endpoint siklus-sesi bisa sedikit berbeda antar versi WAHA. Bila
> ragu, **dashboard (bagian 2) adalah acuan** — atau buka Swagger UI di
> `http://localhost:13000` (login kredensial dashboard) untuk melihat endpoint
> persis versi Anda. Endpoint `.../start` & `.../auth/qr` di bawah sudah
> terverifikasi pada instalasi ini.

Semua perintah butuh header `X-Api-Key: $WAHA_API_KEY` (nilai di `.env`).
Base URL host = `http://localhost:13000`.

```bash
KEY="$WAHA_API_KEY"   # ambil dari .env

# 1) Logout akun lama (lepas pairing)
curl -X POST -H "X-Api-Key: $KEY" http://localhost:13000/api/sessions/default/logout

# 2) Start ulang → masuk mode scan QR
curl -X POST -H "X-Api-Key: $KEY" http://localhost:13000/api/sessions/default/start

# 3) Ambil QR (buka di browser lebih mudah — auto-refresh):
#    http://localhost:13000  → sesi default
#    atau tarik gambar QR:
curl -s -H "X-Api-Key: $KEY" http://localhost:13000/api/default/auth/qr -o /d/tmp/bot-qr.png

# 4) Pantau status hingga WORKING
curl -s -H "X-Api-Key: $KEY" http://localhost:13000/api/sessions/default | grep -o '"status":"[^"]*"'
```

Scan `/d/tmp/bot-qr.png` (atau QR di dashboard) dari HP nomor baru, lalu pastikan
status `WORKING`.

---

## 3. Verifikasi setelah ganti nomor bot

1. **Bot online** — status sesi `default` = **WORKING** (dashboard atau
   `GET /api/sessions/default`). Cek juga health gateway:
   ```bash
   curl -s http://localhost:4000/health
   # harapkan: {"service":"api-gateway","status":"ok","waha_session":"WORKING"}
   ```
2. **Inbound jalan** — dari HP **SU**, kirim pesan ke **nomor bot BARU**. Di log
   gateway (`d:\tmp\gw_liveNN.log`) harus muncul `[INBOUND] ... trust=su`, dan bot
   membalas. (Ingatkan SU & kontak lain bahwa nomor bot berganti.)
3. **Outbound jalan** — minta bot mengirim sesuatu (mis. SU minta laporan) dan
   pastikan terkirim dari nomor bot yang baru.

---

## 4. Penting: cek pengenalan SU & Nova (LID) setelah ganti nomor bot

Sistem mengenali pesan **masuk** lewat **LID** (lihat
[Ganti Nomor SU & Nova](GANTI_NOMOR_SU_NOVA.md)). LID sebuah kontak umumnya
**stabil** dan tidak berubah hanya karena nomor bot berganti — jadi biasanya
`SU_LID`/`NOVA_LID` di `.env` **tetap valid**.

Namun untuk memastikan, lakukan cek cepat:

1. Dari HP **SU**, kirim 1 pesan ke bot baru → di log harus `trust=su`
   (bukan `not_whitelisted`).
2. Dari HP **Nova**, kirim 1 pesan → harus `trust=semi_trusted`.

Bila salah satunya malah **`BLOCKED ... not_whitelisted`**, berarti LID-nya
berbeda di akun bot baru. Perbaiki dengan mengikuti
[Ganti Nomor SU & Nova](GANTI_NOMOR_SU_NOVA.md) **Langkah A–C**: ambil LID baru
dari baris log `[INBOUND] from=...@lid`, isi ke `SU_LID`/`NOVA_LID` di `.env`,
lalu restart gateway.

> Catatan: kontak eksternal (yang di-*whitelist* via nomor) tetap aman —
> gateway kini otomatis meresolusikan `@lid → nomor` lewat WAHA saat pesan masuk,
> sehingga balasan mereka tetap dikenali walau LID-nya baru.

---

## 5. Checklist singkat

- [ ] Siapkan HP nomor bot baru.
- [ ] Dashboard `:13000` → sesi `default`: **Stop → Logout** (lepas akun lama).
- [ ] **Start** → **scan QR** dari HP nomor baru → tunggu **WORKING**.
- [ ] Hapus perangkat tertaut bot di HP nomor **lama** (opsional, kebersihan).
- [ ] Verifikasi health gateway = `waha_session: WORKING`.
- [ ] Uji inbound (SU→bot baru, `trust=su`) & outbound.
- [ ] Cek pengenalan SU & Nova; bila `not_whitelisted`, perbarui LID via
      [GANTI_NOMOR_SU_NOVA.md](GANTI_NOMOR_SU_NOVA.md).

---

## 6. Catatan

- **Riwayat chat lama tidak ikut pindah.** Mengganti nomor bot = akun WhatsApp
  yang berbeda; percakapan WhatsApp lama di akun lama tidak muncul di akun baru.
  Namun **memori & data sistem** (kontak, meeting, approval di PostgreSQL) **tetap
  utuh** — tak tersentuh oleh pergantian ini.
- **Beri tahu semua kontak** (SU, Nova, pihak eksternal) bahwa nomor bot berubah,
  karena mereka harus mengirim ke nomor baru.
- Nomor bot **tidak** perlu (dan sebaiknya tidak) ditulis di `.env`/kode; sistem
  tidak membutuhkannya. Jangan tambahkan variabel `BOT_PHONE` apa pun.
- Jangan hapus/utak-atik isi volume `./.waha` secara manual saat container jalan —
  gunakan **Logout** agar WAHA membersihkan sesi dengan benar.
