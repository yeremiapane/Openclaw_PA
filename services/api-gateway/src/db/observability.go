// observability & evaluasi.
// Operasi pada tabel agent_executions, outbound_messages, meeting_requests,
// dan meeting_status_history untuk trace token usage, pesan keluar, serta
// siklus hidup request meeting.
package db

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pa-ai/api-gateway/src/model"
)

// ── agent_executions ──────────────────────────────────────────────────────

// CreateExecution menyimpan satu giliran agent (sukses maupun gagal) dan
// mengembalikan id-nya. Dipakai untuk evaluasi model & trace jawaban.
func (s *Store) CreateExecution(ctx context.Context, e model.Execution) (int64, error) {
	actions := e.Actions
	if len(actions) == 0 {
		actions = json.RawMessage("[]")
	}
	facts := e.NewFacts
	if len(facts) == 0 {
		facts = json.RawMessage("[]")
	}
	var contactID any
	if e.ContactID != nil && *e.ContactID > 0 {
		contactID = *e.ContactID
	}
	if e.Outcome == "" {
		e.Outcome = "ok"
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO agent_executions
		    (run_id, oc_session_id, session_key, conversation_id, contact_id, agent_id,
		     provider, model, input_text, response_text, requires_approval, actions,
		     new_facts, finish_reason, stop_reason, refusal, input_tokens, output_tokens,
		     cache_read_tokens, cache_write_tokens, total_tokens, system_prompt_chars,
		     prompt_chars, duration_ms, fallback_used, runner, outcome, error_text, raw_meta)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,
		        $21,$22,$23,$24,$25,$26,$27,$28,$29)
		RETURNING id
	`, nullStr(e.RunID), nullStr(e.OCSessionID), nullStr(e.SessionKey),
		nullStr(e.ConversationID), contactID, e.AgentID, nullStr(e.Provider),
		nullStr(e.Model), nullStr(e.InputText), nullStr(e.ResponseText),
		e.RequiresApproval, actions, facts, nullStr(e.FinishReason),
		nullStr(e.StopReason), e.Refusal, e.InputTokens, e.OutputTokens,
		e.CacheReadTokens, e.CacheWriteTokens, e.TotalTokens,
		nullInt(e.SystemPromptChars), nullInt(e.PromptChars), nullInt(e.DurationMs),
		e.FallbackUsed, nullStr(e.Runner), e.Outcome, nullStr(e.ErrorText),
		nullRaw(e.RawMeta)).Scan(&id)
	return id, err
}

// ListExecutions mengembalikan eksekusi terbaru, opsional difilter per conversation.
func (s *Store) ListExecutions(ctx context.Context, convID string, limit int) ([]model.Execution, error) {
	if limit <= 0 {
		limit = 50
	}
	base := `SELECT id, COALESCE(run_id,''), COALESCE(oc_session_id,''), COALESCE(session_key,''),
	                COALESCE(conversation_id,''), COALESCE(contact_id,0), agent_id,
	                COALESCE(provider,''), COALESCE(model,''), COALESCE(input_text,''),
	                COALESCE(response_text,''), requires_approval, actions, new_facts,
	                COALESCE(finish_reason,''), COALESCE(stop_reason,''), refusal,
	                input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
	                total_tokens, COALESCE(system_prompt_chars,0), COALESCE(prompt_chars,0),
	                COALESCE(duration_ms,0), fallback_used, COALESCE(runner,''), outcome,
	                COALESCE(error_text,''), raw_meta, created_at
	         FROM agent_executions`
	var rows pgx.Rows
	var err error
	if convID == "" {
		rows, err = s.pool.Query(ctx, base+` ORDER BY created_at DESC LIMIT $1`, limit)
	} else {
		rows, err = s.pool.Query(ctx, base+` WHERE conversation_id = $1 ORDER BY created_at DESC LIMIT $2`, convID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Execution
	for rows.Next() {
		var e model.Execution
		var contactID int
		var actions, facts, raw []byte
		if err := rows.Scan(&e.ID, &e.RunID, &e.OCSessionID, &e.SessionKey,
			&e.ConversationID, &contactID, &e.AgentID, &e.Provider, &e.Model,
			&e.InputText, &e.ResponseText, &e.RequiresApproval, &actions, &facts,
			&e.FinishReason, &e.StopReason, &e.Refusal, &e.InputTokens, &e.OutputTokens,
			&e.CacheReadTokens, &e.CacheWriteTokens, &e.TotalTokens, &e.SystemPromptChars,
			&e.PromptChars, &e.DurationMs, &e.FallbackUsed, &e.Runner, &e.Outcome,
			&e.ErrorText, &raw, &e.CreatedAt); err != nil {
			return nil, err
		}
		if contactID > 0 {
			e.ContactID = &contactID
		}
		e.Actions = json.RawMessage(actions)
		e.NewFacts = json.RawMessage(facts)
		if len(raw) > 0 {
			e.RawMeta = json.RawMessage(raw)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ConversationUsage = agregat token & biaya per conversation (untuk evaluasi).
type ConversationUsage struct {
	ConversationID   string `json:"conversation_id"`
	AgentID          string `json:"agent_id"`
	Turns            int    `json:"turns"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	TotalDurationMs  int64  `json:"total_duration_ms"`
}

