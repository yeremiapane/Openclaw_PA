// Approval gate pada Store.
// Pesan keluar ditahan di approval_pending sampai disetujui via WhatsApp atau endpoint admin.
package db

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"pa-ai/api-gateway/src/model"
)

// ErrApprovalNotFound dikembalikan bila id approval tidak ada atau sudah diputuskan.
var ErrApprovalNotFound = errors.New("approval tidak ditemukan atau sudah diputuskan")

// CreateApproval menyimpan satu pesan keluar yang menunggu persetujuan SU.
// Mengembalikan id baris yang dibuat.
func (s *Store) CreateApproval(ctx context.Context, a model.Approval) (int64, error) {
	facts := a.NewFacts
	if len(facts) == 0 {
		facts = json.RawMessage("[]")
	}
	var contactID any
	if a.ContactID != nil && *a.ContactID > 0 {
		contactID = *a.ContactID
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO approval_pending
		    (conversation_id, agent_id, contact_id, target_chat, user_text,
		     response_text, approval_reason, new_facts)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id
	`, a.ConversationID, a.AgentID, contactID, a.TargetChat, a.UserText,
		a.ResponseText, a.ApprovalReason, facts).Scan(&id)
	return id, err
}

// GetApproval mengambil satu approval berdasarkan id (status apa pun).
func (s *Store) GetApproval(ctx context.Context, id int64) (*model.Approval, error) {
	return s.scanApproval(s.pool.QueryRow(ctx, `
		SELECT id, conversation_id, agent_id, COALESCE(contact_id,0), target_chat,
		       user_text, response_text, COALESCE(approval_reason,''), new_facts,
		       status, created_at, decided_at
		FROM approval_pending WHERE id = $1`, id))
}

// ListApprovals mengembalikan approval menurut status (kosong = semua), terbaru dulu.
func (s *Store) ListApprovals(ctx context.Context, status string, limit int) ([]model.Approval, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows pgx.Rows
	var err error
	base := `SELECT id, conversation_id, agent_id, COALESCE(contact_id,0), target_chat,
	                user_text, response_text, COALESCE(approval_reason,''), new_facts,
	                status, created_at, decided_at
	         FROM approval_pending`
	if status == "" {
		rows, err = s.pool.Query(ctx, base+` ORDER BY created_at DESC LIMIT $1`, limit)
	} else {
		rows, err = s.pool.Query(ctx, base+` WHERE status = $1 ORDER BY created_at DESC LIMIT $2`, status, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Approval
	for rows.Next() {
		a, err := s.scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// DecideApproval menandai approval sebagai approved/rejected dan mengembalikan
// baris yang diperbarui, atau ErrApprovalNotFound bila tidak ada atau sudah diputuskan.
func (s *Store) DecideApproval(ctx context.Context, id int64, status string) (*model.Approval, error) {
	a, err := s.scanApproval(s.pool.QueryRow(ctx, `
		UPDATE approval_pending
		   SET status = $2, decided_at = now()
		 WHERE id = $1 AND status = 'pending'
		RETURNING id, conversation_id, agent_id, COALESCE(contact_id,0), target_chat,
		          user_text, response_text, COALESCE(approval_reason,''), new_facts,
		          status, created_at, decided_at`, id, status))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrApprovalNotFound
	}
	return a, err
}

// UpdateApprovalResponse memperbarui teks pesan tertahan (response_text) sebuah approval
// yang MASIH pending — dipakai bila kesepakatan waktu meeting offline berubah setelah
// approval diajukan tetapi sebelum SU memutuskan. Mengembalikan ErrApprovalNotFound bila
// approval tidak ada atau sudah diputuskan (sehingga tidak menimpa keputusan final).
func (s *Store) UpdateApprovalResponse(ctx context.Context, id int64, responseText string) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE approval_pending SET response_text = $2
		 WHERE id = $1 AND status = 'pending'`, id, responseText)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrApprovalNotFound
	}
	return nil
}

// rowScanner menyatukan pgx.Row dan pgx.Rows untuk scan bersama.
type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanApproval(row rowScanner) (*model.Approval, error) {
	var a model.Approval
	var contactID int
	var facts []byte
	err := row.Scan(&a.ID, &a.ConversationID, &a.AgentID, &contactID, &a.TargetChat,
		&a.UserText, &a.ResponseText, &a.ApprovalReason, &facts,
		&a.Status, &a.CreatedAt, &a.DecidedAt)
	if err != nil {
		return nil, err
	}
	if contactID > 0 {
		a.ContactID = &contactID
	}
	a.NewFacts = json.RawMessage(facts)
	return &a, nil
}
