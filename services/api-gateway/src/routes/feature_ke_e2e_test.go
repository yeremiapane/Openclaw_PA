//go:build integration

// Integration (E2E) test untuk bundel fitur K → E terhadap Postgres ASLI, memakai kode
// terkompilasi yang sama seperti gateway — TANPA mengirim WhatsApp, TANPA memanggil MS
// Graph live, dan TANPA menembak orchestrator (jalur push sengaja tidak dipicu):
//
//   - Fitur K  : rekomendasi venue dari riwayat meeting (buildVenueRecommendations).
//   - Fitur K2 : penjadwalan digest kalender (scheduleCalendarDigest) + format agenda.
//   - Fitur E  : email watch (watchEmail/cancelWatch/snapshot, pra-saring, deteksi balasan,
//     majunya last_seen, dan format instruksi laporan poin-poin).
//
// Semua baris yang dibuat memakai penanda (marker) ber-namespace dan dibersihkan di akhir.
// Jalankan:
//
//	go test -tags integration ./src/routes/ -run TestFeatureKtoE -v
package routes

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"pa-ai/api-gateway/src/config"
	"pa-ai/api-gateway/src/db"
	"pa-ai/api-gateway/src/model"
	"pa-ai/api-gateway/src/services"
	"pa-ai/api-gateway/src/waha"
)

// keE2EFixture menyiapkan Store + pool mentah + Handler (SU ke nomor bogus) untuk ketiga
// sub-test. Services diisi dari kredensial .env agar gerbang Services.Enabled() lolos —
// tetapi tak ada panggilan jaringan yang dipicu di jalur yang diuji.
func keE2EFixture(t *testing.T) (*Handler, *pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	_ = godotenv.Load("../../../../.env")
	cfg := config.Load()

	store, err := db.NewStore(ctx, cfg)
	if err != nil {
		t.Fatalf("koneksi Postgres gagal: %v", err)
	}
	// email_watches adalah tabel baru — pastikan skema ada (idempoten).
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate skema gagal: %v", err)
	}
	pool, err := pgxpool.New(ctx, cfg.DBConnString())
	if err != nil {
		t.Fatalf("pool mentah gagal: %v", err)
	}

	svc := services.New(cfg.MSGraphTenantID, cfg.MSGraphClientID, cfg.MSGraphClientSecret,
		cfg.MSGraphUserUPN, cfg.MSGraphRefreshToken, cfg.MailFromName, cfg.SignaturePath)

	h := &Handler{
		Store:    store,
		Waha:     waha.New(cfg.WahaURL, cfg.WahaAPIKey, cfg.WahaSession),
		Services: svc,
		SUPhone:  "628100000001",
	}
	return h, pool, ctx
}

// ─────────────────────────────── Fitur K: rekomendasi venue ───────────────────────────────