// UsageByConversation menjumlahkan token per conversation (opsional difilter satu conv).
func (s *Store) UsageByConversation(ctx context.Context, convID string, limit int) ([]ConversationUsage, error) {
	if limit <= 0 {
		limit = 100
	}
	base := `SELECT conversation_id, COALESCE(max(agent_id),''), count(*),
	                COALESCE(sum(input_tokens),0), COALESCE(sum(output_tokens),0),
	                COALESCE(sum(cache_read_tokens),0), COALESCE(sum(cache_write_tokens),0),
	                COALESCE(sum(total_tokens),0), COALESCE(sum(duration_ms),0)
	         FROM agent_executions`
	var rows pgx.Rows
	var err error
	if convID == "" {
		rows, err = s.pool.Query(ctx, base+` GROUP BY conversation_id ORDER BY sum(total_tokens) DESC LIMIT $1`, limit)
	} else {
		rows, err = s.pool.Query(ctx, base+` WHERE conversation_id = $1 GROUP BY conversation_id`, convID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConversationUsage
	for rows.Next() {
		var u ConversationUsage
		if err := rows.Scan(&u.ConversationID, &u.AgentID, &u.Turns, &u.InputTokens,
			&u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens, &u.TotalTokens,
			&u.TotalDurationMs); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ── outbound_messages ─────────────────────────────────────────────────────

// LogOutbound mencatat satu pesan yang dikirim (atau ditahan/gagal) oleh bot.
func (s *Store) LogOutbound(ctx context.Context, o model.OutboundMessage) (int64, error) {
	if o.Status == "" {
		o.Status = "sent"
	}
	var contactID any
	if o.ContactID != nil && *o.ContactID > 0 {
		contactID = *o.ContactID
	}
	var execID, approvalID any
	if o.ExecutionID != nil && *o.ExecutionID > 0 {
		execID = *o.ExecutionID
	}
	if o.ApprovalID != nil && *o.ApprovalID > 0 {
		approvalID = *o.ApprovalID
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO outbound_messages
		    (execution_id, conversation_id, contact_id, agent_id, target_chat,
		     kind, text, status, approval_id, error_text)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING id
	`, execID, nullStr(o.ConversationID), contactID, nullStr(o.AgentID),
		o.TargetChat, o.Kind, o.Text, o.Status, approvalID,
		nullStr(o.ErrorText)).Scan(&id)
	return id, err
}

// ListOutbound mengembalikan pesan keluar terbaru (opsional difilter per conversation).
func (s *Store) ListOutbound(ctx context.Context, convID string, limit int) ([]model.OutboundMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	base := `SELECT id, COALESCE(execution_id,0), COALESCE(conversation_id,''),
	                COALESCE(contact_id,0), COALESCE(agent_id,''), target_chat, kind,
	                text, status, COALESCE(approval_id,0), COALESCE(error_text,''), created_at
	         FROM outbound_messages`
	var rows pgx.Rows
	var err error
	if convID == "" {
		rows, err = s.pool.Query(ctx, base+` ORDER BY created_at DESC LIMIT $1`, limit)
	} else {
		rows, err = s.pool.Query(ctx, base+` WHERE conversation_id = $1 ORDER BY created_at DESC LIMIT $2`, convID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.OutboundMessage
	for rows.Next() {
		var o model.OutboundMessage
		var execID, approvalID int64
		var contactID int
		if err := rows.Scan(&o.ID, &execID, &o.ConversationID, &contactID, &o.AgentID,
			&o.TargetChat, &o.Kind, &o.Text, &o.Status, &approvalID, &o.ErrorText,
			&o.CreatedAt); err != nil {
			return nil, err
		}
		if execID > 0 {
			o.ExecutionID = &execID
		}
		if approvalID > 0 {
			o.ApprovalID = &approvalID
		}
		if contactID > 0 {
			o.ContactID = &contactID
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ── meeting_requests + meeting_status_history ─────────────────────────────

// CreateMeetingRequest membuat satu request meeting (status awal 'pending') dan
// menulis entri riwayat status pertama. Mengembalikan id meeting.
func (s *Store) CreateMeetingRequest(ctx context.Context, m model.MeetingRequest, changedBy string) (int64, error) {
	if m.Status == "" {
		m.Status = "pending"
	}
	if m.RequestedVia == "" {
		m.RequestedVia = "external"
	}
	details := m.Details
	if len(details) == 0 {
		details = json.RawMessage("{}")
	}
	var contactID, approvalID any
	if m.ContactID != nil && *m.ContactID > 0 {
		contactID = *m.ContactID
	}
	if m.ApprovalID != nil && *m.ApprovalID > 0 {
		approvalID = *m.ApprovalID
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO meeting_requests
		    (conversation_id, contact_id, agent_id, approval_id, requested_via,
		     external_name, external_company, topic, meeting_type, proposed_datetime,
		     venue, status, details)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING id
	`, nullStr(m.ConversationID), contactID, m.AgentID, approvalID, m.RequestedVia,
		nullStr(m.ExternalName), nullStr(m.ExternalCompany), nullStr(m.Topic),
		nullStr(m.MeetingType), m.ProposedDatetime, nullStr(m.Venue), m.Status,
		details).Scan(&id)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO meeting_status_history (meeting_id, from_status, to_status, changed_by, reason)
		VALUES ($1, NULL, $2, $3, 'dibuat dari approval gate')
	`, id, m.Status, nullStr(changedBy)); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// UpdateMeetingStatus mengubah status meeting + mencatat transisi ke riwayat.
// Menyetel kolom timestamp yang sesuai (approved_at/rejected_at/scheduled_at).
func (s *Store) UpdateMeetingStatus(ctx context.Context, meetingID int64, toStatus, changedBy, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var fromStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM meeting_requests WHERE id = $1`, meetingID).Scan(&fromStatus); err != nil {
		return err
	}

	tsCol := ""
	switch toStatus {
	case "approved":
		tsCol = ", approved_at = now()"
	case "rejected":
		tsCol = ", rejected_at = now()"
	case "scheduled":
		tsCol = ", scheduled_at = now()"
	}
	if _, err := tx.Exec(ctx, `UPDATE meeting_requests SET status = $2`+tsCol+` WHERE id = $1`, meetingID, toStatus); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO meeting_status_history (meeting_id, from_status, to_status, changed_by, reason)
		VALUES ($1,$2,$3,$4,$5)
	`, meetingID, fromStatus, toStatus, nullStr(changedBy), nullStr(reason)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpdateMeetingStatusByApproval memetakan keputusan approval ke meeting terkait
// (approval_id). Tanpa error bila tidak ada meeting tertaut.
func (s *Store) UpdateMeetingStatusByApproval(ctx context.Context, approvalID int64, toStatus, changedBy, reason string) error {
	var meetingID int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM meeting_requests WHERE approval_id = $1 ORDER BY id DESC LIMIT 1`, approvalID).Scan(&meetingID)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return s.UpdateMeetingStatus(ctx, meetingID, toStatus, changedBy, reason)
}

// MeetingByApproval mengambil satu meeting tertaut approval (nil bila tidak ada).
func (s *Store) MeetingByApproval(ctx context.Context, approvalID int64) (*model.MeetingRequest, error) {
	var m model.MeetingRequest
	var contactID int
	var apID int64
	var details []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, COALESCE(conversation_id,''), COALESCE(contact_id,0), agent_id,
		       COALESCE(approval_id,0), requested_via, COALESCE(external_name,''),
		       COALESCE(external_company,''), COALESCE(topic,''), COALESCE(meeting_type,''),
		       proposed_datetime, COALESCE(venue,''), status, details,
		       created_at, updated_at, approved_at, rejected_at, scheduled_at
		FROM meeting_requests WHERE approval_id = $1 ORDER BY id DESC LIMIT 1
	`, approvalID).Scan(&m.ID, &m.ConversationID, &contactID, &m.AgentID, &apID,
		&m.RequestedVia, &m.ExternalName, &m.ExternalCompany, &m.Topic, &m.MeetingType,
		&m.ProposedDatetime, &m.Venue, &m.Status, &details, &m.CreatedAt, &m.UpdatedAt,
		&m.ApprovedAt, &m.RejectedAt, &m.ScheduledAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if contactID > 0 {
		m.ContactID = &contactID
	}
	if apID > 0 {
		m.ApprovalID = &apID
	}
	m.Details = json.RawMessage(details)
	return &m, nil
}

// meetingScanCols = daftar kolom standar untuk memindai satu meeting_requests.
const meetingScanCols = `id, COALESCE(conversation_id,''), COALESCE(contact_id,0), agent_id,
	COALESCE(approval_id,0), requested_via, COALESCE(external_name,''),
	COALESCE(external_company,''), COALESCE(topic,''), COALESCE(meeting_type,''),
	proposed_datetime, COALESCE(venue,''), status, details,
	created_at, updated_at, approved_at, rejected_at, scheduled_at`

// scanMeetingRow memindai satu baris meeting_requests (urutan kolom = meetingScanCols).
func scanMeetingRow(row pgx.Row) (*model.MeetingRequest, error) {
	var m model.MeetingRequest
	var contactID int
	var apID int64
	var details []byte
	err := row.Scan(&m.ID, &m.ConversationID, &contactID, &m.AgentID, &apID,
		&m.RequestedVia, &m.ExternalName, &m.ExternalCompany, &m.Topic, &m.MeetingType,
		&m.ProposedDatetime, &m.Venue, &m.Status, &details, &m.CreatedAt, &m.UpdatedAt,
		&m.ApprovedAt, &m.RejectedAt, &m.ScheduledAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if contactID > 0 {
		m.ContactID = &contactID
	}
	if apID > 0 {
		m.ApprovalID = &apID
	}
	m.Details = json.RawMessage(details)
	return &m, nil
}

// MeetingByID mengambil satu meeting berdasarkan id (nil,nil bila tak ada). Dipakai
// jalur reschedule/cancel yang merujuk meeting lewat meetingId dari snapshot.
func (s *Store) MeetingByID(ctx context.Context, id int64) (*model.MeetingRequest, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+meetingScanCols+`
		FROM meeting_requests WHERE id = $1`, id)
	return scanMeetingRow(row)
}

// ActiveMeetingByConversation mengembalikan meeting aktif terbaru per percakapan,
// atau (nil,nil) bila tak ada. Dipakai SPAWN_AGENT agar follow-up berulang dari SU
// tidak menghasilkan baris meeting duplikat; rencananya diperbarui pada baris yang sama.
func (s *Store) ActiveMeetingByConversation(ctx context.Context, convID string) (*model.MeetingRequest, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+meetingScanCols+`
		FROM meeting_requests
		WHERE conversation_id = $1 AND status IN ('pending','approved','scheduled')
		ORDER BY id DESC LIMIT 1`, convID)
	return scanMeetingRow(row)
}

// FindPendingSpawnMeeting mencari proposal meeting SPAWN milik SU yang masih pending
// dan belum tertaut approval, cocok berdasarkan nama dan, jika ada, perusahaan.
// Dipakai untuk mencegah duplikasi saat direkonsiliasi dengan meeting final.
func (s *Store) FindPendingSpawnMeeting(ctx context.Context, name, company string) (*model.MeetingRequest, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	q := `SELECT ` + meetingScanCols + `
		FROM meeting_requests
		WHERE requested_via = 'su' AND approval_id IS NULL AND status = 'pending'
		  AND lower(COALESCE(external_name,'')) = lower($1)`
	args := []any{name}
	if c := strings.TrimSpace(company); c != "" {
		q += ` AND lower(COALESCE(external_company,'')) = lower($2)`
		args = append(args, c)
	}
	q += ` ORDER BY id DESC LIMIT 1`
	return scanMeetingRow(s.pool.QueryRow(ctx, q, args...))
}

// FindPendingSpawnMeetingByConversation mencari proposal meeting SPAWN milik SU yang masih
// pending & belum tertaut approval, di PERCAKAPAN yang sama. Lebih tahan dari pencocokan
// nama (nama pihak eksternal kerap berubah: ditambah gelar/"Pak", dsb.) karena proposal
// SPAWN SU dan meeting final dari balasan eksternal selalu berbagi conversation_id yang
// sama. Dipakai sbg fallback rekonsiliasi agar meeting SU-initiated tak salah label
// 'external' (yang menyebabkan undangan email tak dikirim).
func (s *Store) FindPendingSpawnMeetingByConversation(ctx context.Context, convID string) (*model.MeetingRequest, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return nil, nil
	}
	row := s.pool.QueryRow(ctx, `SELECT `+meetingScanCols+`
		FROM meeting_requests
		WHERE requested_via = 'su' AND approval_id IS NULL AND status = 'pending'
		  AND conversation_id = $1
		ORDER BY id DESC LIMIT 1`, convID)
	return scanMeetingRow(row)
}

// MeetingsByGroup mengembalikan seluruh baris meeting AKTIF (belum terminal) yang
// berbagi groupId (Fase 1 meeting grup), tanpa proposal SPAWN yang sudah di-superseded.
// Diurut kronologis (id ASC). Kosong bila groupID kosong atau tak ada yang cocok.
func (s *Store) MeetingsByGroup(ctx context.Context, groupID string) ([]*model.MeetingRequest, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+meetingScanCols+`
		FROM meeting_requests
		WHERE COALESCE(details->>'groupId','') = $1
		  AND status NOT IN ('superseded','cancelled')
		ORDER BY id ASC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.MeetingRequest
	for rows.Next() {
		m, serr := scanMeetingRow(rows)
		if serr != nil {
			return nil, serr
		}
		if m != nil {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

// GroupAgreedCount menghitung jumlah peserta grup yang SUDAH menyepakati (baris pending
// yang telah tertaut approval — sinyal "sudah setuju & siap diajukan ke SU"). Dipakai
// untuk menahan notifikasi SU hingga semua peserta setuju.
func (s *Store) GroupAgreedCount(ctx context.Context, groupID string) (int, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return 0, nil
	}
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*)
		FROM meeting_requests
		WHERE COALESCE(details->>'groupId','') = $1
		  AND approval_id IS NOT NULL
		  AND status IN ('pending','approved','scheduled')`, groupID).Scan(&n)
	return n, err
}

// MarkGroupApprovalNotified menandai (sekali) bahwa notifikasi persetujuan grup gabungan
// sudah dikirim ke SU, dan mengembalikan true HANYA untuk pemanggil pertama. Aman terhadap
// balapan: UPDATE bersyarat pada seluruh baris grup diserialkan oleh row-lock Postgres —
// pemanggil kedua melihat flag sudah ter-set (0 baris terpengaruh). Mencegah SU menerima
// dua prompt approval saat peserta terakhir menyetujui hampir bersamaan.
func (s *Store) MarkGroupApprovalNotified(ctx context.Context, groupID string) (bool, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return false, nil
	}
	ct, err := s.pool.Exec(ctx, `
		UPDATE meeting_requests
		SET details = jsonb_set(details, '{groupApprovalNotified}', 'true'::jsonb)
		WHERE COALESCE(details->>'groupId','') = $1
		  AND COALESCE(details->>'groupApprovalNotified','') <> 'true'
		  AND status NOT IN ('superseded','cancelled')`, groupID)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

// ResetGroupAgreementForReschedule mengembalikan SELURUH peserta grup ke kondisi "belum
// disepakati" untuk negosiasi ulang (KONVERGENSI Fase 2): melepas tautan approval (approval_id
// → NULL) sehingga GroupAgreedCount ter-reset, membatalkan approval lama yang masih pending
// (agar balasan SETUJU basi tidak menembak pesan usang), dan menurunkan bendera kesepakatan
// (timeAgreed/venueConfirmed/groupApprovalNotified) sehingga gerbang notifikasi grup menyala
// kembali hanya setelah semua peserta menyepakati waktu baru. Dijalankan dalam satu transaksi.
func (s *Store) ResetGroupAgreementForReschedule(ctx context.Context, groupID string) error {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Batalkan approval lama (masih pending) yang tertaut peserta grup aktif.
	if _, err := tx.Exec(ctx, `
		UPDATE approval_pending SET status = 'rejected', decided_at = now()
		WHERE status = 'pending'
		  AND id IN (
		      SELECT approval_id FROM meeting_requests
		      WHERE COALESCE(details->>'groupId','') = $1
		        AND approval_id IS NOT NULL
		        AND status IN ('pending','approved','scheduled')
		  )`, groupID); err != nil {
		return err
	}
	// Lepas tautan approval + reset bendera kesepakatan pada peserta grup aktif.
	if _, err := tx.Exec(ctx, `
		UPDATE meeting_requests
		SET approval_id = NULL,
		    details = jsonb_set(
		                jsonb_set(
		                  jsonb_set(details, '{timeAgreed}', 'false'::jsonb),
		                  '{venueConfirmed}', 'false'::jsonb),
		                '{groupApprovalNotified}', 'false'::jsonb),
		    updated_at = now()
		WHERE COALESCE(details->>'groupId','') = $1
		  AND status IN ('pending','approved','scheduled')`, groupID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DetachMeetingFromGroup melepaskan SATU peserta dari grup (SPLIT Fase 2): peserta itu menjadi
// meeting solo (kunci groupId/groupSize dihapus dari details) dan groupSize peserta grup
// yang tersisa diturunkan satu. Mengembalikan jumlah peserta grup yang masih tersisa (aktif)
// setelah pelepasan — pemanggil memakainya untuk memutuskan apakah sisa grup kini lengkap
// (bisa dinotifikasikan) atau menyusut menjadi solo. Dijalankan dalam satu transaksi.
func (s *Store) DetachMeetingFromGroup(ctx context.Context, meetingID int64, groupID string) (int, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	// Peserta yang dilepas menjadi solo — buang kunci grup dari details-nya.
	if _, err := tx.Exec(ctx, `
		UPDATE meeting_requests
		SET details = (details - 'groupId' - 'groupSize' - 'groupApprovalNotified'),
		    updated_at = now()
		WHERE id = $1`, meetingID); err != nil {
		return 0, err
	}
	// Turunkan groupSize peserta grup yang tersisa (minimal 1) DAN reset bendera
	// groupApprovalNotified — komposisi grup berubah, jadi gerbang notifikasi sisa grup
	// harus menyala kembali (mis. bila divergensi awal sudah menandainya lebih dulu).
	if _, err := tx.Exec(ctx, `
		UPDATE meeting_requests
		SET details = jsonb_set(
		                jsonb_set(details, '{groupSize}',
		                  to_jsonb(GREATEST(COALESCE((details->>'groupSize')::int,1) - 1, 1))),
		                '{groupApprovalNotified}', 'false'::jsonb),
		    updated_at = now()
		WHERE COALESCE(details->>'groupId','') = $1
		  AND id <> $2
		  AND status IN ('pending','approved','scheduled')`, groupID, meetingID); err != nil {
		return 0, err
	}
	var remaining int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM meeting_requests
		WHERE COALESCE(details->>'groupId','') = $1
		  AND id <> $2
		  AND status IN ('pending','approved','scheduled')`, groupID, meetingID).Scan(&remaining); err != nil {
		return 0, err
	}
	return remaining, tx.Commit(ctx)
}

// FindActiveMeetingByPartyAt mencari meeting AKTIF pada datetime dan pihak eksternal
// yang sama di percakapan berbeda untuk mencegah duplikasi approval/meeting.
func (s *Store) FindActiveMeetingByPartyAt(ctx context.Context, name string, at time.Time, excludeConvID string) (*model.MeetingRequest, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	row := s.pool.QueryRow(ctx, `SELECT `+meetingScanCols+`
		FROM meeting_requests
		WHERE status IN ('pending','approved','scheduled')
		  AND proposed_datetime = $2
		  AND COALESCE(conversation_id,'') <> $3
		  AND ( lower(COALESCE(external_name,'')) = lower($1)
		        OR lower(COALESCE(details->>'attendeeName','')) = lower($1) )
		ORDER BY id ASC LIMIT 1`, name, at, excludeConvID)
	return scanMeetingRow(row)
}

// FindGroupableSpawnSibling mencari meeting SU-initiated lain (di percakapan BERBEDA) yang
// bertopik dan berwaktu SAMA PERSIS — kandidat untuk dikonsolidasikan menjadi satu meeting
// grup.
func (s *Store) FindGroupableSpawnSibling(ctx context.Context, topic string, at time.Time, excludeConvID string) (*model.MeetingRequest, error) {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return nil, nil
	}
	row := s.pool.QueryRow(ctx, `SELECT `+meetingScanCols+`
		FROM meeting_requests
		WHERE requested_via = 'su'
		  AND status IN ('pending','approved','scheduled')
		  AND proposed_datetime IS NOT NULL
		  AND date_trunc('minute', proposed_datetime) = date_trunc('minute', $2::timestamptz)
		  AND lower(COALESCE(topic,'')) = lower($1)
		  AND COALESCE(conversation_id,'') <> $3
		ORDER BY id ASC LIMIT 1`, topic, at.UTC(), excludeConvID)
	return scanMeetingRow(row)
}

// AttachMeetingsToGroup menautkan baris meeting (setIDs) ke groupID, lalu MENYELARASKAN
// groupSize seluruh anggota grup yang masih aktif ke jumlah anggota aktual, dan mereset
// groupApprovalNotified=false (komposisi grup berubah → gerbang notifikasi grup harus
// menyala kembali; konsisten dengan DetachMeetingFromGroup).
func (s *Store) AttachMeetingsToGroup(ctx context.Context, groupID string, setIDs []int64) (int, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" || len(setIDs) == 0 {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		UPDATE meeting_requests
		SET details = jsonb_set(COALESCE(details,'{}'::jsonb), '{groupId}', to_jsonb($1::text)),
		    updated_at = now()
		WHERE id = ANY($2::bigint[])`, groupID, setIDs); err != nil {
		return 0, err
	}
	var size int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM meeting_requests
		WHERE COALESCE(details->>'groupId','') = $1
		  AND status IN ('pending','approved','scheduled')`, groupID).Scan(&size); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE meeting_requests
		SET details = jsonb_set(
		                jsonb_set(details, '{groupSize}', to_jsonb($2::int)),
		                '{groupApprovalNotified}', 'false'::jsonb),
		    updated_at = now()
		WHERE COALESCE(details->>'groupId','') = $1
		  AND status IN ('pending','approved','scheduled')`, groupID, size); err != nil {
		return 0, err
	}
	return size, tx.Commit(ctx)
}

// venuePendingWhere: meeting offline yang menunggu konfirmasi lokasi.
// Syarat: venueCoordination=true, venueConfirmed!=true, dan status belum terminal.
const venuePendingWhere = `COALESCE(details->>'venueCoordination','') = 'true'
	AND COALESCE(details->>'venueConfirmed','') <> 'true'
	AND status NOT IN ('cancelled','superseded','completed','rejected')`

// FindVenuePendingMeeting mengembalikan satu meeting offline terbaru yang menunggu venue.
// Dipakai untuk konfirmasi tanpa tanggal; kembalikan nil bila tidak ada.
func (s *Store) FindVenuePendingMeeting(ctx context.Context) (*model.MeetingRequest, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+meetingScanCols+`
		FROM meeting_requests
		WHERE `+venuePendingWhere+`
		ORDER BY updated_at DESC, id DESC LIMIT 1`)
	return scanMeetingRow(row)
}

// FindVenuePendingMeetingByDate mencari meeting offline yang menunggu venue pada tanggal WIB.
// Untuk pencocokan; wibDate format "2006-01-02". proposed_datetime disimpan UTC dan
// dikonversi ke WIB (+7) untuk perbandingan.
func (s *Store) FindVenuePendingMeetingByDate(ctx context.Context, wibDate string) (*model.MeetingRequest, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+meetingScanCols+`
		FROM meeting_requests
		WHERE `+venuePendingWhere+`
		  AND proposed_datetime IS NOT NULL
		  AND ((proposed_datetime AT TIME ZONE 'UTC') + interval '7 hours')::date = $1::date
		ORDER BY updated_at DESC, id DESC LIMIT 1`, wibDate)
	return scanMeetingRow(row)
}

// FindVenuePendingMeetingByDatetime mencari meeting offline yang menunggu venue dengan
// proposed_datetime SAMA PERSIS (presisi menit) dengan waktu yang dibawa CONFIRM_VENUE.
func (s *Store) FindVenuePendingMeetingByDatetime(ctx context.Context, t time.Time) (*model.MeetingRequest, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+meetingScanCols+`
		FROM meeting_requests
		WHERE `+venuePendingWhere+`
		  AND proposed_datetime IS NOT NULL
		  AND date_trunc('minute', proposed_datetime) = date_trunc('minute', $1::timestamptz)
		ORDER BY updated_at DESC, id DESC LIMIT 1`, t.UTC())
	return scanMeetingRow(row)
}

// VenueSuggestion = satu lokasi yang pernah dipakai untuk meeting, beserta seberapa sering
// & alamat terakhir yang tercatat. Dipakai untuk merekomendasikan venue familiar ke Bu Nova.
type VenueSuggestion struct {
	Name    string
	Address string
	Times   int
}

// venueNameExpr = ekspresi SQL untuk NAMA venue sebuah meeting: pakai details->>'venueName'
// bila ada, jatuh ke kolom venue (yang mungkin berisi "Nama — Alamat"). venueAddrExpr =
// alamat terpisah bila tercatat. Keduanya dipakai query riwayat venue di bawah.
const venueNameExpr = `COALESCE(NULLIF(details->>'venueName',''), NULLIF(venue,''))`
const venueAddrExpr = `COALESCE(details->>'venueAddress','')`

// scanVenueSuggestions memindai baris (name, address, times) dari query riwayat venue.
func scanVenueSuggestions(rows pgx.Rows) ([]VenueSuggestion, error) {
	defer rows.Close()
	var out []VenueSuggestion
	for rows.Next() {
		var v VenueSuggestion
		if err := rows.Scan(&v.Name, &v.Address, &v.Times); err != nil {
			return nil, err
		}
		v.Name = strings.TrimSpace(v.Name)
		v.Address = strings.TrimSpace(v.Address)
		if v.Name != "" {
			out = append(out, v)
		}
	}
	return out, rows.Err()
}

// VenueHistoryForExternal mengembalikan lokasi yang PERNAH dipakai untuk meeting offline
// (status 'scheduled'/'completed') dengan pihak eksternal yang SAMA (cocok nama, tak peka
// huruf), diurut dari yang paling sering lalu paling baru. Dipakai untuk menyarankan venue
// familiar. Nama kosong → daftar kosong (bukan error).
func (s *Store) VenueHistoryForExternal(ctx context.Context, externalName string, limit int) ([]VenueSuggestion, error) {
	externalName = strings.TrimSpace(externalName)
	if externalName == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 3
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+venueNameExpr+` AS vname,
		       MAX(`+venueAddrExpr+`) AS vaddr,
		       COUNT(*)::int AS times
		FROM meeting_requests
		WHERE status IN ('scheduled','completed')
		  AND `+venueNameExpr+` IS NOT NULL
		  AND lower(COALESCE(external_name,'')) = lower($1)
		GROUP BY `+venueNameExpr+`
		ORDER BY times DESC, MAX(updated_at) DESC
		LIMIT $2`, externalName, limit)
	if err != nil {
		return nil, err
	}
	return scanVenueSuggestions(rows)
}

// FrequentVenues mengembalikan lokasi yang paling sering dipakai LINTAS semua pihak eksternal
// (status 'scheduled'/'completed'), diurut paling sering lalu paling baru. Dipakai sebagai
// cadangan rekomendasi bila belum ada riwayat dengan pihak eksternal tertentu.
func (s *Store) FrequentVenues(ctx context.Context, limit int) ([]VenueSuggestion, error) {
	if limit <= 0 {
		limit = 3
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+venueNameExpr+` AS vname,
		       MAX(`+venueAddrExpr+`) AS vaddr,
		       COUNT(*)::int AS times
		FROM meeting_requests
		WHERE status IN ('scheduled','completed')
		  AND `+venueNameExpr+` IS NOT NULL
		GROUP BY `+venueNameExpr+`
		ORDER BY times DESC, MAX(updated_at) DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return scanVenueSuggestions(rows)
}

