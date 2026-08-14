// Package model berisi tipe data yang dipakai lintas middleware & handler.
package model

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
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
		// HasMedia + Media terisi bila pesan membawa lampiran (dokumen/gambar/video).
		// WAHA GOWS mengirim URL file (host internal, mis. localhost:3000) di Media.URL;
		// unduh via waha.Client.DownloadMedia yang menulis-ulang host. Filename hanya
		// ada untuk DOKUMEN; gambar/video tak membawanya (diturunkan dari basename URL).
		HasMedia bool          `json:"hasMedia"`
		Media    *MediaPayload `json:"media"`
		// VCards berisi kartu kontak terlampir (mis. SU mengirim kontak orang yang
		// ingin dijadwalkan). Tiap elemen = satu kartu vCard mentah. WAHA NOWEB
		// menyatukan kontak tunggal & jamak ke array ini; `body` kosong saat ini.
		VCards []string `json:"vCards"`
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
		// ReplyTo terisi bila pesan ini adalah balasan (quote) atas pesan lain.
		// nil bila bukan balasan. `id`/`participant` engine-dependent (bisa absen);
		// `body` umumnya tersedia. Lihat dok WAHA: receive-messages (field replyTo).
		ReplyTo *struct {
			ID          string `json:"id"`
			Participant string `json:"participant"`
			Body        string `json:"body"`
			HasMedia    bool   `json:"hasMedia"`
		} `json:"replyTo"`
	} `json:"payload"`
}

// MediaPayload
type MediaPayload struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Mimetype string `json:"mimetype"`
	Error    string `json:"error"`
}

// HasFile true bila pesan membawa lampiran media yang bisa diunduh (URL ada, tanpa error).
func (e *WahaEvent) HasFile() bool {
	m := e.Payload.Media
	return m != nil && strings.TrimSpace(m.URL) != "" && strings.TrimSpace(m.Error) == ""
}

// MediaFilename mengembalikan nama berkas media masuk
func (e *WahaEvent) MediaFilename() string {
	m := e.Payload.Media
	if m == nil {
		return ""
	}
	if name := strings.TrimSpace(m.Filename); name != "" {
		return name
	}
	if u, err := url.Parse(m.URL); err == nil {
		return path.Base(u.Path)
	}
	return ""
}

// maxReplyPreview membatasi panjang kutipan pesan yang dibalas agar preamble
// agent tidak membengkak (pesan asal tidak melewati sanitizer panjang).
const maxReplyPreview = 500