func TestFeatureKtoE_VenueRecommendations(t *testing.T) {
	h, pool, ctx := keE2EFixture(t)
	defer h.Store.Close()
	defer pool.Close()

	ts := time.Now().Format("20060102T150405.000")
	extAlpha := "ITEST-K-Alpha-" + ts
	venKantor := "Kantor Alpha " + ts
	venCafe := "Cafe Beta " + ts

	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM meeting_requests WHERE external_name LIKE $1`, "ITEST-K-%")
	}()

	seed := func(ext, venue, status string) {
		_, err := pool.Exec(ctx, `
			INSERT INTO meeting_requests (agent_id, requested_via, external_name, meeting_type, venue, status)
			VALUES ('pa_communicator','external',$1,'offline',$2,$3)`, ext, venue, status)
		if err != nil {
			t.Fatalf("seed meeting gagal: %v", err)
		}
	}
	// Kantor Alpha dipakai 2x (paling sering), Cafe Beta 1x — semua dengan pihak yang sama.
	seed(extAlpha, venKantor, "scheduled")
	seed(extAlpha, venKantor, "completed")
	seed(extAlpha, venCafe, "scheduled")

	// 1. Riwayat dengan pihak yang SAMA → cabang "PERNAH dipakai", Kantor Alpha di depan.
	rec := h.buildVenueRecommendations(ctx, extAlpha)
	if !strings.Contains(rec, "PERNAH dipakai dengan "+extAlpha) {
		t.Fatalf("#1: rekomendasi tak menyebut riwayat pihak sama: %q", rec)
	}
	if !strings.Contains(rec, venKantor) {
		t.Fatalf("#1: rekomendasi tak memuat venue tersering %q: %q", venKantor, rec)
	}
	if idxK, idxC := strings.Index(rec, venKantor), strings.Index(rec, venCafe); idxC != -1 && idxK > idxC {
		t.Fatalf("#1: urutan salah — venue tersering harus di depan: %q", rec)
	}

	// 2. Pihak TANPA riwayat → jatuh ke cabang cadangan "sering dipakai" (DB punya data).
	rec2 := h.buildVenueRecommendations(ctx, "ITEST-K-TanpaRiwayat-"+ts)
	if !strings.Contains(rec2, "sering dipakai") {
		t.Fatalf("#2: fallback FrequentVenues tak aktif: %q", rec2)
	}

	// 3. Nama eksternal kosong & tak ada apa pun → aman (string apa saja, tak panik).
	_ = h.buildVenueRecommendations(ctx, "")

	t.Logf("OK Fitur K: riwayat pihak-sama diutamakan, fallback frequent aktif")
}

// ─────────────────────────────── Fitur K2: digest kalender ───────────────────────────────

func TestFeatureKtoE_CalendarDigest(t *testing.T) {
	h, pool, ctx := keE2EFixture(t)
	defer h.Store.Close()
	defer pool.Close()

	ts := time.Now().Format("20060102T150405.000")
	noteSU := "ITEST-K2-" + ts + " susun rencana kerja"
	noteEXT := "ITEST-K2EXT-" + ts + " coba tembus gerbang"

	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM scheduled_tasks WHERE note LIKE $1 OR note LIKE $2`,
			"ITEST-K2-%", "ITEST-K2EXT-%")
	}()

	suContact := &model.Contact{Phone: h.SUPhone, TrustLevel: "su", Name: "Pak Sudianto (SU)"}
	extContact := &model.Contact{Phone: "628100000009", TrustLevel: "external"}

	// 1. SU menjadwalkan digest BERULANG harian (recurTime dipakai, ReminderTime kosong).
	h.scheduleCalendarDigest(ctx, suContact, model.Action{
		Type: "SCHEDULE_CALENDAR_DIGEST", RecurKind: "daily", RecurTime: "07:30",
		ReminderNote: noteSU, ReminderLabel: "digest pagi",
	})

	var (
		kind, recurKind, recurTime, createdBy, status string
		fireAt                                        time.Time
	)
	err := pool.QueryRow(ctx, `
		SELECT kind, COALESCE(recur_kind,'none'), COALESCE(recur_time,''), COALESCE(created_by,''), status, fire_at
		FROM scheduled_tasks WHERE note = $1 ORDER BY id DESC LIMIT 1`, noteSU).Scan(
		&kind, &recurKind, &recurTime, &createdBy, &status, &fireAt)
	if err != nil {
		t.Fatalf("#1: digest tidak tersimpan: %v", err)
	}
	if kind != "calendar_digest" {
		t.Fatalf("#1: kind salah = %q (ingin calendar_digest)", kind)
	}
	if recurKind != "daily" || recurTime != "07:30" {
		t.Fatalf("#1: rekurensi salah: kind=%q time=%q", recurKind, recurTime)
	}
	if createdBy != "su" || status != "pending" {
		t.Fatalf("#1: created_by/status salah: %q/%q", createdBy, status)
	}
	if !fireAt.After(time.Now().Add(-2 * time.Minute)) {
		t.Fatalf("#1: fire_at tidak masuk akal: %s", fireAt)
	}

	// 2. Gerbang SU-only: non-SU tidak boleh menjadwalkan digest.
	h.scheduleCalendarDigest(ctx, extContact, model.Action{
		Type: "SCHEDULE_CALENDAR_DIGEST", RecurKind: "daily", RecurTime: "09:00", ReminderNote: noteEXT,
	})
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM scheduled_tasks WHERE note = $1`, noteEXT).Scan(&n)
	if n != 0 {
		t.Fatalf("#2: non-SU berhasil membuat digest (gerbang bocor): %d baris", n)
	}

	// 3. formatCalendarAgenda: kosong → penanda tanpa agenda; berisi → memuat subjek.
	if got := formatCalendarAgenda(nil); !strings.Contains(got, "Tidak ada agenda") {
		t.Fatalf("#3: agenda kosong salah format: %q", got)
	}
	evs := []services.AvailabilityEvent{
		{Subject: "Rapat Direksi", Start: "2026-07-06T09:00:00.0000000", End: "2026-07-06T10:00:00.0000000", Location: "R. Rapat 1"},
		{Subject: "Sinkron Tim", Start: "2026-07-06T13:00:00.0000000", End: "2026-07-06T13:30:00.0000000", IsOnline: true},
	}
	ag := formatCalendarAgenda(evs)
	if !strings.Contains(ag, "Rapat Direksi") || !strings.Contains(ag, "Sinkron Tim") {
		t.Fatalf("#3: agenda tak memuat kedua event: %q", ag)
	}

	// 4. buildCalendarDigestInstruction saat kalender nonaktif tetap kirim instruksi + fokus.
	hNoSvc := &Handler{Store: h.Store, SUPhone: h.SUPhone} // Services nil → Enabled()=false
	instr := hNoSvc.buildCalendarDigestInstruction(ctx, model.ScheduledTask{Kind: "calendar_digest", Note: noteSU})
	if !strings.Contains(instr, "DIGEST KALENDER TERJADWAL") || !strings.Contains(instr, noteSU) {
		t.Fatalf("#4: instruksi digest tak lengkap: %q", instr)
	}

	t.Logf("OK Fitur K2: digest berulang tersimpan, gerbang SU-only, format agenda benar")
}

// ─────────────────────────────── Fitur E: email watch ───────────────────────────────

func TestFeatureKtoE_EmailWatch(t *testing.T) {
	h, pool, ctx := keE2EFixture(t)
	defer h.Store.Close()
	defer pool.Close()

	ts := time.Now().Format("20060102T150405.000")
	label := "ITEST-E-" + ts
	labelB := "ITEST-E-gate-" + ts

	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM email_watches WHERE label LIKE $1`, "ITEST-E-%")
	}()

	suContact := &model.Contact{Phone: h.SUPhone, TrustLevel: "su", Name: "Pak Sudianto (SU)"}
	extContact := &model.Contact{Phone: "628100000009", TrustLevel: "external"}

	// 1. SU membuat pantauan email → tersimpan active dengan pra-saring.
	h.watchEmail(ctx, suContact, model.Action{
		Type: "WATCH_EMAIL", WatchCriteria: "email follow up dari Yere soal proposal",
		WatchFrom: "yere", WatchKeyword: "follow up,proposal", WatchLabel: label,
	})
	var (
		id                                    int64
		criteria, fromF, kwF, createdBy, stat string
	)
	err := pool.QueryRow(ctx, `
		SELECT id, criteria, from_filter, keyword_filter, created_by, status
		FROM email_watches WHERE label = $1 ORDER BY id DESC LIMIT 1`, label).Scan(
		&id, &criteria, &fromF, &kwF, &createdBy, &stat)
	if err != nil {
		t.Fatalf("#1: pantauan tidak tersimpan: %v", err)
	}
	if fromF != "yere" || !strings.Contains(kwF, "proposal") || createdBy != "su" || stat != "active" {
		t.Fatalf("#1: kolom pantauan salah: from=%q kw=%q by=%q status=%q", fromF, kwF, createdBy, stat)
	}

	// 2. Snapshot [PANTAUAN EMAIL AKTIF] memuat watchId + label + pra-saring.
	snap := h.buildWatchSnapshot(ctx)
	if !strings.Contains(snap, fmt.Sprintf("watchId=%d", id)) || !strings.Contains(snap, label) {
		t.Fatalf("#2: snapshot tak memuat id/label: %q", snap)
	}
	if !strings.Contains(snap, "dari~yere") {
		t.Fatalf("#2: snapshot tak memuat pra-saring: %q", snap)
	}

	// 3. Pra-saring (murah, tanpa LLM): from + keyword.
	w := model.EmailWatch{FromFilter: "yere", KeywordFilter: "follow up,proposal"}
	match := services.EmailMessage{FromAddress: "yere@contoh.com", FromName: "Yere", Subject: "Follow up proposal"}
	noFrom := services.EmailMessage{FromAddress: "orang@lain.com", Subject: "Follow up proposal"}
	noKw := services.EmailMessage{FromAddress: "yere@contoh.com", Subject: "halo apa kabar"}
	if !emailPassesPrefilter(match, w) {
		t.Fatalf("#3: email cocok malah ditolak pra-saring")
	}
	if emailPassesPrefilter(noFrom, w) || emailPassesPrefilter(noKw, w) {
		t.Fatalf("#3: pra-saring meloloskan email yang seharusnya gagal")
	}

	// 4. Deteksi "sudah dibalas" (Opsi A): cocokkan conversationId + waktu kirim > terima.
	recv := time.Now().Add(-2 * time.Hour)
	em := services.EmailMessage{ConversationID: "conv-1", ReceivedDateTime: recv.UTC().Format(time.RFC3339)}
	idxReplied := map[string]time.Time{"conv-1": recv.Add(30 * time.Minute)}
	idxOld := map[string]time.Time{"conv-1": recv.Add(-30 * time.Minute)} // balasan sebelum email → bukan balasan
	if known, replied := emailReplied(em, idxReplied, true); !known || !replied {
		t.Fatalf("#4: balasan setelah email tak terdeteksi (known=%v replied=%v)", known, replied)
	}
	if known, replied := emailReplied(em, map[string]time.Time{}, true); !known || replied {
		t.Fatalf("#4: tanpa balasan seharusnya known&belum-dibalas")
	}
	if known, replied := emailReplied(em, idxOld, true); !known || replied {
		t.Fatalf("#4: balasan LEBIH LAMA dari email tak boleh dihitung sebagai dibalas")
	}
	if known, _ := emailReplied(em, nil, false); known {
		t.Fatalf("#4: indeks Sent tak tersedia harus status TAK diketahui")
	}

	// 5. evaluateWatch memajukan last_seen tanpa menembak orchestrator (email tak lolos
	//    pra-saring → tak ada kandidat → tak ada push), sekaligus menandai email terproses.
	watches, err := h.Store.ListActiveEmailWatches(ctx, "su", 50)
	if err != nil {
		t.Fatalf("#5: list pantauan gagal: %v", err)
	}
	var target model.EmailWatch
	for _, x := range watches {
		if x.ID == id {
			target = x
		}
	}
	if target.ID == 0 {
		t.Fatalf("#5: pantauan #%d tak ada di daftar aktif", id)
	}
	future := time.Now().Add(3 * time.Hour)
	nonMatch := services.EmailMessage{
		FromAddress: "bukan-yere@lain.com", FromName: "Orang Lain", Subject: "promo diskon",
		ReceivedDateTime: future.UTC().Format(time.RFC3339),
	}
	h.evaluateWatch(ctx, target, []services.EmailMessage{nonMatch}, nil, false)
	var lastSeen time.Time
	_ = pool.QueryRow(ctx, `SELECT last_seen_at FROM email_watches WHERE id=$1`, id).Scan(&lastSeen)
	if !lastSeen.After(target.LastSeenAt) {
		t.Fatalf("#5: last_seen tidak maju: sebelum=%s sesudah=%s", target.LastSeenAt, lastSeen)
	}

	// 6. Format instruksi laporan: poin-poin + status baca & balas, memuat kriteria.
	reportEmail := services.EmailMessage{
		FromName: "Yere", FromAddress: "yere@contoh.com", Subject: "Follow up proposal kerja sama",
		BodyPreview: "Menanyakan kelanjutan proposal.", ReceivedDateTime: recv.UTC().Format(time.RFC3339),
		IsRead: false, ConversationID: "conv-2",
	}
	instr := buildEmailWatchInstruction(target, []services.EmailMessage{reportEmail}, map[string]time.Time{}, true)
	for _, want := range []string{"📧", "BELUM dibaca", "BELUM dibalas", "Follow up proposal kerja sama", "TIDAK TEPERCAYA"} {
		if !strings.Contains(instr, want) {
			t.Fatalf("#6: instruksi laporan tak memuat %q:\n%s", want, instr)
		}
	}

	// 7. Gerbang cancel: non-SU tak bisa membatalkan; SU bisa.
	h.watchEmail(ctx, suContact, model.Action{
		Type: "WATCH_EMAIL", WatchCriteria: "pantauan uji gerbang", WatchLabel: labelB,
	})
	var idB int64
	_ = pool.QueryRow(ctx, `SELECT id FROM email_watches WHERE label=$1 ORDER BY id DESC LIMIT 1`, labelB).Scan(&idB)
	h.cancelWatch(ctx, extContact, model.Action{Type: "CANCEL_WATCH", WatchID: idB})
	_ = pool.QueryRow(ctx, `SELECT status FROM email_watches WHERE id=$1`, idB).Scan(&stat)
	if stat != "active" {
		t.Fatalf("#7: non-SU berhasil membatalkan pantauan (gerbang bocor): status=%q", stat)
	}
	h.cancelWatch(ctx, suContact, model.Action{Type: "CANCEL_WATCH", WatchID: id})
	_ = pool.QueryRow(ctx, `SELECT status FROM email_watches WHERE id=$1`, id).Scan(&stat)
	if stat != "cancelled" {
		t.Fatalf("#7: SU gagal membatalkan pantauan #%d: status=%q", id, stat)
	}

	t.Logf("OK Fitur E: watch tersimpan+snapshot, pra-saring, deteksi balasan, last_seen maju, format laporan, gerbang cancel")
}