// FindUnfinalizedOfflineMeetings mengembalikan meeting offline yang WAKTU-nya sudah
// disetujui SU (status 'approved') DAN lokasinya sudah dikonfirmasi (venueConfirmed=true)
// namun BELUM difinalisasi menjadi 'scheduled' — mis. karena proses finalisasi terputus.
// Dipakai reconciler saat start-up untuk menuntaskan finalisasi yang tertahan. Diurutkan
// lama→baru agar deterministik.
func (s *Store) FindUnfinalizedOfflineMeetings(ctx context.Context) ([]model.MeetingRequest, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+meetingScanCols+`
		FROM meeting_requests
		WHERE COALESCE(details->>'venueCoordination','') = 'true'
		  AND COALESCE(details->>'timeAgreed','') = 'true'
		  AND COALESCE(details->>'venueConfirmed','') = 'true'
		  AND status = 'approved'
		ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.MeetingRequest
	for rows.Next() {
		m, err := scanMeetingRow(rows)
		if err != nil {
			return nil, err
		}
		if m != nil {
			out = append(out, *m)
		}
	}
	return out, rows.Err()
}

// ActiveVenueMeetingAwaitingTime mengembalikan meeting offline pada percakapan ini yang
// masih MENUNGGU kesepakatan WAKTU dari pihak eksternal (venueCoordination=true,
// timeAgreed!=true, status 'pending'), atau (nil,nil). Dipakai gateway untuk menyuntik
// penegasan ke PA Communicator agar ketika pihak eksternal menyetujui waktu, agent
// menandai kesepakatan lewat sinyal terstruktur (requiresApproval + objek meeting).
func (s *Store) ActiveVenueMeetingAwaitingTime(ctx context.Context, convID string) (*model.MeetingRequest, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+meetingScanCols+`
		FROM meeting_requests
		WHERE conversation_id = $1
		  AND status = 'pending'
		  AND COALESCE(details->>'venueCoordination','') = 'true'
		  AND COALESCE(details->>'timeAgreed','') <> 'true'
		ORDER BY id DESC LIMIT 1`, convID)
	return scanMeetingRow(row)
}

// LinkMeetingApproval menetapkan approval_id tanpa merubah status.
// Dipakai saat membuat approval final; juga mencatat riwayat.
func (s *Store) LinkMeetingApproval(ctx context.Context, id, approvalID int64, changedBy, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM meeting_requests WHERE id = $1`, id).Scan(&status); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE meeting_requests SET approval_id = $2, updated_at = now() WHERE id = $1`,
		id, approvalID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO meeting_status_history (meeting_id, from_status, to_status, changed_by, reason)
		VALUES ($1,$2,$2,$3,$4)
	`, id, status, nullStr(changedBy), nullStr(reason)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpdateMeetingPlan memperbarui data rencana meeting tanpa mengubah status.