// ReplyToText mengembalikan ringkasan teks pesan yang sedang dibalas (quote),
// terpotong bila panjang. Kosong bila pesan ini bukan balasan / tanpa teks.
func (e *WahaEvent) ReplyToText() string {
	rt := e.Payload.ReplyTo
	if rt == nil {
		return ""
	}
	body := strings.TrimSpace(rt.Body)
	if body == "" {
		if rt.HasMedia {
			return "[media tanpa teks]"
		}
		return ""
	}
	if len([]rune(body)) > maxReplyPreview {
		body = string([]rune(body)[:maxReplyPreview]) + "…"
	}
	return body
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

// VCardContact = kontak hasil parse satu kartu vCard (nama + nomor kanonik).
type VCardContact struct {
	Name  string // nama tampilan (FN), fallback dari N: atau kosong
	Phone string // MSISDN digit-only, mis. "628970258733"; kosong bila tak terbaca
}

// Contacts mem-parse payload.vCards (kartu kontak WhatsApp) menjadi daftar
// nama+nomor. Nomor diutamakan dari parameter waid= pada baris TEL (nomor kanonik
// WA), fallback ke digit nilai TEL. Mengembalikan nil bila tak ada kartu kontak.
func (e *WahaEvent) Contacts() []VCardContact {
	if len(e.Payload.VCards) == 0 {
		return nil
	}
	var out []VCardContact
	for _, raw := range e.Payload.VCards {
		c := parseVCard(raw)
		if c.Name == "" && c.Phone == "" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// ContactText merangkai kartu kontak terlampir menjadi teks yang bisa dibaca 
func (e *WahaEvent) ContactText() string {
	cs := e.Contacts()
	if len(cs) == 0 {
		return ""
	}
	var b strings.Builder
	if len(cs) == 1 {
		b.WriteString("[Kartu kontak dilampirkan]\n")
	} else {
		fmt.Fprintf(&b, "[%d kartu kontak dilampirkan]\n", len(cs))
	}
	for i, c := range cs {
		name := c.Name
		if name == "" {
			name = "(tanpa nama)"
		}
		phone := c.Phone
		if phone == "" {
			phone = "(nomor tak terbaca)"
		}
		fmt.Fprintf(&b, "%d. %s — %s\n", i+1, name, phone)
	}
	b.WriteString("Gunakan kontak ini bila diminta menghubungi atau menjadwalkan pertemuan dengannya.")
	return b.String()
}

// parseVCard mengekstrak nama (FN, fallback N) & nomor (TEL/waid) dari satu kartu
// vCard mentah. Toleran terhadap CRLF/LF dan parameter TEL yang beragam.
func parseVCard(raw string) VCardContact {
	var c VCardContact
	var nLine string
	lines := strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' })
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		up := strings.ToUpper(ln)
		switch {
		case strings.HasPrefix(up, "FN:"):
			if c.Name == "" {
				c.Name = unescapeVCard(ln[len("FN:"):])
			}
		case strings.HasPrefix(up, "N:"):
			nLine = ln[len("N:"):]
		case strings.HasPrefix(up, "TEL"):
			if c.Phone == "" {
				c.Phone = phoneFromTEL(ln)
			}
		}
	}
	if c.Name == "" && nLine != "" {
		// N:Family;Given;Middle;Prefix;Suffix → "Given Family"
		parts := strings.Split(nLine, ";")
		var family, given string
		if len(parts) > 0 {
			family = strings.TrimSpace(parts[0])
		}
		if len(parts) > 1 {
			given = strings.TrimSpace(parts[1])
		}
		c.Name = unescapeVCard(strings.TrimSpace(given + " " + family))
	}
	if r := []rune(c.Name); len(r) > 100 { 
		c.Name = string(r[:100])
	}
	return c
}

// phoneFromTEL mengambil MSISDN dari satu baris TEL vCard. Prioritas parameter
// waid= (nomor kanonik WA), fallback ke digit pada nilai setelah ':'.
func phoneFromTEL(line string) string {
	paramPart, value := line, ""
	if colon := strings.Index(line, ":"); colon >= 0 {
		paramPart, value = line[:colon], line[colon+1:]
	}
	if i := strings.Index(strings.ToLower(paramPart), "waid="); i >= 0 {
		if d := leadingDigits(paramPart[i+len("waid="):]); d != "" {
			return normalizeMSISDN(d)
		}
	}
	return normalizeMSISDN(onlyDigits(value))
}

// leadingDigits mengembalikan deret digit di AWAL string (berhenti di non-digit).
func leadingDigits(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return s[:i]
		}
	}
	return s
}

