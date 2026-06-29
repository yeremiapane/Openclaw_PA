// Package model berisi tipe data yang dipakai lintas middleware & handler.
package model

import (
	"encoding/json"
	"strings"
	"time"
)

// WahaEvent merepresentasikan envelope webhook dari WAHA.
type WahaEvent struct {
	Event   string `json:"event"`
	Session string `json:"session"`
	Payload struct {
		ID        string `json:"id"`
		Timestamp int64  `json:"timestamp"`
		From      string `json:"from"`
		FromMe    bool   `json:"fromMe"`
		Body      string `json:"body"`
		// Data._data.key.remoteJidAlt menyimpan nomor asli (@s.whatsapp.net)
		// saat `from` @lid; dipakai memetakan @lid→phone agar kontak @lid
		// dikenali whitelist.
		Data struct {
			Key struct {
				RemoteJid      string `json:"remoteJid"`
				RemoteJidAlt   string `json:"remoteJidAlt"`
				Participant    string `json:"participant"`
				ParticipantAlt string `json:"participantAlt"`
				AddressingMode string `json:"addressingMode"`
			} `json:"key"`
		} `json:"_data"`
	} `json:"payload"`
}

// AltPhone mengembalikan nomor kanonik (hanya digit) dari remoteJidAlt/
// participantAlt bila `from` berupa @lid. Kosong bila tidak tersedia.
func (e *WahaEvent) AltPhone() string {
	for _, alt := range []string{e.Payload.Data.Key.RemoteJidAlt, e.Payload.Data.Key.ParticipantAlt} {
		if i := strings.LastIndex(alt, "@"); i > 0 {
			switch alt[i+1:] {
			case "s.whatsapp.net", "c.us":
				return alt[:i]
			}
		}
	}
	return ""
}

// Contact = satu baris whitelist dari tabel contacts.
// Profiling lengkap hanya dari query admin; hot-path auth hanya kolom inti.
type Contact struct {
	ID         int             `json:"id"`
	Phone      string          `json:"phone"`
	Lid        string          `json:"lid,omitempty"`
	Name       string          `json:"name,omitempty"`
	Company    string          `json:"company,omitempty"`
	Email      string          `json:"email,omitempty"`
	Address    string          `json:"address,omitempty"`
	TrustLevel string          `json:"trust_level"`
	Tags       []string        `json:"tags,omitempty"`
	Profile    json.RawMessage `json:"profile,omitempty"` // fakta fleksibel utk AI
	Notes      string          `json:"notes,omitempty"`
	CreatedAt  time.Time       `json:"created_at,omitempty"`
	UpdatedAt  time.Time       `json:"updated_at,omitempty"`
	DeletedAt  *time.Time      `json:"deleted_at,omitempty"`
}