// ─────────────────── Cek Email On-Demand (READ_EMAILS) ───────────────────
// Menguji builder instruksi hasil cek inbox on-demand: pembingkaian scope, format poin-poin,
// jalur daftar kosong ("tidak ada"), status baca/balas, dan pertahanan prompt-injection —
// semuanya fungsi murni, tanpa jaringan.
func TestFeatureKtoE_ReadEmailsOnDemand(t *testing.T) {
	if wibZone == nil {
		t.Fatal("wibZone nil")
	}
	recv := time.Now().Add(-2 * time.Hour)

	// 1. describeReadScope membingkai scope + filter dengan benar.
	cases := []struct {
		scope string
		pf    model.EmailWatch
		want  []string
	}{
		{"unread", model.EmailWatch{}, []string{"BELUM dibaca"}},
		{"unreplied", model.EmailWatch{FromFilter: "klien"}, []string{"BELUM dibalas", "dari ~klien"}},
		{"", model.EmailWatch{KeywordFilter: "invoice"}, []string{"email terbaru", "kata kunci ~invoice"}},
	}
	for _, c := range cases {
		got := describeReadScope(c.scope, c.pf)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Fatalf("describeReadScope(%q,%+v)=%q tak memuat %q", c.scope, c.pf, got, w)
			}
		}
	}

	// 2. Daftar KOSONG → instruksi memerintahkan menyampaikan "tidak ada", bukan mengarang.
	empty := buildEmailReadInstruction("unread", model.EmailWatch{}, nil, nil, false)
	for _, want := range []string{"HASIL CEK EMAIL", "KOSONG", "TIDAK ADA email"} {
		if !strings.Contains(empty, want) {
			t.Fatalf("instruksi kosong tak memuat %q:\n%s", want, empty)
		}
	}
	// Sanity: tak boleh memuat template baris email berisi angka urut palsu.
	if strings.Contains(empty, "1) Dari:") {
		t.Fatalf("instruksi kosong seharusnya tak memuat baris email:\n%s", empty)
	}

	// 3. Daftar berisi: format poin-poin + status baca/balas + kriteria + peringatan keamanan.
	sentIdx := map[string]time.Time{"conv-x": recv.Add(1 * time.Hour)} // dibalas SETELAH diterima
	emails := []services.EmailMessage{
		{ // belum dibaca, BELUM dibalas (conversation tak ada di indeks Sent)
			FromName: "Klien A", FromAddress: "a@klien.com", Subject: "Penawaran baru",
			BodyPreview: "Mohon ditinjau.", ReceivedDateTime: recv.UTC().Format(time.RFC3339),
			IsRead: false, ConversationID: "conv-a",
		},
		{ // sudah dibaca, SUDAH dibalas (ada di indeks Sent, waktu kirim > terima)
			FromName: "Klien B", FromAddress: "b@klien.com", Subject: "Konfirmasi jadwal",
			BodyPreview: "Terima kasih.", ReceivedDateTime: recv.UTC().Format(time.RFC3339),
			IsRead: true, ConversationID: "conv-x",
		},
	}
	instr := buildEmailReadInstruction("all", model.EmailWatch{}, emails, sentIdx, true)
	for _, want := range []string{
		"📧", "Hasil cek inbox", "Penawaran baru", "Konfirmasi jadwal",
		"BELUM dibaca", "sudah dibaca", "BELUM dibalas", "SUDAH dibalas",
		"TIDAK TEPERCAYA",
	} {
		if !strings.Contains(instr, want) {
			t.Fatalf("instruksi berisi tak memuat %q:\n%s", want, instr)
		}
	}

	t.Logf("OK READ_EMAILS: pembingkaian scope, jalur kosong, format poin-poin, status baca/balas, keamanan")
}

