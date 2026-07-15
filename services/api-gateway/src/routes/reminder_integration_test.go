//go:build integration

// Integration test untuk pengingat BERULANG + pengelolaan (SET_REMINDER recur,
// reschedule-on-fire, CANCEL_REMINDER, snapshot [PENGINGAT AKTIF], gerbang kepemilikan).
// Berjalan terhadap Postgres ASLI memakai kode terkompilasi yang sama seperti gateway —
// TANPA mengirim WhatsApp (SU diarahkan ke nomor bogus; tidak ada pengiriman di jalur ini).
// Jalankan:
//
//	go test -tags integration ./src/routes/ -run TestReminderRecurrenceAndCancel -v
//
// Semua baris yang dibuat memakai penanda note ber-namespace dan dibersihkan di akhir.
package routes

import (
	"context"
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

func TestReminderRecurrenceAndCancel(t *testing.T) {
	ctx := context.Background()
	_ = godotenv.Load("../../../../.env")
	cfg := config.Load()

	store, err := db.NewStore(ctx, cfg)
	if err != nil {
		t.Fatalf("koneksi Postgres gagal: %v", err)
	}
	defer store.Close()

	pool, err := pgxpool.New(ctx, cfg.DBConnString())
	if err != nil {
		t.Fatalf("pool mentah gagal: %v", err)
	}
	defer pool.Close()

	h := &Handler{
		Store:   store,
		Waha:    waha.New(cfg.WahaURL, cfg.WahaAPIKey, cfg.WahaSession),
		SUPhone: "628100000001",
	}

	marker := "ITEST-RECUR-" + time.Now().Format("20060102T150405.000")
	note := marker + " minum obat pagi"

	// Bersihkan apa pun hasilnya (scheduled_tasks tak punya conversation_id → pakai note).
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM scheduled_tasks WHERE note LIKE $1`, marker+"%")
	}()

	suContact := &model.Contact{Phone: h.SUPhone, TrustLevel: "su", Name: "Pak Sudianto (SU)"}

	// ---- 1. SET_REMINDER berulang harian (tanpa reminderTime → dihitung dari recurTime) ----
	h.setReminder(ctx, suContact, model.Action{
		Type: "SET_REMINDER", RecurKind: "daily", RecurTime: "08:00",
		ReminderNote: note, ReminderLabel: "minum obat pagi",
	})

	first := firstTaskByNote(t, ctx, pool, marker)
	if first.RecurKind != "daily" || first.RecurTime != "08:00" {
		t.Fatalf("#1: recur salah: kind=%q time=%q", first.RecurKind, first.RecurTime)
	}
	if first.Status != "pending" || first.CreatedBy != "su" {
		t.Fatalf("#1: status/created_by salah: %q/%q", first.Status, first.CreatedBy)
	}
	if !first.FireAt.After(time.Now().Add(-2 * time.Minute)) {
		t.Fatalf("#1: fire_at tidak masuk akal: %s", first.FireAt)
	}

	// ---- 2. Snapshot [PENGINGAT AKTIF] harus memuat reminderId + label + tanda berulang ----
	snap := h.buildReminderSnapshot(ctx)
	if !strings.Contains(snap, "reminderId=") || !strings.Contains(snap, "minum obat pagi") {
		t.Fatalf("#2: snapshot tidak memuat id/label: %q", snap)
	}
	if !strings.Contains(snap, "BERULANG tiap hari 08:00") {
		t.Fatalf("#2: snapshot tidak menandai berulang: %q", snap)
	}

	// ---- 3. Reschedule-on-fire: rearm harus melahirkan baris pending BARU untuk besok ----
	countPendingByNote := func() int {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM scheduled_tasks WHERE note LIKE $1 AND status='pending'`, marker+"%").Scan(&n)
		return n
	}
	if got := countPendingByNote(); got != 1 {
		t.Fatalf("#3: awal harus 1 pending, dapat %d", got)
	}
	h.rearmRecurring(ctx, first)
	if got := countPendingByNote(); got != 2 {
		t.Fatalf("#3: setelah rearm harus 2 pending (kejadian berikut lahir), dapat %d", got)
	}

	// ---- 4. Gerbang kepemilikan: non-SU (created_by mismatch) TIDAK bisa membatalkan ----
	if err := store.CancelScheduledTask(ctx, first.ID, "system"); err != db.ErrScheduledTaskNotFound {
		t.Fatalf("#4: cancel oleh non-pemilik harus ErrScheduledTaskNotFound, dapat %v", err)
	}

	// ---- 5. CANCEL_REMINDER oleh SU membatalkan baris pertama ----
	h.cancelReminder(ctx, suContact, model.Action{Type: "CANCEL_REMINDER", ReminderID: first.ID})
	var st string
	_ = pool.QueryRow(ctx, `SELECT status FROM scheduled_tasks WHERE id=$1`, first.ID).Scan(&st)
	if st != "cancelled" {
		t.Fatalf("#5: status baris #%d = %q, ingin cancelled", first.ID, st)
	}

	// ---- 6. Gerbang non-SU pada handler cancelReminder (trust bukan su) diabaikan ----
	// Ambil id baris kejadian-berikut yang masih pending.
	next := firstTaskByNote(t, ctx, pool, marker) // pending terdekat = kejadian berikut
	ext := &model.Contact{Phone: "628100000009", TrustLevel: "external"}
	h.cancelReminder(ctx, ext, model.Action{Type: "CANCEL_REMINDER", ReminderID: next.ID})
	_ = pool.QueryRow(ctx, `SELECT status FROM scheduled_tasks WHERE id=$1`, next.ID).Scan(&st)
	if st != "pending" {
		t.Fatalf("#6: baris #%d dibatalkan oleh non-SU (status=%q) — gerbang bocor", next.ID, st)
	}

	t.Logf("OK: recur harian dibuat, rearm melahirkan kejadian berikut, cancel & gerbang kepemilikan bekerja")
}

// firstTaskByNote mengambil pengingat pending TERDEKAT (fire_at naik) yang note-nya
// diawali marker.
func firstTaskByNote(t *testing.T, ctx context.Context, pool *pgxpool.Pool, marker string) model.ScheduledTask {
	t.Helper()
	var task model.ScheduledTask
	var recurDow *int16
	err := pool.QueryRow(ctx, `
		SELECT id, fire_at, kind, note, status, COALESCE(created_by,''),
		       COALESCE(recur_kind,'none'), COALESCE(recur_time,''), recur_dow, COALESCE(label,'')
		FROM scheduled_tasks WHERE note LIKE $1 AND status='pending'
		ORDER BY fire_at LIMIT 1`, marker+"%").Scan(
		&task.ID, &task.FireAt, &task.Kind, &task.Note, &task.Status, &task.CreatedBy,
		&task.RecurKind, &task.RecurTime, &recurDow, &task.Label)
	if err != nil {
		t.Fatalf("firstTaskByNote gagal: %v", err)
	}
	if recurDow != nil {
		d := int(*recurDow)
		task.RecurDow = &d
	}
	return task
}