// Nilai kosong/nil tidak menimpa data lama. Riwayat tetap dicatat.
func (s *Store) UpdateMeetingPlan(ctx context.Context, id int64, topic, meetingType, venue string, proposed *time.Time, changedBy, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM meeting_requests WHERE id = $1`, id).Scan(&status); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE meeting_requests SET
			topic             = COALESCE(NULLIF($2,''), topic),
			meeting_type      = COALESCE(NULLIF($3,''), meeting_type),
			venue             = CASE WHEN $4 <> '' THEN $4 ELSE venue END,
			proposed_datetime = COALESCE($5, proposed_datetime),
			updated_at        = now()
		WHERE id = $1
	`, id, topic, meetingType, venue, proposed); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO meeting_status_history (meeting_id, from_status, to_status, changed_by, reason)
		VALUES ($1,$2,$2,$3,$4)
	`, id, status, nullStr(changedBy), nullStr(reason)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpdateMeetingDetails memperbarui kolom JSON details sebuah meeting TANPA mengubah
// status/jadwal (mis. menandai reschedulePending). Mencatat riwayat (from=to=status).
func (s *Store) UpdateMeetingDetails(ctx context.Context, id int64, details json.RawMessage, changedBy, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM meeting_requests WHERE id = $1`, id).Scan(&status); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE meeting_requests SET details = $2, updated_at = now() WHERE id = $1`,
		id, nullRaw(details)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO meeting_status_history (meeting_id, from_status, to_status, changed_by, reason)
		VALUES ($1,$2,$2,$3,$4)
	`, id, status, nullStr(changedBy), nullStr(reason)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RelinkMeetingForReschedule menautkan meeting ke approval reschedule baru dengan status 'pending'.