// ExternalContact = nomor di luar whitelist yang pernah menghubungi bot.
type ExternalContact struct {
	ID           int             `json:"id"`
	Identifier   string          `json:"identifier"`
	Kind         string          `json:"kind"`
	Phone        string          `json:"phone,omitempty"`
	Lid          string          `json:"lid,omitempty"`
	DisplayName  string          `json:"display_name,omitempty"`
	Email        string          `json:"email,omitempty"`
	Company      string          `json:"company,omitempty"`
	Address      string          `json:"address,omitempty"`
	MessageCount int             `json:"message_count"`
	Status       string          `json:"status"`
	RiskScore    int             `json:"risk_score"`
	Tags         []string        `json:"tags,omitempty"`
	Profile      json.RawMessage `json:"profile,omitempty"`
	Notes        string          `json:"notes,omitempty"`
	FirstSeen    time.Time       `json:"first_seen"`
	LastSeen     time.Time       `json:"last_seen"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	DeletedAt    *time.Time      `json:"deleted_at,omitempty"`
}

// Message = satu pesan dalam sebuah percakapan (Fase 7).
// Role bernilai "user" (dari kontak) atau "assistant" (balasan agent).
type Message struct {
	Role      string    `json:"role"`
	Text      string    `json:"text"`
	AgentID   string    `json:"agent_id,omitempty"`
	CreatedAt time.Time `json:"ts"`
}

// Action = instruksi terstruktur dari agent
// Type yang dikenal: "UPDATE_STATE" (NewState), "NOTIFY_ORCHESTRATOR" (Payload),
// "SPAWN_AGENT" (Agent/Target/Task + opsional Target*), "CONFIRM_MEETING" (ApprovalID),
// "RESCHEDULE_MEETING" (MeetingID+NewDatetime), "CANCEL_MEETING" (MeetingID+Reason),
// "REQUEST_MEETING_CHANGE" (ChangeKind+Reason + opsional NewDatetime), "CONFIRM_VENUE"
// (VenueName+VenueAddress).
type Action struct {
	Type     string `json:"type"`
	NewState string `json:"newState,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Target   string `json:"target,omitempty"`
	Task     string `json:"task,omitempty"`
	// Profil kontak target untuk SPAWN_AGENT (opsional) — dipakai untuk mendaftarkan
	// kontak baru agar balasan dikenali dan untuk undangan email.
	TargetName    string `json:"targetName,omitempty"`
	TargetCompany string `json:"targetCompany,omitempty"`
	TargetEmail   string `json:"targetEmail,omitempty"`
	// Usulan detail meeting untuk SPAWN_AGENT (opsional).
	MeetingTopic    string `json:"meetingTopic,omitempty"`
	MeetingDatetime string `json:"meetingDatetime,omitempty"` // RFC3339, mis. 2026-06-26T10:00:00+07:00
	MeetingVenue    string `json:"meetingVenue,omitempty"`    // kosong → online (Teams)
	// ApprovalID untuk CONFIRM_MEETING: id approval meeting yang sudah ada & menunggu,
	// yang ingin disetujui via obrolan tanpa membuat approval/meeting baru.
	ApprovalID int64 `json:"approvalId,omitempty"`
	// MeetingID merujuk satu meeting di snapshot status (kolom meetingId=N). Dipakai
	// action RESCHEDULE_MEETING & CANCEL_MEETING (orchestrator/SU) untuk mengubah atau
	// membatalkan meeting yang SUDAH tercatat/terjadwal.
	MeetingID int64 `json:"meetingId,omitempty"`
	// NewDatetime = jadwal MULAI baru (RFC3339 +07:00) untuk RESCHEDULE_MEETING.
	NewDatetime string `json:"newDatetime,omitempty"`
	// Reason = alasan singkat untuk CANCEL_MEETING / RESCHEDULE_MEETING / REQUEST_MEETING_CHANGE.
	Reason string `json:"reason,omitempty"`
	// ChangeKind = jenis perubahan yang diminta untuk REQUEST_MEETING_CHANGE.
	ChangeKind string `json:"changeKind,omitempty"`
	VenueName    string          `json:"venueName,omitempty"`
	VenueAddress string          `json:"venueAddress,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
}

// Approval = satu pesan keluar yang ditahan menunggu persetujuan Pak Sudianto
// Status: pending | approved | rejected.
type Approval struct {
	ID             int64           `json:"id"`
	ConversationID string          `json:"conversation_id"`
	AgentID        string          `json:"agent_id"`
	ContactID      *int            `json:"contact_id,omitempty"`
	TargetChat     string          `json:"target_chat"`
	UserText       string          `json:"user_text"`
	ResponseText   string          `json:"response_text"`
	ApprovalReason string          `json:"approval_reason,omitempty"`
	NewFacts       json.RawMessage `json:"new_facts,omitempty"`
	Status         string          `json:"status"`
	CreatedAt      time.Time       `json:"created_at"`
	DecidedAt      *time.Time      `json:"decided_at,omitempty"`
}

// Execution = satu giliran agent (inject) yang dicatat untuk evaluasi & trace
// Menangkap token usage, model, durasi, actions, dan outcome.
type Execution struct {
	ID                int64           `json:"id"`
	RunID             string          `json:"run_id,omitempty"`
	OCSessionID       string          `json:"oc_session_id,omitempty"`
	SessionKey        string          `json:"session_key,omitempty"`
	ConversationID    string          `json:"conversation_id,omitempty"`
	ContactID         *int            `json:"contact_id,omitempty"`
	AgentID           string          `json:"agent_id"`
	Provider          string          `json:"provider,omitempty"`
	Model             string          `json:"model,omitempty"`
	InputText         string          `json:"input_text,omitempty"`
	ResponseText      string          `json:"response_text,omitempty"`
	RequiresApproval  bool            `json:"requires_approval"`
	Actions           json.RawMessage `json:"actions,omitempty"`
	NewFacts          json.RawMessage `json:"new_facts,omitempty"`
	FinishReason      string          `json:"finish_reason,omitempty"`
	StopReason        string          `json:"stop_reason,omitempty"`
	Refusal           bool            `json:"refusal"`
	InputTokens       int             `json:"input_tokens"`
	OutputTokens      int             `json:"output_tokens"`
	CacheReadTokens   int             `json:"cache_read_tokens"`
	CacheWriteTokens  int             `json:"cache_write_tokens"`
	TotalTokens       int             `json:"total_tokens"`
	SystemPromptChars int             `json:"system_prompt_chars,omitempty"`
	PromptChars       int             `json:"prompt_chars,omitempty"`
	DurationMs        int             `json:"duration_ms,omitempty"`
	FallbackUsed      bool            `json:"fallback_used"`
	Runner            string          `json:"runner,omitempty"`
	Outcome           string          `json:"outcome"`
	ErrorText         string          `json:"error_text,omitempty"`
	RawMeta           json.RawMessage `json:"raw_meta,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
}

