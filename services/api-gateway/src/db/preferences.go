package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// GetAgentPrefs mengambil overlay preferensi (gaya & sebagian perilaku) untuk sebuah
// agent. Mengembalikan string kosong (tanpa error) bila belum pernah disetel.
func (s *Store) GetAgentPrefs(ctx context.Context, agent string) (string, error) {
	var prefs string
	err := s.pool.QueryRow(ctx,
		`SELECT prefs FROM agent_preferences WHERE agent=$1`, agent).Scan(&prefs)
	if err != nil {
		// Baris belum ada = belum ada preferensi kustom; itu bukan kondisi error.
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return prefs, nil
}

// SetAgentPrefs menyimpan (UPSERT) overlay preferensi untuk sebuah agent. Teks kosong
// berarti menghapus semua preferensi kustom (baris tetap ada dengan prefs=”).
func (s *Store) SetAgentPrefs(ctx context.Context, agent, prefs string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO agent_preferences (agent, prefs, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (agent) DO UPDATE SET prefs=EXCLUDED.prefs, updated_at=now()`,
		agent, prefs)
	return err
}
