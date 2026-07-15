//go:build integration

// Integration test untuk alur negosiasi waktu meeting offline + edge case renegosiasi
// waktu (presentTimeApprovalToSU). Berjalan terhadap Postgres ASLI (infra live) memakai
// kode terkompilasi yang sama seperti gateway terdeploy — TANPA mengirim WhatsApp ke
// orang nyata (SU/Nova diarahkan ke nomor bogus; pesan tertahan tidak dikirim). Jalankan:
//
//	go test -tags integration ./src/routes/ -run TestVenueTimeNegotiation -v
//
// Seluruh baris yang dibuat memakai conversation_id ber-namespace "itest:" dan dibersihkan
// pada akhir test.
package routes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"pa-ai/api-gateway/src/config"
	"pa-ai/api-gateway/src/db"
	"pa-ai/api-gateway/src/model"
	"pa-ai/api-gateway/src/waha"
)

func TestVenueTimeNegotiationAndEdgeCase(t *testing.T) {
	ctx := context.Background()
	// CWD test = src/routes; .env ada di root repo (4 level naik). config.Load hanya
	// menelusuri sampai 3 level, jadi muat eksplisit dulu agar kredensial DB terbaca.
	_ = godotenv.Load("../../../../.env")
	cfg := config.Load()

	store, err := db.NewStore(ctx, cfg)
	if err != nil {
		t.Fatalf("koneksi Postgres gagal: %v", err)
	}
	defer store.Close()

	// Pool mentah untuk seed-bump-cleanup presisi (tanpa efek samping method Store).
	pool, err := pgxpool.New(ctx, cfg.DBConnString())
	if err != nil {
		t.Fatalf("pool mentah gagal: %v", err)
	}
	defer pool.Close()

	// SU & Nova diarahkan ke nomor bogus → tidak ada notifikasi ke orang nyata.
	h := &Handler{
		Store:     store,
		Waha:      waha.New(cfg.WahaURL, cfg.WahaAPIKey, cfg.WahaSession),
		SUPhone:   "628100000001",
		NovaPhone: "628100000002",
		// Services sengaja nil (jalur waktu tidak menyentuh Calendar/Email).
	}

	convID := "itest:venueflow:" + time.Now().Format("20060102T150405.000")
	extChat := "6281200000007@c.us"

	// Bersihkan di akhir apa pun hasilnya.
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM meeting_status_history WHERE meeting_id IN
			(SELECT id FROM meeting_requests WHERE conversation_id = $1)`, convID)
		_, _ = pool.Exec(ctx, `DELETE FROM outbound_messages WHERE conversation_id = $1`, convID)
		_, _ = pool.Exec(ctx, `DELETE FROM meeting_requests WHERE conversation_id = $1`, convID)
		_, _ = pool.Exec(ctx, `DELETE FROM approval_pending WHERE conversation_id = $1`, convID)
	}()

	t1 := time.Now().Add(72 * time.Hour).Truncate(time.Minute)
	t2 := t1.Add(3 * time.Hour) // waktu baru hasil renegosiasi

	details, _ := json.Marshal(meetingDetails{
		VenueCoordination: true,
		TimeAgreed:        true,
		AttendeeName:      "Budi ITest",
		Title:             "Diskusi ITest",
		DurationMinutes:   60,
	})
	mID, err := store.CreateMeetingRequest(ctx, model.MeetingRequest{
		ConversationID:   convID,
		AgentID:          "pa_communicator",
		RequestedVia:     "external",
		ExternalName:     "Budi ITest",
		Topic:            "Diskusi ITest",
		MeetingType:      "onsite",
		ProposedDatetime: &t1,
		Status:           "pending",
		Details:          details,
	}, "itest")
	if err != nil {
		t.Fatalf("seed meeting gagal: %v", err)
	}
	// externalChatIDForMeeting resolusi via target_chat approval (panggilan #2/#3) atau
	// segmen ke-3 conversation_id (panggilan #1) — tidak butuh seed outbound.
	_ = extChat

	countApprovals := func() int {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM approval_pending WHERE conversation_id = $1`, convID).Scan(&n)
		return n
	}
	countSUNotify := func() int {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM outbound_messages
			WHERE conversation_id = $1 AND kind = 'approval_notify'`, convID).Scan(&n)
		return n
	}
	readDetails := func() meetingDetails {
		m, e := store.MeetingByID(ctx, mID)
		if e != nil || m == nil {
			t.Fatalf("MeetingByID gagal: %v", e)
		}
		var d meetingDetails
		_ = json.Unmarshal(m.Details, &d)
		return d
	}

	// ---- Panggilan #1: ajukan waktu T1 ke SU ----
	h.presentTimeApprovalToSU(ctx, mID)

	m1, _ := store.MeetingByID(ctx, mID)
	if m1.ApprovalID == nil {
		t.Fatalf("#1: meeting tidak tertaut approval")
	}
	ap := *m1.ApprovalID
	if got := countApprovals(); got != 1 {
		t.Fatalf("#1: jumlah approval = %d, ingin 1", got)
	}
	if d := readDetails(); d.TimePresentedAt != t1.Format(time.RFC3339) {
		t.Fatalf("#1: timePresentedAt = %q, ingin %q", d.TimePresentedAt, t1.Format(time.RFC3339))
	}
	a1, _ := store.GetApproval(ctx, ap)
	if a1 == nil || a1.Status != "pending" {
		t.Fatalf("#1: approval status tidak pending: %+v", a1)
	}
	if !strings.Contains(a1.ResponseText, formatWIBLong(t1)) {
		t.Fatalf("#1: teks approval tidak memuat waktu T1 (%s): %q", formatWIBLong(t1), a1.ResponseText)
	}
	if got := countSUNotify(); got != 1 {
		t.Fatalf("#1: notifikasi SU = %d, ingin 1", got)
	}
	respT1 := a1.ResponseText

	// ---- Renegosiasi: eksternal ubah waktu ke T2 (SEBELUM SU memutuskan) ----
	if _, e := pool.Exec(ctx, `UPDATE meeting_requests SET proposed_datetime = $2 WHERE id = $1`, mID, t2); e != nil {
		t.Fatalf("bump waktu ke T2 gagal: %v", e)
	}

	// ---- Panggilan #2: approval yang MASIH pending harus disegarkan (bukan dobel) ----
	h.presentTimeApprovalToSU(ctx, mID)

	m2, _ := store.MeetingByID(ctx, mID)
	if m2.ApprovalID == nil || *m2.ApprovalID != ap {
		t.Fatalf("#2: approval berubah/ganda (ingin tetap #%d, dapat %v)", ap, m2.ApprovalID)
	}
	if got := countApprovals(); got != 1 {
		t.Fatalf("#2: jumlah approval = %d, ingin tetap 1 (tidak dobel)", got)
	}
	if d := readDetails(); d.TimePresentedAt != t2.Format(time.RFC3339) {
		t.Fatalf("#2: timePresentedAt = %q, ingin %q", d.TimePresentedAt, t2.Format(time.RFC3339))
	}
	a2, _ := store.GetApproval(ctx, ap)
	if a2.ResponseText == respT1 {
		t.Fatalf("#2: teks approval tidak diperbarui (masih T1)")
	}
	if !strings.Contains(a2.ResponseText, formatWIBLong(t2)) {
		t.Fatalf("#2: teks approval tidak memuat waktu T2 (%s): %q", formatWIBLong(t2), a2.ResponseText)
	}
	if got := countSUNotify(); got != 2 {
		t.Fatalf("#2: notifikasi SU = %d, ingin 2 (SU dinotifikasi ulang dgn waktu baru)", got)
	}

	// ---- Panggilan #3: waktu TIDAK berubah → tidak boleh notif ulang / tidak dobel ----
	h.presentTimeApprovalToSU(ctx, mID)

	if got := countApprovals(); got != 1 {
		t.Fatalf("#3: jumlah approval = %d, ingin tetap 1", got)
	}
	if got := countSUNotify(); got != 2 {
		t.Fatalf("#3: notifikasi SU = %d, ingin tetap 2 (tanpa spam saat waktu tak berubah)", got)
	}

	t.Logf("OK: approval #%d tunggal, disegarkan T1→T2, idempoten tanpa spam", ap)
}