// OutboundMessage = satu pesan yang benar-benar dikirim bot.
// Berbeda dari Message (memori konteks): ini jejak pengiriman aktual.
type OutboundMessage struct {
	ID             int64     `json:"id"`
	ExecutionID    *int64    `json:"execution_id,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	ContactID      *int      `json:"contact_id,omitempty"`
	AgentID        string    `json:"agent_id,omitempty"`
	TargetChat     string    `json:"target_chat"`
	Kind           string    `json:"kind"`
	Text           string    `json:"text"`
	Status         string    `json:"status"`
	ApprovalID     *int64    `json:"approval_id,omitempty"`
	ErrorText      string    `json:"error_text,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// MeetingRequest = request meeting + status lifecycle.
// Dibuat otomatis saat approval gate menahan pesan; status mengikuti keputusan SU.
type MeetingRequest struct {
	ID               int64           `json:"id"`
	ConversationID   string          `json:"conversation_id,omitempty"`
	ContactID        *int            `json:"contact_id,omitempty"`
	AgentID          string          `json:"agent_id"`
	ApprovalID       *int64          `json:"approval_id,omitempty"`
	RequestedVia     string          `json:"requested_via"`
	ExternalName     string          `json:"external_name,omitempty"`
	ExternalCompany  string          `json:"external_company,omitempty"`
	Topic            string          `json:"topic,omitempty"`
	MeetingType      string          `json:"meeting_type,omitempty"`
	ProposedDatetime *time.Time      `json:"proposed_datetime,omitempty"`
	Venue            string          `json:"venue,omitempty"`
	Status           string          `json:"status"`
	Details          json.RawMessage `json:"details,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	ApprovedAt       *time.Time      `json:"approved_at,omitempty"`
	RejectedAt       *time.Time      `json:"rejected_at,omitempty"`
	ScheduledAt      *time.Time      `json:"scheduled_at,omitempty"`
}

// MeetingStatusEvent = satu transisi status pada meeting_status_history.
type MeetingStatusEvent struct {
	ID         int64     `json:"id"`
	MeetingID  int64     `json:"meeting_id"`
	FromStatus string    `json:"from_status,omitempty"`
	ToStatus   string    `json:"to_status"`
	ChangedBy  string    `json:"changed_by,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// AccessLog = satu entri audit keputusan security layer.
type AccessLog struct {
	ID          int64     `json:"id"`
	Identifier  string    `json:"identifier"`
	Kind        string    `json:"kind"`
	Phone       string    `json:"phone,omitempty"`
	ContactID   *int      `json:"contact_id,omitempty"`
	Decision    string    `json:"decision"`
	Reason      string    `json:"reason,omitempty"`
	BodyPreview string    `json:"body_preview,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Identifier menggambarkan asal pesan: jenis (phone/lid/other) + nilai digit.
type Identifier struct {
	Kind  string // "phone" (@c.us), "lid" (@lid), atau "other" (grup/broadcast/dll)
	Value string // hanya digit, tanpa suffix
	Raw   string // nilai mentah dari payload.From
}

// ParseFrom memecah field `from` WAHA menjadi Identifier.
//
//	"6285277603027@c.us"  -> {phone, 6285277603027}
//	"5296382582977@lid"   -> {lid,   5296382582977}
//	"1203...@g.us"        -> {other, ...}  (grup → tidak akan lolos whitelist)
//	"status@broadcast"    -> {other, status}
func ParseFrom(from string) Identifier {
	at := strings.LastIndex(from, "@")
	if at < 0 {
		return Identifier{Kind: "other", Value: from, Raw: from}
	}
	value := from[:at]
	suffix := from[at+1:]
	switch suffix {
	case "c.us":
		return Identifier{Kind: "phone", Value: value, Raw: from}
	case "lid":
		return Identifier{Kind: "lid", Value: value, Raw: from}
	default:
		return Identifier{Kind: "other", Value: value, Raw: from}
	}
}