// ─────────────────── Fitur E (LIVE): baca email dari MS Graph ───────────────────
// Smoke test read-only ke mailbox ASLI (pa@hypernet.co.id): membuktikan auth (izin
// aplikasi → fallback refresh token .env) & pembacaan inbox + status baca benar-benar
// bekerja. TIDAK mengubah apa pun (hanya GET). Jalankan terpisah:
//
//	go test -tags integration ./src/routes/ -run TestLiveEmailReadSmoke -v
func TestLiveEmailReadSmoke(t *testing.T) {
	h, pool, ctx := keE2EFixture(t)
	defer h.Store.Close()
	defer pool.Close()

	if !h.Services.Enabled() {
		t.Skip("MS Graph nonaktif (kredensial belum lengkap) — lewati smoke live")
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	emails, err := h.Services.ListRecentEmails(rctx, "", 5)
	if err != nil {
		t.Fatalf("baca email LIVE gagal (cek Mail.Read app / MS_GRAPH_REFRESH_TOKEN): %v", err)
	}
	t.Logf("LIVE: berhasil membaca %d email terbaru dari inbox", len(emails))
	for i, e := range emails {
		status := "belum dibaca"
		if e.IsRead {
			status = "sudah dibaca"
		}
		t.Logf("  %d) [%s] dari %q subj=%q", i+1, status, e.FromName, e.Subject)
	}

	// Buktikan folder Sent (deteksi 'sudah dibalas') juga terbaca.
	sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
	defer scancel()
	sent, serr := h.Services.ListSentReplies(sctx, "", 5)
	if serr != nil {
		t.Fatalf("baca folder Sent LIVE gagal: %v", serr)
	}
	t.Logf("LIVE: berhasil membaca %d email terkirim (folder Sent)", len(sent))
}
