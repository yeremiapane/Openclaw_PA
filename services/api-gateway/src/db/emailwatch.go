package db

import (
	"context"
	"errors"
	"time"

	"pa-ai/api-gateway/src/model"
)

// ErrEmailWatchNotFound dikembalikan bila pantauan email tidak ada, sudah dihentikan,
// atau created_by-nya tidak cocok (gerbang kepemilikan).
var ErrEmailWatchNotFound = errors.New("pantauan email tidak ditemukan atau bukan milik Anda")

// CreateEmailWatch menyimpan satu pantauan email baru dan mengembalikan id-nya.
// Status awal selalu 'active'; last_seen_at default now() (email SEBELUM ini diabaikan
// agar tak melapor email lama). created_by default 'su'.
func (s *Store) CreateEmailWatch(ctx context.Context, w model.EmailWatch) (int64, error) {
	createdBy := w.CreatedBy
	if createdBy == "" {
		createdBy = "su"
	}
	var expiresAt any
	if w.ExpiresAt != nil {
		expiresAt = *w.ExpiresAt
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO email_watches (criteria, label, from_filter, keyword_filter,
		                           created_by, status, last_seen_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,'active',now(),$6)
		RETURNING id
	`, w.Criteria, w.Label, w.FromFilter, w.KeywordFilter, createdBy, expiresAt).Scan(&id)
	return id, err
}

// ListActiveEmailWatches mengembalikan pantauan 'active' milik created_by (mis. "su"),
// terurut terbaru dulu. Pantauan yang sudah kedaluwarsa (expires_at < now) DIKECUALIKAN.
// Dipakai worker poller DAN untuk menyuntik snapshot [PANTAUAN EMAIL AKTIF] ke orchestrator.
func (s *Store) ListActiveEmailWatches(ctx context.Context, createdBy string, limit int) ([]model.EmailWatch, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, criteria, COALESCE(label,''), COALESCE(from_filter,''),
		       COALESCE(keyword_filter,''), COALESCE(created_by,''), status,
		       last_seen_at, expires_at, created_at, last_checked_at
		FROM email_watches
		WHERE status='active' AND created_by=$1
		  AND (expires_at IS NULL OR expires_at > now())
		ORDER BY created_at DESC
		LIMIT $2
	`, createdBy, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.EmailWatch
	for rows.Next() {
		var w model.EmailWatch
		if err := rows.Scan(&w.ID, &w.Criteria, &w.Label, &w.FromFilter, &w.KeywordFilter,
			&w.CreatedBy, &w.Status, &w.LastSeenAt, &w.ExpiresAt, &w.CreatedAt, &w.LastCheckedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// TouchEmailWatch memajukan last_seen_at (batas bawah waktu email yang sudah dinilai)
// dan menandai waktu pemeriksaan terakhir. Dipanggil worker setelah satu ronde evaluasi
// agar tiap email dinilai tepat sekali. last_seen_at hanya MAJU (tak pernah mundur).
func (s *Store) TouchEmailWatch(ctx context.Context, id int64, seenUpTo time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE email_watches
		SET last_seen_at = GREATEST(last_seen_at, $2), last_checked_at = now()
		WHERE id=$1
	`, id, seenUpTo)
	return err
}

// CancelEmailWatch menghentikan satu pantauan 'active' — hanya bila created_by cocok
// (gerbang: agent/kontak lain tak bisa menghentikan pantauan SU). Mengembalikan
// ErrEmailWatchNotFound bila tak ada baris aktif yang cocok.
func (s *Store) CancelEmailWatch(ctx context.Context, id int64, createdBy string) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE email_watches SET status='cancelled'
		WHERE id=$1 AND created_by=$2 AND status='active'
	`, id, createdBy)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrEmailWatchNotFound
	}
	return nil
}