// Memperbarui approval_id, proposed_datetime, venue, dan details tanpa membuat duplikat.
func (s *Store) RelinkMeetingForReschedule(ctx context.Context, id, approvalID int64, proposed *time.Time, venue string, details json.RawMessage, changedBy, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM meeting_requests WHERE id = $1`, id).Scan(&status); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE meeting_requests SET
			approval_id       = $2,
			proposed_datetime = COALESCE($3, proposed_datetime),
			venue             = CASE WHEN $4 <> '' THEN $4 ELSE venue END,
			details           = $5,
			status            = 'pending',
			updated_at        = now()
		WHERE id = $1
	`, id, approvalID, proposed, venue, nullRaw(details)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO meeting_status_history (meeting_id, from_status, to_status, changed_by, reason)
		VALUES ($1,$2,'pending',$3,$4)
	`, id, status, nullStr(changedBy), nullStr(reason)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReopenMeetingForReschedule mengembalikan meeting offline ke jalur (re)koordinasi:
// melepas tautan approval lama (approval_id = NULL) agar persetujuan waktu baru dapat
// diajukan ulang, mengembalikan status ke 'pending', dan mengganti details dengan
// penanda reschedule (reschedulePending/rescheduleFrom + venueConfirmed/timeAgreed=false).
// Event kalender lama TETAP (di-PATCH saat finalisasi), jadi eventId disimpan di details.
func (s *Store) ReopenMeetingForReschedule(ctx context.Context, id int64, details json.RawMessage, changedBy, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM meeting_requests WHERE id = $1`, id).Scan(&status); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE meeting_requests SET
			approval_id = NULL,
			details     = $2,
			status      = 'pending',
			updated_at  = now()
		WHERE id = $1
	`, id, nullRaw(details)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO meeting_status_history (meeting_id, from_status, to_status, changed_by, reason)
		VALUES ($1,$2,'pending',$3,$4)
	`, id, status, nullStr(changedBy), nullStr(reason)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ScheduleMeeting menandai meeting 'scheduled' (set scheduled_at), memperbarui
