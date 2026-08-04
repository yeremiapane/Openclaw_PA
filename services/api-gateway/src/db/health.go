package db

import "context"

// CountScheduledTasksByStatus mengembalikan jumlah tugas terjadwal dikelompokkan per
// status (pending/fired/error/cancelled). Dipakai kolektor metrik untuk
// mengekspos gateway_scheduled_tasks — mis. memantau tugas yang gagal terkirim (error).
func (s *Store) CountScheduledTasksByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT status, count(*) FROM scheduled_tasks GROUP BY status
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// CountPendingApprovals mengembalikan jumlah approval (pesan keluar) yang masih
// menunggu keputusan SU. Dipakai kolektor metrik untuk gateway_pending_approvals.
func (s *Store) CountPendingApprovals(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM approval_pending WHERE status = 'pending'
	`).Scan(&n)
	return n, err
}

// PoolStats mengembalikan statistik pool koneksi pgx: koneksi terpakai (acquired),
// menganggur (idle), total, dan batas maksimum. Dipakai kolektor metrik
// untuk mendeteksi pool yang nyaris habis (acquired mendekati max).
func (s *Store) PoolStats() (acquired, idle, total, max int32) {
	st := s.pool.Stat()
	return st.AcquiredConns(), st.IdleConns(), st.TotalConns(), st.MaxConns()
}
