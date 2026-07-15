# Cara Mengganti Nomor SU & Nova

Panduan ini menjelaskan cara mengganti nomor WhatsApp **SU (Pak Sudianto)** dan
**Nova (Bu Nova)** kapan saja, dengan aman.

> Ringkas: nomor disimpan di file **`.env`** (bukan di kode). Saat gateway
> dinyalakan, nilai itu di-*seed* otomatis ke tabel `contacts`. Jadi mengganti
> nomor = ubah `.env` → temukan **LID** baru → restart gateway → hapus kontak
> lama.

---

## 1. Konsep penting: kenapa butuh 2 nilai (PHONE & LID)

Tiap kontak punya **dua** identitas yang dipakai sistem:

| Nilai | Contoh | Dipakai untuk |
|---|---|---|
| **PHONE** | `628970258733` | Tujuan **kirim** pesan/dokumen ke kontak (outbound). Untuk SU juga jadi target notifikasi & laporan. |
| **LID** | `5296382582977` | **Mengenali** pesan **masuk**. WhatsApp (engine GOWS/NOWEB) mengirim `from` sebagai `<lid>@lid`, bukan nomor asli. Gateway mencocokkan LID ini ke kontak. |

> ⚠️ **Kalau LID salah/kosong, pesan masuk dari nomor baru TIDAK akan dikenali**
> (SU baru tidak akan dianggap `su`, Nova baru tidak dianggap `semi_trusted`).
> Karena itu LID **wajib** ikut diganti, bukan cuma nomor telepon.

Format penulisan:
- PHONE: angka saja, kode negara tanpa `+` dan tanpa `0` di depan. Contoh
  `08xxxx` ditulis `628xxxx`.
- LID: **angka saja**, tanpa akhiran `@lid`. Contoh tulis `5296382582977`
  (bukan `5296382582977@lid`).

---

## 2. Langkah mengganti nomor

### Langkah A — Temukan LID nomor baru

LID tidak bisa ditebak; ia muncul saat nomor itu mengirim pesan ke bot.

1. Pastikan gateway sedang berjalan.
2. Dari **HP nomor baru**, kirim 1 pesan WhatsApp apa saja ke nomor bot
   (`085277603027`), misalnya `halo`.
3. Lihat log gateway (`d:\tmp\gw_liveNN.log`). Cari baris seperti:

   ```
   [INBOUND] session=default from=5296382582977@lid name="..." ...
   ```

   Angka sebelum `@lid` itulah **LID** nomor tersebut.
   (Kalau pesan dari nomor baru muncul sebagai `BLOCKED ... not_whitelisted`,
   itu normal — nomor itu memang belum di-whitelist; LID-nya tetap tercatat di
   baris log `from=...@lid`.)

> Alternatif: cek di database — `SELECT phone, lid, name, trust_level FROM contacts;`
> (Postgres host `:25432`, db `pa_ai`).

### Langkah B — Ubah `.env`

Buka file `.env` di root proyek, ubah 4 kunci ini sesuai kebutuhan:

```dotenv
SU_PHONE=628970258733       # nomor WhatsApp SU (tanpa + / 0 depan)
SU_LID=5296382582977        # LID SU dari Langkah A (tanpa @lid)
NOVA_PHONE=628197969041     # nomor WhatsApp Nova
NOVA_LID=3939206447269      # LID Nova dari Langkah A
```

> `.env` **tidak boleh** di-commit ke git. Cukup diedit di server.

### Langkah C — Restart gateway agar `.env` terbaca & di-seed ulang

Gateway membaca `.env` dan menjalankan *seed* kontak **hanya saat start**. Jadi
setelah mengubah `.env`, **restart prosesnya** (tidak perlu build ulang —
kodenya tidak berubah):

```bash
# 1) hentikan proses gateway yang berjalan
#    (cari & stop api-gateway.exe)
# 2) jalankan lagi
nohup ./api-gateway.exe > /d/tmp/gw_live_new.log 2>&1 &
```

Pada log start akan muncul konfirmasi:

```
[db] seed kontak trusted selesai (SU=628... lid=..., Nova=628... lid=...)
```

Pastikan nilainya sudah sesuai nomor baru.

### Langkah D — Hapus / nonaktifkan kontak LAMA (penting untuk keamanan)

Proses seed meng-*upsert* **berdasarkan PHONE**. Artinya:

- Kalau Bapak hanya mengganti **LID** (nomor PHONE tetap sama) → baris kontak
  yang sama diperbarui. **Tidak ada sisa.** Selesai di Langkah C.
- Kalau Bapak mengganti **PHONE** ke nomor lain → seed membuat **baris baru**,
  sedangkan **kontak lama tetap ada** dengan trust `su`/`semi_trusted`. Kontak
  lama itu **masih dipercaya** sistem. Demi keamanan, **hapus** kontak lama:

  ```bash
  curl -X DELETE \
    -H "X-Admin-Key: $ADMIN_API_KEY" \
    http://localhost:4000/admin/contacts/<NOMOR_LAMA>
  ```

  Ganti `<NOMOR_LAMA>` dengan PHONE lama (mis. `628970258733`).
  (`ADMIN_API_KEY` ada di `.env`.)

  Verifikasi sisa kontak:

  ```bash
  curl -H "X-Admin-Key: $ADMIN_API_KEY" http://localhost:4000/admin/contacts
  ```

---

## 3. Verifikasi setelah ganti

1. **Pesan masuk dikenali** — dari HP nomor baru, kirim pesan ke bot. Di log
   harus muncul `trust=su` (untuk SU) atau `trust=semi_trusted` (untuk Nova),
   **bukan** `not_whitelisted`.
2. **Pesan keluar sampai** — minta bot mengirim sesuatu ke nomor baru (mis. SU
   minta laporan/dokumen) dan pastikan diterima di WhatsApp nomor baru.

---

## 4. Checklist singkat

- [ ] Dapatkan LID nomor baru dari log `[INBOUND] from=...@lid` (Langkah A).
- [ ] Ubah `SU_PHONE`/`SU_LID` dan/atau `NOVA_PHONE`/`NOVA_LID` di `.env`.
- [ ] Restart `api-gateway.exe`; cek baris `[db] seed kontak trusted selesai`.
- [ ] Jika **PHONE** berubah: hapus kontak lama via `DELETE /admin/contacts/<lama>`.
- [ ] Verifikasi inbound (`trust=su`/`semi_trusted`) & outbound ke nomor baru.

---

## 5. Catatan

- Nomor **bot** (akun WhatsApp yang men-scan QR, `085277603027`) berbeda dari
  ini; menggantinya berarti pairing ulang QR, di luar cakupan dokumen ini.
- Tabel `contacts` memberi `lid` constraint **UNIQUE** — satu LID tak bisa
  dipakai dua kontak aktif. Jadi pastikan LID lama sudah lepas (kontak lama
  dihapus) sebelum LID itu dipakai kontak lain.
- Trust level: SU = `su` (akses penuh, target laporan/dokumen), Nova =
  `semi_trusted` (koordinasi venue, dll). Seed menetapkan ini otomatis; jangan
  ubah manual kecuali memang diinginkan.
