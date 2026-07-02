package db

import (
	"context"
	"errors"
	"time"

	"pa-ai/api-gateway/src/model"
)

// ErrScheduledTaskNotFound dikembalikan bila pengingat tidak ada, sudah diputuskan,
// atau created_by-nya tidak cocok (gerbang kepemilikan).
var ErrScheduledTaskNotFound = errors.New("pengingat tidak ditemukan atau bukan milik Anda")

// CreateScheduledTask menyimpan satu tugas terjadwal (pengingat) baru dan
// mengembalikan id-nya. Status awal selalu 'pending'.
func (s *Store) CreateScheduledTask(ctx context.Context, t model.ScheduledTask) (int64, error) {
	var meetingID any
	if t.MeetingID != nil && *t.MeetingID > 0 {
		meetingID = *t.MeetingID
	}
	createdBy := t.CreatedBy
	if createdBy == "" {
		createdBy = "su"
	}
	kind := t.Kind
	if kind == "" {
		kind = "reminder"
	}
	recurKind := t.RecurKind
	if recurKind == "" {
		recurKind = "none"
	}
	var recurTime any
	if t.RecurTime != "" {
		recurTime = t.RecurTime
	}
	var recurDow any
	if t.RecurDow != nil {
		recurDow = *t.RecurDow
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO scheduled_tasks (fire_at, kind, note, meeting_id, status, created_by,
		                             recur_kind, recur_time, recur_dow, label)
		VALUES ($1,$2,$3,$4,'pending',$5,$6,$7,$8,$9)
		RETURNING id
	`, t.FireAt, kind, t.Note, meetingID, createdBy,
		recurKind, recurTime, recurDow, t.Label).Scan(&id)
	return id, err
}

// ClaimDueTasks mengambil-alih (atomik) tugas 'pending' yang jatuh tempo pada atau
// sebelum `cutoff`. Dengan memberi cutoff = now()+prepLead, pemanggil bisa MENGKLAIM
// LEBIH AWAL agar orchestrator sempat menyusun pesan sebelum waktu tampil, lalu pesan
// ditahan sampai fire_at. Menandai 'fired' dan mengembalikannya. FOR UPDATE SKIP
// LOCKED mencegah satu tugas terkirim ganda meski ada beberapa pemroses. Pemanggil
// yang gagal mengirim memanggil MarkTaskError untuk mengoreksi status menjadi 'error'.
func (s *Store) ClaimDueTasks(ctx context.Context, limit int, cutoff time.Time) ([]model.ScheduledTask, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE scheduled_tasks SET status='fired', fired_at=now()
		WHERE id IN (
			SELECT id FROM scheduled_tasks
			WHERE status='pending' AND fire_at <= $2
			ORDER BY fire_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, fire_at, kind, note, COALESCE(meeting_id,0), status,
		          COALESCE(created_by,''), COALESCE(error_text,''),
		          COALESCE(recur_kind,'none'), COALESCE(recur_time,''), recur_dow,
		          COALESCE(label,''), created_at, fired_at
	`, limit, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.ScheduledTask
	for rows.Next() {
		t, err := scanScheduledTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// scanScheduledTask memindai satu baris scheduled_tasks dengan urutan kolom kanonis
// (dipakai bersama ClaimDueTasks & ListActiveTasks).
func scanScheduledTask(row interface{ Scan(...any) error }) (model.ScheduledTask, error) {
	var t model.ScheduledTask
	var meetingID int64
	var recurDow *int16
	var firedAt *time.Time
	if err := row.Scan(&t.ID, &t.FireAt, &t.Kind, &t.Note, &meetingID,
		&t.Status, &t.CreatedBy, &t.ErrorText,
		&t.RecurKind, &t.RecurTime, &recurDow, &t.Label, &t.CreatedAt, &firedAt); err != nil {
		return t, err
	}
	if meetingID > 0 {
		mid := meetingID
		t.MeetingID = &mid
	}
	if recurDow != nil {
		d := int(*recurDow)
		t.RecurDow = &d
	}
	t.FiredAt = firedAt
	return t, nil
}

// ListActiveTasks mengembalikan pengingat 'pending' milik created_by (mis. "su"),
// terurut waktu terdekat. Dipakai untuk menyuntik snapshot [PENGINGAT AKTIF] ke
// orchestrator agar SU bisa melihat & membatalkannya tanpa action LIST khusus.
func (s *Store) ListActiveTasks(ctx context.Context, createdBy string, limit int) ([]model.ScheduledTask, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, fire_at, kind, note, COALESCE(meeting_id,0), status,
		       COALESCE(created_by,''), COALESCE(error_text,''),
		       COALESCE(recur_kind,'none'), COALESCE(recur_time,''), recur_dow,
		       COALESCE(label,''), created_at, fired_at
		FROM scheduled_tasks
		WHERE status='pending' AND created_by=$1
		ORDER BY fire_at
		LIMIT $2
	`, createdBy, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.ScheduledTask
	for rows.Next() {
		t, err := scanScheduledTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CancelScheduledTask membatalkan satu pengingat 'pending' berdasarkan id — TAPI hanya
// bila created_by cocok (gerbang: agent/kontak lain tidak bisa membatalkan pengingat
// SU). Untuk pengingat berulang, ini menghentikan seluruh seri (baris kejadian
// berikutnya belum ada sampai baris ini fire). Mengembalikan ErrScheduledTaskNotFound
// bila tidak ada baris pending yang cocok.
func (s *Store) CancelScheduledTask(ctx context.Context, id int64, createdBy string) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE scheduled_tasks SET status='cancelled'
		WHERE id=$1 AND created_by=$2 AND status='pending'
	`, id, createdBy)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrScheduledTaskNotFound
	}
	return nil
}

// MarkTaskError menandai tugas yang gagal dikirim agar bisa ditelusuri (tidak
// dicoba ulang otomatis — keputusan sengaja untuk menghindari spam saat error
// berulang; bisa dijadwalkan ulang manual).
func (s *Store) MarkTaskError(ctx context.Context, id int64, errText string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE scheduled_tasks SET status='error', error_text=$2 WHERE id=$1
	`, id, errText)
	return err
}

// CancelTasksForMeeting membatalkan semua pengingat 'pending' yang tertaut ke satu
// meeting (dipakai saat meeting dibatalkan atau dijadwal ulang).
func (s *Store) CancelTasksForMeeting(ctx context.Context, meetingID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE scheduled_tasks SET status='cancelled'
		WHERE meeting_id=$1 AND status='pending'
	`, meetingID)
	return err
}
