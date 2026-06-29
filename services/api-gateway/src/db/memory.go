package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"pa-ai/api-gateway/src/model"
)

// ─── Memory Layer (PostgreSQL) ─────────────
//
// conversationId dipakai konsisten sebagai PK conversations dan FK messages.
// Formatnya sama dengan session-key OpenClaw: "agent:<agentType>:<phone|lid>".

// EnsureConversation memastikan baris conversation ada (idempotent).
// Tidak menimpa state/agent yang sudah ada.
func (s *Store) EnsureConversation(ctx context.Context, convID string, contactID int, agentID string) error {
	var cid any
	if contactID > 0 {
		cid = contactID
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO conversations (id, contact_id, agent_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (id) DO NOTHING`,
		convID, cid, agentID)
	return err
}

// SaveMessage menyimpan satu pesan ke tabel messages (sumber kebenaran permanen).
func (s *Store) SaveMessage(ctx context.Context, convID, role, text, agentID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO messages (conversation_id, role, text, agent_id)
		VALUES ($1, $2, $3, $4)`,
		convID, role, text, nullStr(agentID))
	return err
}

// RecentMessages mengambil `limit` pesan terakhir, dikembalikan secara
// kronologis (terlama → terbaru) agar siap dipakai sebagai riwayat prompt.
func (s *Store) RecentMessages(ctx context.Context, convID string, limit int) ([]model.Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT role, text, COALESCE(agent_id,''), created_at
		FROM   messages
		WHERE  conversation_id = $1
		ORDER  BY created_at DESC, id DESC
		LIMIT  $2`, convID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []model.Message
	for rows.Next() {
		var m model.Message
		if err := rows.Scan(&m.Role, &m.Text, &m.AgentID, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Balik urutan menjadi kronologis (terlama dulu).
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// GetConversationState mengembalikan state percakapan; "NEW_CONTACT" bila belum ada.
func (s *Store) GetConversationState(ctx context.Context, convID string) (string, error) {
	var state string
	err := s.pool.QueryRow(ctx,
		`SELECT state FROM conversations WHERE id = $1`, convID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "NEW_CONTACT", nil
	}
	if err != nil {
		return "", err
	}
	return state, nil
}

// UpdateConversationState mengubah state percakapan (dipakai penuh di Fase 8).
func (s *Store) UpdateConversationState(ctx context.Context, convID, state string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE conversations SET state = $2 WHERE id = $1`, convID, state)
	return err
}

// SaveFact menyimpan satu fakta kontak (deduplikasi via UNIQUE(contact_id, fact)).
func (s *Store) SaveFact(ctx context.Context, contactID int, fact, source string) error {
	if contactID <= 0 || fact == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO contact_facts (contact_id, fact, source)
		VALUES ($1, $2, $3)
		ON CONFLICT (contact_id, fact) DO UPDATE SET extracted_at = now()`,
		contactID, fact, nullStr(source))
	return err
}

// ListFacts mengembalikan fakta-fakta kontak (terbaru dulu).
func (s *Store) ListFacts(ctx context.Context, contactID, limit int) ([]string, error) {
	if contactID <= 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := s.pool.Query(ctx, `
		SELECT fact FROM contact_facts
		WHERE  contact_id = $1
		ORDER  BY extracted_at DESC
		LIMIT  $2`, contactID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var facts []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		facts = append(facts, f)
	}
	return facts, rows.Err()
}
