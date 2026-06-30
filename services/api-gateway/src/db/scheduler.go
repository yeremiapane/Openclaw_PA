package db

import (
	"context"
	"time"

	"pa-ai/api-gateway/src/model"
)

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
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO scheduled_tasks (fire_at, kind, note, meeting_id, status, created_by)
		VALUES ($1,$2,$3,$4,'pending',$5)
		RETURNING id
	`, t.FireAt, kind, t.Note, meetingID, createdBy).Scan(&id)
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
		          COALESCE(created_by,''), COALESCE(error_text,''), created_at, fired_at
	`, limit, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.ScheduledTask
	for rows.Next() {
		var t model.ScheduledTask
		var meetingID int64
		var firedAt *time.Time
		if err := rows.Scan(&t.ID, &t.FireAt, &t.Kind, &t.Note, &meetingID,
			&t.Status, &t.CreatedBy, &t.ErrorText, &t.CreatedAt, &firedAt); err != nil {
			return nil, err
		}
		if meetingID > 0 {
			mid := meetingID
			t.MeetingID = &mid
		}
		t.FiredAt = firedAt
		out = append(out, t)
	}
	return out, rows.Err()
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