// details (mis. eventId/calendarLink/teamsLink), dan menulis riwayat transisi.
func (s *Store) ScheduleMeeting(ctx context.Context, meetingID int64, details json.RawMessage, changedBy, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var fromStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM meeting_requests WHERE id = $1`, meetingID).Scan(&fromStatus); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE meeting_requests SET status = 'scheduled', scheduled_at = now(), details = $2
		WHERE id = $1
	`, meetingID, nullRaw(details)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO meeting_status_history (meeting_id, from_status, to_status, changed_by, reason)
		VALUES ($1,$2,'scheduled',$3,$4)
	`, meetingID, fromStatus, nullStr(changedBy), nullStr(reason)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListMeetings mengembalikan request meeting terbaru (opsional difilter per status).
func (s *Store) ListMeetings(ctx context.Context, status string, limit int) ([]model.MeetingRequest, error) {
	if limit <= 0 {
		limit = 50
	}
	base := `SELECT id, COALESCE(conversation_id,''), COALESCE(contact_id,0), agent_id,
	                COALESCE(approval_id,0), requested_via, COALESCE(external_name,''),
	                COALESCE(external_company,''), COALESCE(topic,''), COALESCE(meeting_type,''),
	                proposed_datetime, COALESCE(venue,''), status, details,
	                created_at, updated_at, approved_at, rejected_at, scheduled_at
	         FROM meeting_requests`
	var rows pgx.Rows
	var err error
	if status == "" {
		rows, err = s.pool.Query(ctx, base+` ORDER BY created_at DESC LIMIT $1`, limit)
	} else {
		rows, err = s.pool.Query(ctx, base+` WHERE status = $1 ORDER BY created_at DESC LIMIT $2`, status, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.MeetingRequest
	for rows.Next() {
		var m model.MeetingRequest
		var contactID int
		var approvalID int64
		var details []byte
		if err := rows.Scan(&m.ID, &m.ConversationID, &contactID, &m.AgentID, &approvalID,
			&m.RequestedVia, &m.ExternalName, &m.ExternalCompany, &m.Topic, &m.MeetingType,
			&m.ProposedDatetime, &m.Venue, &m.Status, &details, &m.CreatedAt, &m.UpdatedAt,
			&m.ApprovedAt, &m.RejectedAt, &m.ScheduledAt); err != nil {
			return nil, err
		}
		if contactID > 0 {
			m.ContactID = &contactID
		}
		if approvalID > 0 {
			m.ApprovalID = &approvalID
		}
		m.Details = json.RawMessage(details)
		out = append(out, m)
	}
	return out, rows.Err()
}

// MeetingHistory mengembalikan riwayat transisi status sebuah meeting (kronologis).
func (s *Store) MeetingHistory(ctx context.Context, meetingID int64) ([]model.MeetingStatusEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, meeting_id, COALESCE(from_status,''), to_status, COALESCE(changed_by,''),
		       COALESCE(reason,''), created_at
		FROM meeting_status_history WHERE meeting_id = $1 ORDER BY created_at, id
	`, meetingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.MeetingStatusEvent
	for rows.Next() {
		var ev model.MeetingStatusEvent
		if err := rows.Scan(&ev.ID, &ev.MeetingID, &ev.FromStatus, &ev.ToStatus,
			&ev.ChangedBy, &ev.Reason, &ev.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ── helper null ───────────────────────────────────────────────────────────
// (nullStr didefinisikan di admin.go)

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullRaw(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return r
}