// onlyDigits membuang semua karakter selain digit.
func onlyDigits(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// normalizeMSISDN menormalkan nomor lokal Indonesia ('0…') ke awalan 62.
func normalizeMSISDN(d string) string {
	if strings.HasPrefix(d, "0") {
		return "62" + d[1:]
	}
	return d
}

// unescapeVCard membatalkan escape sederhana vCard (\n \, \; \\).
func unescapeVCard(s string) string {
	r := strings.NewReplacer(`\n`, " ", `\N`, " ", `\,`, ",", `\;`, ";", `\\`, `\`)
	return strings.TrimSpace(r.Replace(s))
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

// Message = satu pesan dalam sebuah percakapan.
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
// "RESCHEDULE_MEETING" (MeetingID+NewDatetime; bila meeting bagian dari grup → KONVERGENSI:
// seluruh peserta grup dinegosiasi ulang ke waktu itu), "CANCEL_MEETING" (MeetingID+Reason),
// "SPLIT_GROUP_MEETING" (MeetingID peserta grup yang divergen; opsional NewDatetime untuk
// jadwal terpisah atau ChangeKind="cancel" untuk mengeluarkannya — peserta lain tetap),
// "REQUEST_MEETING_CHANGE" (ChangeKind+Reason + opsional NewDatetime), "CONFIRM_VENUE"
// (VenueName+VenueAddress), "SET_REMINDER" (ReminderTime+ReminderNote + opsional
// RecurKind/RecurTime/RecurDow/ReminderLabel), "CANCEL_REMINDER" (ReminderID),
// "SCHEDULE_CALENDAR_DIGEST" (RecurKind/RecurTime/RecurDow atau ReminderTime + opsional
// ReminderNote sbg fokus + ReminderLabel; dibatalkan lewat CANCEL_REMINDER),
// "WATCH_EMAIL" (WatchCriteria + opsional WatchFrom/WatchKeyword/WatchLabel),
// "CANCEL_WATCH" (WatchID),
// "READ_EMAILS" (opsional EmailScope=unread|unreplied|all + EmailFrom/EmailKeyword),
// "SEND_DOCUMENT" (DocFilename + DocContent ATAU DocPath + opsional DocMime/DocEncoding/DocCaption),
// "DEFER_TASK" (Task + opsional DeferMinutes/ReminderLabel),
// "UPDATE_AGENT_PERSONA" (PersonaText + opsional TargetAgent).
type Action struct {
	Type     string `json:"type"`
	NewState string `json:"newState,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Target   string `json:"target,omitempty"`
	Task     string `json:"task,omitempty"`
	// Profil kontak untuk SPAWN_AGENT (opsional) — digunakan untuk registrasi kontak dan undangan email.
	TargetName    string `json:"targetName,omitempty"`
	TargetCompany string `json:"targetCompany,omitempty"`
	TargetEmail   string `json:"targetEmail,omitempty"`
	// Usulan detail meeting untuk SPAWN_AGENT (opsional).
	MeetingTopic    string `json:"meetingTopic,omitempty"`
	MeetingDatetime string `json:"meetingDatetime,omitempty"` // RFC3339, contoh: 2026-06-26T10:00:00+07:00
	MeetingVenue    string `json:"meetingVenue,omitempty"`    // kosong = online
	// ApprovalID untuk CONFIRM_MEETING: ID approval meeting yang menunggu persetujuan.
	ApprovalID int64 `json:"approvalId,omitempty"`
	// MeetingID: referensi meeting di snapshot; dipakai untuk RESCHEDULE_MEETING/CANCEL_MEETING.
	MeetingID int64 `json:"meetingId,omitempty"`
	// NewDatetime: jadwal mulai baru (RFC3339 +07:00) untuk RESCHEDULE_MEETING.
	NewDatetime string `json:"newDatetime,omitempty"`
	// Reason: alasan singkat untuk cancel/reschedule/request change.
	Reason string `json:"reason,omitempty"`
	// ChangeKind: jenis perubahan untuk REQUEST_MEETING_CHANGE.
	ChangeKind   string          `json:"changeKind,omitempty"`
	VenueName    string          `json:"venueName,omitempty"`
	VenueAddress string          `json:"venueAddress,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	ReminderTime  string `json:"reminderTime,omitempty"`
	ReminderNote  string `json:"reminderNote,omitempty"`
	RecurKind     string `json:"recurKind,omitempty"` // none|daily|weekly
	RecurTime     string `json:"recurTime,omitempty"` // "HH:MM" WIB (berulang)
	RecurDow      *int   `json:"recurDow,omitempty"`  // 0-6 (weekly)
	ReminderLabel string `json:"reminderLabel,omitempty"`
	// DEFER_TASK: tunda pekerjaan berat ke giliran latar belakang.
	// Task = uraian lengkap. DeferMinutes = jeda sebelum eksekusi (0 = segera).
	DeferMinutes int `json:"deferMinutes,omitempty"`
	// CANCEL_REMINDER (orchestrator/SU): ReminderID = id pengingat aktif (lihat snapshot)
	// yang ingin dibatalkan. Untuk pengingat berulang, membatalkan menghentikan seluruh seri.
	ReminderID int64 `json:"reminderId,omitempty"`
	// WATCH_EMAIL (orchestrator/SU): pantau inbox & lapor proaktif bila ada email yang cocok.
	//   WatchCriteria = kriteria bahasa alami (mis. "email follow up dari Yere"). WAJIB.
	//   WatchFrom     = pra-saring pengirim (substring email/nama; opsional, hemat token).
	//   WatchKeyword  = pra-saring kata kunci subjek/isi (opsional).
	//   WatchLabel    = rujukan singkat untuk pembatalan (opsional).
	// CANCEL_WATCH (orchestrator/SU): WatchID = id pantauan aktif (lihat snapshot) yang dihentikan.
	WatchCriteria string `json:"watchCriteria,omitempty"`
	WatchFrom     string `json:"watchFrom,omitempty"`
	WatchKeyword  string `json:"watchKeyword,omitempty"`
	WatchLabel    string `json:"watchLabel,omitempty"`
	WatchID       int64  `json:"watchId,omitempty"`
	// READ_EMAILS (orchestrator/SU): cek inbox SAAT ITU JUGA (on-demand)
	//   EmailScope   = "unread" (belum dibaca) | "unreplied" (belum dibalas) | "" / "all" (semua terbaru).
	//   EmailFrom    = filter pengirim opsional (substring email/nama).
	//   EmailKeyword = filter kata kunci subjek/isi opsional (dipisah koma = OR).
	EmailScope   string `json:"emailScope,omitempty"`
	EmailFrom    string `json:"emailFrom,omitempty"`
	EmailKeyword string `json:"emailKeyword,omitempty"`
	// SEND_DOCUMENT (orchestrator/SU): kirim dokumen/laporan.
	//   DocFilename = nama file.
	//   DocMime = tipe konten; kosong → deteksi dari ekstensi.
	//   DocEncoding = "utf8" (default) | "base64".
	//   DocContent = isi file.
	//   DocCaption = caption lampiran (opsional).
	//   DocPath = path file yang sudah ditulis agent di DOC_WORK_DIR.
	// Gunakan DocContent untuk teks (CSV/MD/HTML/JSON/TXT), dan DocPath untuk
	// biner (XLSX/PDF/PPTX/PNG). Jika keduanya ada, DocPath diprioritaskan.
	DocFilename string `json:"docFilename,omitempty"`
	DocMime     string `json:"docMime,omitempty"`
	DocEncoding string `json:"docEncoding,omitempty"`
	DocContent  string `json:"docContent,omitempty"`
	DocCaption  string `json:"docCaption,omitempty"`
	DocPath     string `json:"docPath,omitempty"`
	// UPDATE_AGENT_PERSONA (orchestrator/SU): Ubah preferensi gaya ringan agent.
	// PersonaText = overlay penuh (kosong = hapus preferensi kustom).
	// TargetAgent = agent tujuan (kosong -> "orchestrator").
	PersonaText string `json:"personaText,omitempty"`
	TargetAgent string `json:"targetAgent,omitempty"`

	Resource string `json:"resource,omitempty"`
	Limit    int    `json:"limit,omitempty"`

	Trust string `json:"trust,omitempty"`
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

// ScheduledTask = tugas pengingat terjadwal yang dijalankan saat FireAt tercapai.
type ScheduledTask struct {
	ID        int64      `json:"id"`
	FireAt    time.Time  `json:"fire_at"`
	Kind      string     `json:"kind"`
	Note      string     `json:"note"`
	MeetingID *int64     `json:"meeting_id,omitempty"`
	Status    string     `json:"status"`
	CreatedBy string     `json:"created_by"`
	ErrorText string     `json:"error_text,omitempty"`
	RecurKind string     `json:"recur_kind,omitempty"` // none|daily|weekly
	RecurTime string     `json:"recur_time,omitempty"` // "HH:MM" WIB
	RecurDow  *int       `json:"recur_dow,omitempty"`  // 0-6 (weekly)
	Label     string     `json:"label,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	FiredAt   *time.Time `json:"fired_at,omitempty"`
}

// EmailWatch = permintaan pemantauan email.
type EmailWatch struct {
	ID            int64      `json:"id"`
	Criteria      string     `json:"criteria"`
	Label         string     `json:"label,omitempty"`
	FromFilter    string     `json:"from_filter,omitempty"`
	KeywordFilter string     `json:"keyword_filter,omitempty"`
	CreatedBy     string     `json:"created_by"`
	Status        string     `json:"status"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`
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
