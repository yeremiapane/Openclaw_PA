// Package routes berisi HTTP handler untuk API Gateway.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"pa-ai/api-gateway/src/db"
	"pa-ai/api-gateway/src/memory"
	"pa-ai/api-gateway/src/middleware"
	"pa-ai/api-gateway/src/model"
	"pa-ai/api-gateway/src/openclaw"
	"pa-ai/api-gateway/src/services"
	"pa-ai/api-gateway/src/waha"
)

// Handler menampung dependency untuk route (WAHA client, store, OpenClaw client, memory).
type Handler struct {
	Waha     *waha.Client
	Store    *db.Store
	OpenClaw *openclaw.Client
	Memory   *memory.Service
	Services *services.Client // Calendar + Email (penjadwalan saat approve)
	SUPhone  string           // nomor Pak Sudianto — tujuan notifikasi approval & orchestrator
	// NovaPhone = koordinator venue (Bu Nova). Dipakai sebagai target default saat
	// orchestrator meng-spawn agent 'support' untuk koordinasi venue tetapi lupa
	// mengisi `target` (LLM kerap mengosongkannya karena tak tahu nomor Bu Nova).
	NovaPhone string
	// ReminderLeadMinutes = berapa menit sebelum meeting mulai pengingat otomatis
	// dikirim ke SU (default 15 bila <= 0).
	ReminderLeadMinutes int
}

// agentForTrust memetakan trust_level kontak ke agent OpenClaw (Fase 8 routing).
//
//	su            -> orchestrator (jalur langsung Pak Sudianto)
//	semi_trusted  -> support      (koordinasi internal, mis. Bu Nova)
//	lainnya       -> pa_communicator (pihak eksternal)
func agentForTrust(trust string) string {
	switch trust {
	case "su":
		return "orchestrator"
	case "semi_trusted":
		return "support"
	default:
		return "pa_communicator"
	}
}

// approvalCmdRe mencocokkan perintah approval dari SU: "SETUJU 12" / "TOLAK #12".
var approvalCmdRe = regexp.MustCompile(`(?i)^(setuju|tolak|approve|reject)\s+#?(\d+)\b`)

func isApprove(verb string) bool {
	v := strings.ToLower(verb)
	return v == "setuju" || v == "approve"
}

// WahaInbound menangani POST /webhook/waha setelah auth, rate limit, dan sanitize.
// Event, kontak, dan teks bersih sudah ada di context.
//
// Pesan di-inject ke agent pa_communicator via CLI, balasan dikirim ke chat asal.
// Karena proses bisa lama, handler langsung balas 200 ke WAHA lalu lanjut di goroutine.
func (h *Handler) WahaInbound(c *gin.Context) {
	ev := c.MustGet(middleware.CtxEvent).(*model.WahaEvent)
	contact := c.MustGet(middleware.CtxContact).(*model.Contact)
	text := c.GetString(middleware.CtxText)

	// Audit: pesan lolos seluruh security layer.
	cid := contact.ID
	id := model.ParseFrom(ev.Payload.From)
	if err := h.Store.RecordAccess(c.Request.Context(), model.AccessLog{
		Identifier: ev.Payload.From, Kind: id.Kind, Phone: contact.Phone,
		ContactID: &cid, Decision: "allowed", BodyPreview: text,
	}); err != nil {
		log.Printf("[ERROR] gagal tulis access_log allowed: %v", err)
	}

	log.Printf("[INBOUND] session=%s from=%s name=%q trust=%s ts=%d body=%q",
		ev.Session, ev.Payload.From, contact.Name, contact.TrustLevel,
		ev.Payload.Timestamp, text)

	// Fase 8: routing multi-agent berdasarkan trust_level kontak.
	agentID := agentForTrust(contact.TrustLevel)

	// conversationId = session key OpenClaw = PK conversations (satu string konsisten).
	// Disertakan agentID agar sesi tiap agent terisolasi per kontak.
	keyID := contact.Phone
	if keyID == "" {
		keyID = id.Value
	}
	convID := "agent:" + agentID + ":" + keyID
	from := ev.Payload.From

	// Presence: tandai pesan masuk sudah dibaca (centang biru). Best-effort &
	// di luar jalur utama agar tak menambah latensi balasan.
	go func(chatID, msgID string) {
		if err := h.Waha.SendSeen(chatID, msgID); err != nil {
			log.Printf("[PRESENCE] sendSeen %s gagal: %v", chatID, err)
		}
	}(from, ev.Payload.ID)

	// Approval gate: Pak Sudianto dapat menyetujui/menolak pesan tertahan
	// langsung via WhatsApp ("SETUJU <id>" / "TOLAK <id>"). Ditangani sebelum
	// routing ke agent agar tidak diperlakukan sebagai percakapan biasa.
	if contact.TrustLevel == "su" {
		if m := approvalCmdRe.FindStringSubmatch(strings.TrimSpace(text)); m != nil {
			go h.handleApprovalCommand(from, m[1], m[2])
			c.JSON(http.StatusOK, gin.H{"status": "approval_command"})
			return
		}
	}

	// Konteks pesan yang sedang dibalas (quote). Hanya dipakai orchestrator (SU);
	// agent lain mengabaikannya. Diekstrak di sini selagi event masih tersedia.
	replyTo := ev.ReplyToText()

	// Proses inject + kirim balasan di luar request lifecycle WAHA.
	go h.process(contact, agentID, convID, from, text, replyTo)

	c.JSON(http.StatusOK, gin.H{"status": "received"})
}

// process menjalankan satu turn agent (rakit konteks → inject → terapkan actions
// → approval gate / kirim + simpan memori). Berjalan di goroutine sendiri —
// pakai context.Background (request WAHA sudah selesai).
func (h *Handler) process(contact *model.Contact, agentID, convID, from, text, replyTo string) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// Tampilkan "sedang mengetik…" saat agent memproses; auto-refresh karena
	// WhatsApp meng-expire composing, dan defer memastikan berhenti di semua jalur.
	typingCtx, stopTyping := context.WithCancel(ctx)
	go h.typingKeepAlive(typingCtx, from)
	defer stopTyping()

	injectMsg := text
	if h.Memory != nil {
		mc, err := h.Memory.Assemble(ctx, convID, contact)
		if err != nil {
			log.Printf("[MEMORY] assemble gagal conv=%s: %v (lanjut tanpa konteks)", convID, err)
		} else {
			// Anchor tanggal otoritatif untuk semua agent dan status meeting real-time.
			mc.LiveStatus = buildDateAnchor()
			if agentID == "orchestrator" {
				if snap := h.buildMeetingSnapshot(ctx); snap != "" {
					mc.LiveStatus += "\n\n" + snap
				}
				// Konteks balasan (quote) — hanya untuk orchestrator/SU. Bila Pak
				// Sudianto membalas pesan tertentu, sertakan kutipannya agar
				// orchestrator paham acuan balasan beliau.
				if replyTo != "" {
					mc.LiveStatus += "\n\n[PESAN YANG SEDANG DIBALAS PAK SUDIANTO]\n" +
						"Beliau menanggapi pesan ini: \"" + replyTo + "\"\n" +
						"Pakai kutipan ini sebagai acuan konteks balasan beliau."
				}
			} else if agentID == "pa_communicator" {
				// Penegasan: bila percakapan ini punya meeting offline yang masih menunggu
				// kesepakatan WAKTU, wajibkan agent menandai kesepakatan lewat sinyal
				// terstruktur saat pihak eksternal setuju (lihat buildVenueTimeReinforcement).
				if note := h.buildVenueTimeReinforcement(ctx, convID); note != "" {
					mc.LiveStatus += "\n\n" + note
				}
			}
			injectMsg = mc.BuildInjectMessage(text)
			log.Printf("[MEMORY] conv=%s agent=%s returning=%v history=%d facts=%d state=%s live=%v",
				convID, agentID, mc.IsReturning, len(mc.History), len(mc.Facts), mc.State, mc.LiveStatus != "")
		}
	}

	// Routing ke agent sesuai trust_level. injectWithRecovery menambah
	// ketahanan terhadap sesi OpenClaw yang terjebak prosa (retry + reset sesi).
	reply, meta, err := h.injectWithRecovery(ctx, agentID, convID, injectMsg)

	// Catat SETIAP giliran (sukses/gagal/diam) untuk evaluasi & trace.
	if errors.Is(err, openclaw.ErrNoReply) {
		log.Printf("[OPENCLAW] session=%s: agent memilih tidak membalas (NO_REPLY) — tidak ada kiriman", convID)
		h.logExecution(ctx, convID, agentID, contact, injectMsg, nil, meta, "no_reply", "")
		return
	}
	if err != nil {
		log.Printf("[OPENCLAW] inject gagal session=%s: %v", convID, err)
		h.logExecution(ctx, convID, agentID, contact, injectMsg, nil, meta, outcomeFromErr(err), err.Error())
		return
	}

	log.Printf("[OPENCLAW] session=%s agent=%s requiresApproval=%v actions=%d newFacts=%v response=%q",
		convID, agentID, reply.RequiresApproval, len(reply.Actions), reply.NewFacts, reply.Response)

	execID := h.logExecution(ctx, convID, agentID, contact, injectMsg, reply, meta, "ok", "")

	// Fase 8: terapkan actions terstruktur (UPDATE_STATE, NOTIFY_ORCHESTRATOR).
	// `text` diteruskan agar SPAWN_AGENT bisa koreksi nomor tujuan bila LLM salah ketik.
	h.applyActions(ctx, convID, contact, reply.Actions, execID, text)

	// Simpan profil yang BARU dipelajari (email/nama) ke tabel contacts agar diingat
	// lintas-percakapan — sehingga bot tak menanyakan ulang data yang sudah diberikan.
	h.persistContactProfile(ctx, contact, reply)

	// Approval gate: pesan yang mengikat Pak Sudianto ditahan sampai beliau
	// menyetujui. Memori turn ini ditunda hingga approve (lihat handleApprovalCommand).
	if reply.RequiresApproval {
		h.holdForApproval(ctx, convID, agentID, contact, from, text, reply, execID)
		return
	}

	// Tidak butuh approval: simpan memori (PostgreSQL + Redis) SEBELUM kirim,
	// agar tidak hilang bila pengiriman gagal.
	if h.Memory != nil {
		if err := h.Memory.Write(ctx, convID, contact, agentID, text, reply.Response, reply.NewFacts); err != nil {
			log.Printf("[MEMORY] write gagal conv=%s: %v", convID, err)
		}
	}
	stopTyping() // hentikan indikator "mengetik…" tepat sebelum balasan terkirim
	h.sendAndRecord(ctx, func() error { return h.Waha.SendToChat(from, reply.Response) },
		model.OutboundMessage{
			ExecutionID: execPtr(execID), ConversationID: convID, ContactID: cidPtr(contact),
			AgentID: agentID, TargetChat: from, Kind: "agent_reply", Text: reply.Response,
		})
}

// typingKeepAlive menampilkan indikator "sedang mengetik…", menyegarkannya
// tiap ~8 dtk sampai ctx dibatalkan, lalu menghentikannya.
func (h *Handler) typingKeepAlive(ctx context.Context, chatID string) {
	if err := h.Waha.StartTyping(chatID); err != nil {
		log.Printf("[PRESENCE] startTyping %s gagal: %v", chatID, err)
	}
	ticker := time.NewTicker(8 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := h.Waha.StopTyping(chatID); err != nil {
				log.Printf("[PRESENCE] stopTyping %s gagal: %v", chatID, err)
			}
			return
		case <-ticker.C:
			if err := h.Waha.StartTyping(chatID); err != nil {
				log.Printf("[PRESENCE] refresh typing %s gagal: %v", chatID, err)
			}
		}
	}
}

// wibZone = zona waktu tampilan untuk SU (WIB, UTC+7). FixedZone
var wibZone = time.FixedZone("WIB", 7*3600)

// Nama hari & bulan Bahasa Indonesia (Go tidak menyediakan lokalisasi bawaan).
var idDays = map[time.Weekday]string{
	time.Sunday: "Minggu", time.Monday: "Senin", time.Tuesday: "Selasa",
	time.Wednesday: "Rabu", time.Thursday: "Kamis", time.Friday: "Jumat",
	time.Saturday: "Sabtu",
}
var idMonths = map[time.Month]string{
	time.January: "Januari", time.February: "Februari", time.March: "Maret",
	time.April: "April", time.May: "Mei", time.June: "Juni", time.July: "Juli",
	time.August: "Agustus", time.September: "September", time.October: "Oktober",
	time.November: "November", time.December: "Desember",
}

// formatWIBLong merangkai waktu WIB dalam Bahasa Indonesia, mis.
// "Kamis, 25 Juni 2026 pukul 13.00 WIB".
func formatWIBLong(t time.Time) string {
	w := t.In(wibZone)
	return fmt.Sprintf("%s, %d %s %d pukul %02d.%02d WIB",
		idDays[w.Weekday()], w.Day(), idMonths[w.Month()], w.Year(), w.Hour(), w.Minute())
}

// formatWIBDate menyusun tanggal WIB tanpa jam (mis. "Senin, 30 Juni 2026").
func formatWIBDate(t time.Time) string {
	w := t.In(wibZone)
	return fmt.Sprintf("%s, %d %s %d", idDays[w.Weekday()], w.Day(), idMonths[w.Month()], w.Year())
}

// buildDateAnchor buat acuan tanggal otoritatif (hari ini + 14 hari) untuk
// agar agent menerjemahkan nama-hari ke tanggal dengan benar.
func buildDateAnchor() string {
	now := time.Now().In(wibZone)
	var b strings.Builder
	b.WriteString("[ACUAN TANGGAL — WAJIB & OTORITATIF. Pakai daftar ini untuk menerjemahkan " +
		"nama hari (mis. \"Rabu\", \"Senin depan\") menjadi TANGGAL yang benar. JANGAN menghitung " +
		"atau menebak tanggal sendiri.]\n")
	b.WriteString("Hari ini: " + formatWIBDate(now) + " (WIB).\n")
	b.WriteString("Tanggal 14 hari ke depan:\n")
	for i := 1; i <= 14; i++ {
		b.WriteString("- " + formatWIBDate(now.AddDate(0, 0, i)) + "\n")
	}
	b.WriteString("Bila seseorang menyebut nama hari tanpa tanggal eksplisit, pilih kemunculan " +
		"TERDEKAT dari daftar di atas. Saat mengisi datetime (newDatetime / meeting.datetime), " +
		"gunakan tanggal PERSIS dari daftar ini dengan offset +07:00.")
	return b.String()
}

// formatMeetingReportSU buat ringkasan meeting (ID) untuk laporan persetujuan
// ke SU — TANPA draf pesan ke pihak eksternal. Mengembalikan badan laporan.
func formatMeetingReportSU(who string, reply *openclaw.AgentReply) string {
	var b strings.Builder
	b.WriteString(who + " telah menyetujui jadwal berikut:\n")
	m := reply.Meeting
	topic := strings.TrimSpace(reply.ApprovalReason)
	if m != nil && strings.TrimSpace(m.Title) != "" {
		topic = m.Title
	}
	if m != nil && m.Datetime != "" {
		if t, err := time.Parse(time.RFC3339, m.Datetime); err == nil {
			b.WriteString("📅 " + formatWIBLong(t) + "\n")
		}
	}
	if topic != "" {
		b.WriteString("📋 " + topic + "\n")
	}
	if m != nil {
		if strings.TrimSpace(m.Venue) == "" {
			b.WriteString("💻 Online (Microsoft Teams)\n")
		} else {
			b.WriteString("📍 " + m.Venue + "\n")
		}
	}
	return b.String()
}

// meetingStatusLabel menerjemahkan status mentah meeting_requests ke keterangan
// jelas (Bahasa Indonesia) untuk orchestrator. Krusial: 'scheduled' berarti
// meeting SUDAH terkonfirmasi & masuk kalender — bukan lagi "menunggu".
func meetingStatusLabel(status string) string {
	switch status {
	case "scheduled":
		return "SUDAH TERJADWAL & TERKONFIRMASI (event kalender Pak Sudianto sudah dibuat) — ini jadwal RESMI, jangan sebut menunggu"
	case "approved":
		return "disetujui Pak Sudianto, sedang diproses penjadwalan"
	case "pending":
		return "masih menunggu (belum disetujui SU dan/atau belum dikonfirmasi pihak eksternal)"
	default:
		return status
	}
}

// buildMeetingSnapshot merakit ringkasan status meeting LANGSUNG dari PostgreSQL
// untuk disisipkan ke konteks orchestrator.
func (h *Handler) buildMeetingSnapshot(ctx context.Context) string {
	if h.Store == nil {
		return ""
	}
	all, err := h.Store.ListMeetings(ctx, "", 50)
	if err != nil {
		log.Printf("[SNAPSHOT] ambil meeting gagal: %v", err)
		return ""
	}
	var rows []string
	for _, m := range all {
		switch m.Status {
		case "pending", "approved", "scheduled":
		default:
			continue
		}
		who := strings.TrimSpace(m.ExternalName)
		if who == "" {
			who = "(pihak eksternal)"
		}
		if m.ExternalCompany != "" {
			who += " (" + m.ExternalCompany + ")"
		}
		when := "waktu belum ditentukan"
		if t := m.ProposedDatetime; t != nil {
			when = t.In(wibZone).Format("Mon 02 Jan 2006 15:04") + " WIB"
		}
		platform := strings.TrimSpace(m.MeetingType)
		if m.Venue != "" {
			if platform != "" {
				platform += " — " + m.Venue
			} else {
				platform = m.Venue
			}
		}
		line := fmt.Sprintf("- %s | %s", who, when)
		if platform != "" {
			line += " | " + platform
		}
		if m.Topic != "" {
			line += " | topik: " + m.Topic
		}
		line += " | STATUS: " + meetingStatusLabel(m.Status)
		// meetingId WAJIB disertakan: dipakai orchestrator untuk RESCHEDULE_MEETING /
		// CANCEL_MEETING atas meeting yang SUDAH tercatat/terjadwal.
		line += fmt.Sprintf(" | meetingId=%d", m.ID)
		// Jika meeting masih menunggu konfirmasi SU dan punya approval tertaut,
		// sertakan approvalId agar orchestrator mengonfirmasi yang sudah ada
		// (CONFIRM_MEETING), bukan membuat meeting baru.
		if m.Status == "pending" && m.ApprovalID != nil {
			line += fmt.Sprintf(" | approvalId=%d (untuk menyetujui meeting INI, pakai CONFIRM_MEETING approvalId %d — JANGAN buat meeting baru)", *m.ApprovalID, *m.ApprovalID)
		}
		rows = append(rows, line)
	}
	if len(rows) == 0 {
		return ""
	}
	return "[STATUS MEETING TERKINI — data LANGSUNG & OTORITATIF dari sistem. " +
		"WAJIB pakai ini untuk menjawab pertanyaan jadwal/status meeting; JANGAN " +
		"mengandalkan ingatan percakapan lama yang mungkin sudah usang. Untuk MENJADWAL " +
		"ULANG meeting pakai RESCHEDULE_MEETING (meetingId + newDatetime); untuk " +
		"MEMBATALKAN pakai CANCEL_MEETING (meetingId). JANGAN buat meeting baru untuk " +
		"mengubah/membatalkan yang sudah ada.]\n" +
		strings.Join(rows, "\n")
}

// jsonNudge ditambahkan ke pesan saat retry parse_error: mengingatkan agent agar
// mematuhi kontrak output JSON (tanpa prosa/markdown).
const jsonNudge = "\n\n[SISTEM — PENTING] Balas HANYA satu objek JSON valid sesuai kontrak SOUL " +
	"(mulai dengan '{' dan akhiri dengan '}'), tanpa teks, sapaan, atau markdown apa pun di luar JSON."

// isParseErr true bila kegagalan inject berasal dari pelanggaran kontrak JSON
// (agent membalas prosa / JSON rusak) — kandidat untuk recovery.
func isParseErr(err error) bool {
	return err != nil && outcomeFromErr(err) == "parse_error"
}

// injectWithRecovery menjalankan satu turn agent dengan ketahanan terhadap sesi
// OpenClaw yang "terjebak prosa" (kontrak JSON gagal → turn hilang, SU tak dibalas).
// Strategi berlapis:
//  1. coba normal (session-key efektif = convID + epoch dari memori).
//  2. bila parse_error: retry sekali pada sesi yang SAMA dengan dorongan "JSON saja".
//  3. bila masih parse_error: RESET sesi OpenClaw (bump epoch → session-key baru,
//     lepas dari riwayat terkontaminasi) lalu ulangi. Konteks tidak hilang karena
//     pesan sudah memuat preamble dari memori gateway (di-key oleh convID).
func (h *Handler) injectWithRecovery(ctx context.Context, agentID, convID, message string) (*openclaw.AgentReply, *openclaw.RunMeta, error) {
	sk := convID
	if h.Memory != nil {
		sk = h.Memory.OCSessionKey(ctx, convID)
	}
	reply, meta, err := h.OpenClaw.InjectAgent(ctx, agentID, sk, message)
	if !isParseErr(err) {
		return reply, meta, err
	}

	// (2) retry pada sesi sama dengan dorongan kepatuhan JSON.
	log.Printf("[RECOVERY] parse_error conv=%s sk=%s — retry dorongan JSON (sesi sama)", convID, sk)
	reply, meta, err = h.OpenClaw.InjectAgent(ctx, agentID, sk, message+jsonNudge)
	if !isParseErr(err) {
		return reply, meta, err
	}

	// (3) reset sesi OpenClaw — sesi lama dianggap terkontaminasi prosa.
	if h.Memory != nil {
		if nsk, rerr := h.Memory.ResetOCSession(ctx, convID); rerr != nil {
			log.Printf("[RECOVERY] gagal reset sesi conv=%s: %v", convID, rerr)
		} else {
			log.Printf("[RECOVERY] RESET sesi OpenClaw conv=%s: %s → %s (sesi lama dibuang)", convID, sk, nsk)
			sk = nsk
		}
	}
	reply, meta, err = h.OpenClaw.InjectAgent(ctx, agentID, sk, message+jsonNudge)
	if isParseErr(err) {
		log.Printf("[RECOVERY] GAGAL conv=%s — masih parse_error setelah retry+reset", convID)
	} else if err == nil {
		log.Printf("[RECOVERY] PULIH conv=%s pada sesi baru", convID)
	}
	return reply, meta, err
}

// ── Fase 8.5: helper observability (token usage, outbound, meeting) ──────────

// cidPtr mengembalikan pointer ID kontak (nil bila tidak ada).
func cidPtr(contact *model.Contact) *int {
	if contact != nil && contact.ID > 0 {
		id := contact.ID
		return &id
	}
	return nil
}

// execPtr membungkus execID jadi pointer (nil bila 0).
func execPtr(id int64) *int64 {
	if id > 0 {
		return &id
	}
	return nil
}

// outcomeFromErr memetakan error inject ke label outcome agent_executions.
func outcomeFromErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "output kosong"):
		return "empty"
	case strings.Contains(s, "parse balasan agent"),
		strings.Contains(s, "balasan agent kosong"),
		strings.Contains(s, "tidak ada payload"),
		strings.Contains(s, "parse envelope"):
		return "parse_error"
	default:
		return "error"
	}
}

// logExecution mencatat satu giliran agent ke agent_executions. Mengembalikan
// execID (0 bila gagal). reply boleh nil (kasus gagal/diam); meta tidak pernah nil.
func (h *Handler) logExecution(ctx context.Context, convID, agentID string, contact *model.Contact, inputText string, reply *openclaw.AgentReply, meta *openclaw.RunMeta, outcome, errText string) int64 {
	if meta == nil {
		meta = &openclaw.RunMeta{}
	}
	e := model.Execution{
		RunID: meta.RunID, OCSessionID: meta.OCSessionID, SessionKey: convID,
		ConversationID: convID, ContactID: cidPtr(contact), AgentID: agentID,
		Provider: meta.Provider, Model: meta.Model, InputText: inputText,
		FinishReason: meta.FinishReason, StopReason: meta.StopReason, Refusal: meta.Refusal,
		InputTokens: meta.Usage.Input, OutputTokens: meta.Usage.Output,
		CacheReadTokens: meta.Usage.CacheRead, CacheWriteTokens: meta.Usage.CacheWrite,
		TotalTokens: meta.Usage.Total(), SystemPromptChars: meta.SystemPromptChars,
		PromptChars: meta.PromptChars, DurationMs: meta.DurationMs,
		FallbackUsed: meta.FallbackUsed, Runner: meta.Runner, Outcome: outcome,
		ErrorText: errText, RawMeta: meta.ExecutionTrace,
	}
	if reply != nil {
		e.ResponseText = reply.Response
		e.RequiresApproval = reply.RequiresApproval
		e.Actions, _ = json.Marshal(reply.Actions)
		e.NewFacts, _ = json.Marshal(reply.NewFacts)
	}
	id, err := h.Store.CreateExecution(ctx, e)
	if err != nil {
		log.Printf("[OBS] log execution gagal conv=%s: %v", convID, err)
		return 0
	}
	log.Printf("[OBS] exec #%d conv=%s model=%s tokens(in=%d out=%d cr=%d cw=%d tot=%d) dur=%dms outcome=%s",
		id, convID, meta.Model, meta.Usage.Input, meta.Usage.Output, meta.Usage.CacheRead,
		meta.Usage.CacheWrite, meta.Usage.Total(), meta.DurationMs, outcome)
	return id
}

// sendAndRecord menjalankan pengiriman (bila sendFn != nil) lalu mencatat pesan
// keluar ke outbound_messages. Status diisi otomatis (sent/failed) bila kosong.
func (h *Handler) sendAndRecord(ctx context.Context, sendFn func() error, o model.OutboundMessage) {
	if sendFn != nil {
		if err := sendFn(); err != nil {
			o.Status = "failed"
			o.ErrorText = err.Error()
			log.Printf("[ERROR] kirim %s ke %s gagal: %v", o.Kind, o.TargetChat, err)
		} else if o.Status == "" {
			o.Status = "sent"
		}
	}
	if o.Status == "" {
		o.Status = "sent"
	}
	if _, err := h.Store.LogOutbound(ctx, o); err != nil {
		log.Printf("[OBS] log outbound gagal: %v", err)
	}
}

// meetingDetails = isi kolom JSON details meeting_requests. Menyimpan data yang
// tidak punya kolom sendiri + hasil penjadwalan (eventId/link) saat scheduled.
type meetingDetails struct {
	ApprovalReason  string   `json:"approvalReason,omitempty"`
	NewFacts        []string `json:"newFacts,omitempty"`
	Title           string   `json:"title,omitempty"`
	DurationMinutes int      `json:"durationMinutes,omitempty"`
	AttendeeEmail   string   `json:"attendeeEmail,omitempty"`
	AttendeeName    string   `json:"attendeeName,omitempty"`
	EventID         string   `json:"eventId,omitempty"`
	CalendarLink    string   `json:"calendarLink,omitempty"`
	TeamsLink       string   `json:"teamsLink,omitempty"`
	// ReschedulePending menandai meeting ini SEDANG dijadwal ulang: PA Communicator
	// sudah ditugaskan menegosiasikan waktu baru dengan pihak eksternal. Saat
	// kesepakatan masuk approval gate, createMeetingFromApproval memakai penanda ini
	// untuk MEMPERBARUI meeting yang sama (bukan membuat baris baru).
	ReschedulePending bool `json:"reschedulePending,omitempty"`
	// RescheduleFrom menyimpan jadwal LAMA (RFC3339) yang dikonfirmasi sebelum
	// reschedule — dipakai email "jadwal lama → baru" & sebagai sinyal finalisasi
	// reschedule (PATCH event) alih-alih membuat event baru.
	RescheduleFrom string `json:"rescheduleFrom,omitempty"`
	// Koordinasi venue (meeting offline yang lokasinya dicarikan support↔Bu Nova).
	// VenueCoordination menandai meeting ini WAJIB punya lokasi pasti sebelum boleh
	// difinalisasi — paket lengkap (waktu+lokasi) baru diajukan ke SU saat keduanya
	// siap. TimeAgreed=true setelah pihak eksternal menyepakati WAKTU (lewat PA
	// Communicator). VenueConfirmed=true setelah Bu Nova memastikan lokasi
	// (VenueName + VenueAddress). KEBIJAKAN: tidak menyimpan/menampilkan biaya.
	VenueCoordination bool   `json:"venueCoordination,omitempty"`
	TimeAgreed        bool   `json:"timeAgreed,omitempty"`
	VenueConfirmed    bool   `json:"venueConfirmed,omitempty"`
	VenueName         string `json:"venueName,omitempty"`
	VenueAddress      string `json:"venueAddress,omitempty"`
}

// createMeetingFromApproval membuat meeting_request (pending) tertaut ke approval
// yang baru ditahan. Mengisi field terstruktur dari reply.Meeting (Fase 9) bila ada.
func (h *Handler) createMeetingFromApproval(ctx context.Context, convID, agentID string, contact *model.Contact, approvalID int64, reply *openclaw.AgentReply) {
	via := "external"
	name, company, email := "", "", ""
	if contact != nil {
		if contact.TrustLevel == "su" {
			via = "su"
		}
		name, company, email = contact.Name, contact.Company, contact.Email
	}

	det := meetingDetails{ApprovalReason: reply.ApprovalReason, NewFacts: reply.NewFacts}
	topic := reply.ApprovalReason
	venue, meetingType := "", ""
	var proposed *time.Time

	if m := reply.Meeting; m != nil {
		det.Title = m.Title
		det.DurationMinutes = m.DurationMinutes
		det.AttendeeName = firstNonEmptyStr(m.AttendeeName, name)
		det.AttendeeEmail = firstNonEmptyStr(m.AttendeeEmail, email)
		if m.Title != "" {
			topic = m.Title
		}
		venue = m.Venue
		if venue == "" {
			meetingType = "online"
		} else {
			meetingType = "onsite"
		}
		if m.Datetime != "" {
			if t, err := time.Parse(time.RFC3339, m.Datetime); err == nil {
				proposed = &t
			} else {
				log.Printf("[MEETING] datetime agent tak valid (%q): %v — disimpan tanpa jadwal", m.Datetime, err)
			}
		}
	}

	// Cabang RESCHEDULE: bila percakapan ini sedang menjadwal ulang meeting yang sudah
	// ada (ditandai reschedulePending saat RESCHEDULE_MEETING di-dispatch), JANGAN buat
	// baris baru. Perbarui meeting tersebut: tautkan approval baru, warisi eventId
	// (agar finalisasi MEM-PATCH event yang sama, bukan membuat baru) & simpan waktu lama.
	if existing, eerr := h.Store.ActiveMeetingByConversation(ctx, convID); eerr == nil && existing != nil {
		var ed meetingDetails
		_ = json.Unmarshal(existing.Details, &ed)
		if ed.ReschedulePending {
			det.EventID = ed.EventID
			det.CalendarLink = ed.CalendarLink
			det.TeamsLink = ed.TeamsLink
			det.RescheduleFrom = ed.RescheduleFrom
			det.Title = firstNonEmptyStr(det.Title, ed.Title, existing.Topic)
			det.AttendeeEmail = firstNonEmptyStr(det.AttendeeEmail, ed.AttendeeEmail)
			det.AttendeeName = firstNonEmptyStr(det.AttendeeName, ed.AttendeeName)
			if det.DurationMinutes == 0 {
				det.DurationMinutes = ed.DurationMinutes
			}
			det.ReschedulePending = false
			merged, _ := json.Marshal(det)
			if rerr := h.Store.RelinkMeetingForReschedule(ctx, existing.ID, approvalID, proposed, venue, merged, agentID,
				"reschedule diajukan — menunggu persetujuan SU"); rerr != nil {
				log.Printf("[MEETING] relink reschedule #%d (approval #%d) gagal: %v", existing.ID, approvalID, rerr)
			} else {
				log.Printf("[MEETING] #%d di-reschedule via approval #%d (waktu baru=%v) — tidak buat baris baru", existing.ID, approvalID, proposed)
			}
			return
		}
	}

	details, _ := json.Marshal(det)

	apID := approvalID
	mid, err := h.Store.CreateMeetingRequest(ctx, model.MeetingRequest{
		ConversationID: convID, ContactID: cidPtr(contact), AgentID: agentID,
		ApprovalID: &apID, RequestedVia: via, ExternalName: name, ExternalCompany: company,
		Topic: topic, MeetingType: meetingType, ProposedDatetime: proposed, Venue: venue,
		Status: "pending", Details: details,
	}, agentID)
	if err != nil {
		log.Printf("[MEETING] buat gagal approval #%d: %v", approvalID, err)
		return
	}
	log.Printf("[MEETING] #%d dibuat (pending) dari approval #%d conv=%s datetime=%v", mid, approvalID, convID, proposed)

	// Rekonsiliasi dgn proposal SPAWN: meeting final ditahan di percakapan orchestrator
	// (kontak=SU), sedangkan proposal inisiasi SU dibuat di percakapan pa_communicator
	// (kontak=eksternal) — keduanya merepresentasikan satu meeting. Pensiunkan proposal
	// yang cocok (nama peserta eksternal) agar tidak terhitung ganda di snapshot.
	if det.AttendeeName != "" {
		if prop, ferr := h.Store.FindPendingSpawnMeeting(ctx, det.AttendeeName, ""); ferr == nil && prop != nil && prop.ID != mid {
			if uerr := h.Store.UpdateMeetingStatus(ctx, prop.ID, "superseded", "su",
				fmt.Sprintf("digantikan oleh meeting final #%d (masuk approval gate)", mid)); uerr != nil {
				log.Printf("[MEETING] pensiun proposal SPAWN #%d gagal: %v", prop.ID, uerr)
			} else {
				log.Printf("[MEETING] proposal SPAWN #%d → superseded (digantikan #%d)", prop.ID, mid)
			}
		}
	}
}

// persistContactProfile menyimpan profil yang BARU dipelajari agent (email/nama)
// ke tabel contacts agar diingat lintas-percakapan. Sumber deterministik = objek
// meeting yang diisi agent (AttendeeEmail/AttendeeName). UpdateContact memakai
// COALESCE → hanya mengisi field yang masih kosong, tidak menimpa data yang ada.
func (h *Handler) persistContactProfile(ctx context.Context, contact *model.Contact, reply *openclaw.AgentReply) {
	if h.Store == nil || contact == nil || contact.Phone == "" || reply.Meeting == nil {
		return
	}
	var in db.ContactInput
	upd := false
	if contact.Email == "" {
		if e := strings.TrimSpace(reply.Meeting.AttendeeEmail); e != "" {
			in.Email = e
			upd = true
		}
	}
	if contact.Name == "" {
		if n := strings.TrimSpace(reply.Meeting.AttendeeName); n != "" {
			in.Name = n
			upd = true
		}
	}
	if !upd {
		return
	}
	if _, err := h.Store.UpdateContact(ctx, contact.Phone, in); err != nil {
		log.Printf("[PROFILE] update kontak %s gagal: %v", contact.Phone, err)
		return
	}
	log.Printf("[PROFILE] kontak %s diperbarui dari percakapan (email/nama)", contact.Phone)
}

// firstNonEmptyStr mengembalikan string non-kosong pertama.
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// applyActions menjalankan instruksi terstruktur dari agent (Fase 8).
// srcText = pesan ASLI yang memicu turn ini (mis. teks SU). Dipakai SPAWN_AGENT
// untuk mengoreksi nomor tujuan secara deterministik dari sumber manusia, sebab
// LLM kerap menjatuhkan digit saat mengetik ulang nomor ke field action.target.
func (h *Handler) applyActions(ctx context.Context, convID string, contact *model.Contact, actions []model.Action, execID int64, srcText string) {
	for _, a := range actions {
		switch strings.ToUpper(strings.TrimSpace(a.Type)) {
		case "UPDATE_STATE":
			if a.NewState == "" || h.Memory == nil {
				continue
			}
			if err := h.Memory.SetState(ctx, convID, a.NewState); err != nil {
				log.Printf("[ACTION] update_state gagal conv=%s -> %s: %v", convID, a.NewState, err)
			} else {
				log.Printf("[ACTION] state conv=%s -> %s", convID, a.NewState)
			}
		case "NOTIFY_ORCHESTRATOR":
			h.notifyOrchestrator(ctx, convID, contact, a.Payload, execID)
		case "CONFIRM_MEETING":
			// SU (lewat orchestrator) menyetujui meeting yang SUDAH ADA & menunggu.
			// Selesaikan approval yang ada — TIDAK membuat approval/meeting baru.
			// Sinkron (ctx masih hidup selama applyActions) agar selesai sebelum
			// balasan orchestrator dikirim ke SU.
			h.confirmExistingMeeting(ctx, contact, a)
		case "CONFIRM_VENUE":
			// Support: Bu Nova sudah memastikan SATU venue. Simpan lokasi pasti ke
			// meeting offline yang sedang menunggu venue; bila waktu juga sudah
			// disepakati pihak eksternal, ajukan paket lengkap (waktu+lokasi) ke SU.
			// Tanpa biaya/estimasi harga. Sinkron (ctx masih hidup di applyActions).
			h.confirmVenue(ctx, contact, a)
		case "RESCHEDULE_MEETING":
			// SU (lewat orchestrator) ingin menjadwal ulang: TUGASKAN PA Communicator
			// menegosiasikan waktu baru dengan pihak eksternal lebih dulu (bukan eksekusi
			// sepihak). Finalisasi (PATCH event + email) terjadi setelah SU menyetujui
			// jadwal yang disepakati. Goroutine sendiri karena ada inject LLM.
			act := a
			go h.rescheduleDispatch(contact, act)
		case "CANCEL_MEETING":
			// SU (lewat orchestrator) membatalkan: TUGASKAN PA Communicator mengabari
			// pihak eksternal secara natural, lalu sistem finalisasi (hapus event O365 +
			// email pembatalan + status cancelled). SU sudah memutuskan → tanpa approval ulang.
			act := a
			go h.cancelDispatch(contact, act)
		case "REQUEST_MEETING_CHANGE":
			// pa_communicator/support: pihak EKSTERNAL minta ubah/batal jadwal. Tidak
			// mengeksekusi apa pun — hanya meneruskan permintaan ke SU untuk diputuskan.
			h.requestMeetingChange(ctx, convID, contact, a)
		case "SPAWN_AGENT":
			// Inisiasi outbound: SU (via orchestrator) minta agent menghubungi pihak
			// eksternal. Dijalankan di goroutine sendiri (inject bisa lama) dan
			// SELALU melewati approval gate sebelum pesan benar-benar terkirim.
			act := a
			go h.spawnOutbound(contact, act, srcText)
		case "SET_REMINDER":
			// SU (lewat orchestrator) minta pengingat pada waktu tertentu. Disimpan ke
			// scheduled_tasks; worker latar belakang menyuruh orchestrator menyampaikannya
			// ke SU saat jatuh tempo. Hanya boleh dari percakapan SU (gerbang di setReminder).
			h.setReminder(ctx, contact, a)
		case "SEND_DOCUMENT":
			// SU (lewat orchestrator) minta sebuah laporan/dokumen. Agent menyusun
			// SENDIRI isi & format-nya; gateway hanya mengemas jadi file & mengirim ke
			// SU. Hanya boleh dari percakapan SU (gerbang di sendDocument).
			h.sendDocument(ctx, convID, contact, a, execID)
		default:
			if a.Type != "" {
				log.Printf("[ACTION] tipe tidak dikenal: %q (diabaikan)", a.Type)
			}
		}
	}
}

// spawnOutbound menjalankan action SPAWN_AGENT: SU (lewat orchestrator) menginisiasi
// percakapan WhatsApp KELUAR ke pihak eksternal. Alurnya:
//  1. Hanya inisiator ber-trust 'su' yang diizinkan (keamanan — cegah agent lain
//     atau kontak eksternal memicu pengiriman keluar).
//  2. Hanya boleh mendelegasikan ke agent penghubung (pa_communicator/support),
//     tidak ke orchestrator sendiri.
//  3. Daftarkan kontak target ke whitelist (trust 'external') agar balasannya
//     nanti lolos security layer dan dirutekan kembali ke agent yang sama.
//  4. Inject tugas ke agent untuk menyusun PESAN PEMBUKA.
//  5. SELALU tahan pesan pembuka itu di approval gate — SU wajib menyetujui
//     sebelum pesan benar-benar terkirim ke pihak eksternal (keputusan keamanan).
//
// Berjalan di goroutine sendiri (inject bisa puluhan detik) dengan context baru,
// karena request/turn pemicunya sudah selesai.
func (h *Handler) spawnOutbound(initiator *model.Contact, act model.Action, srcText string) {
	// (1) Keamanan: hanya SU/orchestrator yang boleh memulai kontak keluar.
	if initiator == nil || initiator.TrustLevel != "su" {
		trust := "(nil)"
		if initiator != nil {
			trust = initiator.TrustLevel
		}
		log.Printf("[SPAWN] DITOLAK: inisiator non-SU (trust=%s) target=%s", trust, act.Target)
		return
	}

	// (2) Agent tujuan: default pa_communicator; orchestrator tidak boleh di-spawn.
	agentID := strings.TrimSpace(act.Agent)
	if agentID == "" {
		agentID = "pa_communicator"
	}
	if agentID != "pa_communicator" && agentID != "support" {
		log.Printf("[SPAWN] agent target tak diizinkan: %q (dibatalkan)", agentID)
		return
	}

	// Spawn ke 'support' = koordinasi venue, dan koordinator venue SELALU Bu Nova
	// (otoritatif dari env NOVA_PHONE). Orchestrator (LLM) kerap mengosongkan atau
	// keliru mengisi `target` karena tak punya nomor Bu Nova di konteksnya. Maka untuk
	// support kita PAKSA target = Nova dan (di bawah) LEWATI semua koreksi/peminjaman
	// nomor manusia. Tanpa ini, reconcileMSISDN bisa "meminjam" satu-satunya nomor
	// manusia di pesan SU (mis. orang yang justru ingin dihubungi SU) sehingga pesan
	// venue "Halo Bu Nova ..." malah TERKIRIM KE KONTAK YANG SALAH.
	if agentID == "support" {
		if strings.TrimSpace(h.NovaPhone) == "" {
			log.Printf("[SPAWN] support tapi NOVA_PHONE kosong — dibatalkan")
			h.notifySpawnFailed(act.TargetName, act.Target, "nomor koordinator venue (Nova) tidak dikonfigurasi")
			return
		}
		act.Target = h.NovaPhone
		act.TargetName = "Nova"
		log.Printf("[SPAWN] support → koordinator venue Bu Nova (%s) [paksa; target LLM diabaikan]", h.NovaPhone)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	task := strings.TrimSpace(act.Task)
	chatID := waha.NormalizeChatID(act.Target) // "628...@c.us"
	phone := strings.TrimSuffix(chatID, "@c.us")

	// (2b) Koreksi nomor dari SUMBER MANUSIA. Nomor tujuan diketik langsung oleh
	// Pak Sudianto di salah satu pesannya; LLM kerap menjatuhkan 1 digit saat
	// menyalinnya ke field action.target (panjang masih "valid" → lolos
	// isValidMSISDN). Pemicu SPAWN bisa pesan TANPA nomor (mis. "tolong follow up"),
	// jadi kumpulkan nomor dari pesan pemicu DAN riwayat pesan SU di percakapan
	// orchestrator. Lalu cocokkan: bila target LLM bukan salah satu nomor manusia
	// tapi merupakan versi "garbled" (beda ≤1 digit) dari satu nomor manusia, atau
	// hanya ada satu nomor manusia, pakai nomor manusia yang otoritatif.
	humanNums := h.collectSUNumbers(ctx, initiator, srcText)
	// Untuk support, target sudah DIPAKSA = Nova di atas; jangan koreksi/pinjam nomor
	// manusia dari pesan SU (akar bug pesan venue nyasar ke kontak lain).
	if corrected, ok := reconcileMSISDN(phone, humanNums); ok && agentID != "support" {
		log.Printf("[SPAWN] koreksi nomor: target LLM=%q → %s (sumber: pesan SU, kandidat=%v)", phone, corrected, humanNums)
		phone = corrected
		chatID = waha.NormalizeChatID(phone)
	}

	// (2c) Resolve NAMA → nomor kontak tersimpan. Orchestrator kerap mengisi
	// act.Target dengan nama kontak yang sudah dikenal (mis. "nova") alih-alih nomor,
	// karena ia tak punya nomor di konteksnya — yang punya data otoritatif justru
	// sistem (kontak whitelisted). Bila target bukan MSISDN valid, coba cocokkan
	// act.TargetName / act.Target sebagai nama kontak dan pakai nomor tersimpan.
	if agentID != "support" && !isValidMSISDN(phone) && h.Store != nil {
		for _, cand := range []string{act.TargetName, act.Target} {
			cand = strings.TrimSpace(cand)
			if cand == "" || isValidMSISDN(cand) {
				continue
			}
			if c := h.resolveContactByLooseName(ctx, cand); c != nil && isValidMSISDN(c.Phone) {
				log.Printf("[SPAWN] resolve nama→kontak: %q → %s (%s)", cand, c.Phone, c.Name)
				phone = c.Phone
				chatID = waha.NormalizeChatID(phone)
				if strings.TrimSpace(act.TargetName) == "" {
					act.TargetName = c.Name
				}
				break
			}
		}
	}

	if task == "" || !isValidMSISDN(phone) {
		log.Printf("[SPAWN] target/task tidak valid (target=%q phone=%q task_len=%d) — dibatalkan", act.Target, phone, len(task))
		h.notifySpawnFailed(act.TargetName, act.Target, "nomor tujuan tidak valid")
		return
	}

	// (3) Rekonsiliasi target ke kontak yang SUDAH ada. Orchestrator (LLM) kadang
	// mengetik ulang nomor dari memorinya dan menjatuhkan digit — nomor kontak
	// tersimpan lebih otoritatif. Bila nama+perusahaan cocok dengan kontak yang
	// nomornya berbeda, pakai yang tersimpan (cegah kirim ke nomor salah & duplikat).
	var contact *model.Contact
	var err error
	if agentID == "support" {
		// Target sudah DIPAKSA = NOVA_PHONE (otoritatif dari env). Ambil kontak Nova
		// lewat NOMOR — JANGAN by-nama: bisa ada kontak lain yang juga bernama "Nova"
		// dengan nomor berbeda, dan FindContactByName akan menimpanya (akar bug pesan
		// venue "Halo Bu Nova ..." nyasar ke nomor salah).
		contact, err = h.Store.FindContact(ctx, model.Identifier{Kind: "phone", Value: phone})
		if err != nil {
			log.Printf("[SPAWN] kontak Nova (%s) belum ada (%v) — daftarkan", phone, err)
			contact, err = h.Store.EnsureContact(ctx, phone, "Bu Nova", "", "")
			if err != nil {
				log.Printf("[SPAWN] daftar kontak Nova %s gagal: %v", phone, err)
				return
			}
		}
	} else {
		contact, err = h.Store.FindContactByName(ctx, act.TargetName, act.TargetCompany)
		if err == nil && contact.Phone != phone {
			log.Printf("[SPAWN] rekonsiliasi nomor: target LLM=%s ≠ kontak tersimpan %q=%s — pakai nomor tersimpan",
				phone, contact.Name, contact.Phone)
			phone = contact.Phone
			chatID = waha.NormalizeChatID(phone)
		} else if errors.Is(err, db.ErrNotWhitelisted) || contact == nil {
			// Kontak belum ada (atau nama tak cocok): daftarkan baru dengan trust external.
			contact, err = h.Store.EnsureContact(ctx, phone, act.TargetName, act.TargetCompany, act.TargetEmail)
			if err != nil {
				log.Printf("[SPAWN] daftar kontak target %s gagal: %v", phone, err)
				return
			}
		} else if err != nil {
			log.Printf("[SPAWN] cari kontak target gagal: %v", err)
			return
		}
	}

	convID := "agent:" + agentID + ":" + phone
	log.Printf("[SPAWN] inisiasi SU → agent=%s target=%s conv=%s", agentID, chatID, convID)

	// (4) Frame instruksi: minta agent MENYUSUN pesan pembuka untuk dikirim ke kontak.
	injectMsg := buildSpawnInject(task)
	if h.Memory != nil {
		if mc, aerr := h.Memory.Assemble(ctx, convID, contact); aerr != nil {
			log.Printf("[SPAWN] assemble konteks gagal conv=%s: %v (lanjut tanpa konteks)", convID, aerr)
		} else {
			injectMsg = mc.BuildInjectMessage(injectMsg)
		}
	}

	reply, meta, err := h.injectWithRecovery(ctx, agentID, convID, injectMsg)
	if errors.Is(err, openclaw.ErrNoReply) {
		log.Printf("[SPAWN] agent memilih diam conv=%s — tidak ada pesan pembuka", convID)
		h.logExecution(ctx, convID, agentID, contact, injectMsg, nil, meta, "no_reply", "")
		return
	}
	if err != nil {
		log.Printf("[SPAWN] inject gagal conv=%s: %v", convID, err)
		h.logExecution(ctx, convID, agentID, contact, injectMsg, nil, meta, outcomeFromErr(err), err.Error())
		return
	}

	execID := h.logExecution(ctx, convID, agentID, contact, injectMsg, reply, meta, "ok", "")
	h.applyActions(ctx, convID, contact, reply.Actions, execID, "")

	// (5) Inisiasi oleh SU = auto-approve: kirim pesan pembuka LANGSUNG ke pihak
	// eksternal. SU sudah memerintahkan kontak ini, jadi tidak perlu menahan draf
	// untuk persetujuan ulang. Memori ditulis dulu agar balasan eksternal nyambung.
	userText := "[Inisiasi oleh Pak Sudianto] " + task
	if h.Memory != nil {
		if werr := h.Memory.Write(ctx, convID, contact, agentID, userText, reply.Response, reply.NewFacts); werr != nil {
			log.Printf("[SPAWN] memory write gagal conv=%s: %v", convID, werr)
		}
	}
	// Opener dikirim LANGSUNG ke pihak eksternal saja
	h.sendAndRecord(ctx, func() error { return h.Waha.SendToChat(chatID, reply.Response) },
		model.OutboundMessage{
			ExecutionID: execPtr(execID), ConversationID: convID, ContactID: cidPtr(contact),
			AgentID: agentID, TargetChat: chatID, Kind: "agent_reply", Text: reply.Response,
		})
	h.recordSpawnMeeting(ctx, convID, agentID, contact, act)
}

// spawnMeetingReusable memutuskan apakah meeting AKTIF yang sudah ada di percakapan boleh
// dipakai ulang (di-update) untuk permintaan SPAWN baru, ATAU harus dibuat baris baru.
// Hanya boleh reuse bila meeting masih 'pending' (belum dijadwalkan/disetujui) DAN tanggalnya
// kompatibel (salah satu tanpa tanggal, atau tanggal WIB sama). Ini mencegah konflasi: dulu
// permintaan meeting baru menimpa meeting LAMA yang sudah 'scheduled' di percakapan eksternal
// yang sama → satu baris berisi campuran data dua meeting berbeda (mis. #7: datetime/topik baru
// tapi eventId/approval milik meeting lama).
func spawnMeetingReusable(existing *model.MeetingRequest, newProposed *time.Time) bool {
	if existing == nil || existing.Status != "pending" {
		return false
	}
	if existing.ProposedDatetime == nil || newProposed == nil {
		return true
	}
	return existing.ProposedDatetime.In(wibZone).Format("2006-01-02") ==
		newProposed.In(wibZone).Format("2006-01-02")
}

// recordSpawnMeeting membuat (atau memperbarui) baris meeting_requests berstatus
// 'pending' untuk meeting yang DIINISIASI SU lewat SPAWN_AGENT.
func (h *Handler) recordSpawnMeeting(ctx context.Context, convID, agentID string, contact *model.Contact, act model.Action) {
	if h.Store == nil {
		return
	}
	// Spawn ke 'support' = koordinasi venue internal (ke Bu Nova), BUKAN meeting dengan
	// peserta eksternal — jangan buat baris meeting_requests untuk percakapan itu (cukup
	// satu baris meeting di percakapan pa_communicator/eksternal yang otoritatif).
	if agentID == "support" {
		return
	}
	topic := strings.TrimSpace(act.MeetingTopic)
	venue := strings.TrimSpace(act.MeetingVenue)
	var proposed *time.Time
	if dt := strings.TrimSpace(act.MeetingDatetime); dt != "" {
		if t, err := time.Parse(time.RFC3339, dt); err == nil {
			proposed = &t
		} else {
			log.Printf("[SPAWN-MEETING] datetime agent tak valid (%q): %v — diabaikan", dt, err)
		}
	}
	if topic == "" && proposed == nil {
		return
	}
	meetingType := "online"
	if venue != "" {
		meetingType = "onsite"
	}

	if existing, err := h.Store.ActiveMeetingByConversation(ctx, convID); err != nil {
		log.Printf("[SPAWN-MEETING] cek meeting aktif conv=%s gagal: %v", convID, err)
	} else if existing != nil && spawnMeetingReusable(existing, proposed) {
		if err := h.Store.UpdateMeetingPlan(ctx, existing.ID, topic, meetingType, venue, proposed, "su", "rencana diperbarui dari inisiasi SU"); err != nil {
			log.Printf("[SPAWN-MEETING] update #%d gagal: %v", existing.ID, err)
		} else {
			log.Printf("[SPAWN-MEETING] #%d diperbarui (conv=%s datetime=%v topic=%q)", existing.ID, convID, proposed, topic)
		}
		// Meeting offline → pastikan ditandai venue-coordination agar finalisasinya
		// ditahan sampai lokasi pasti dikonfirmasi Bu Nova.
		if meetingType == "onsite" {
			h.markVenueCoordination(ctx, existing)
		}
		return
	}

	name, company, email := "", "", ""
	if contact != nil {
		name, company, email = contact.Name, contact.Company, contact.Email
	}
	det := meetingDetails{Title: topic, AttendeeName: name, AttendeeEmail: email}
	// Meeting offline (venue diisi sebagai area/preferensi) → tandai venue-coordination:
	// lokasi pasti belum ada (VenueConfirmed=false), finalisasi ditahan sampai Bu Nova
	// memastikan venue lewat CONFIRM_VENUE.
	if meetingType == "onsite" {
		det.VenueCoordination = true
	}
	details, _ := json.Marshal(det)
	mid, err := h.Store.CreateMeetingRequest(ctx, model.MeetingRequest{
		ConversationID: convID, ContactID: cidPtr(contact), AgentID: agentID,
		RequestedVia: "su", ExternalName: name, ExternalCompany: company,
		Topic: topic, MeetingType: meetingType, ProposedDatetime: proposed, Venue: venue,
		Status: "pending", Details: details,
	}, "su")
	if err != nil {
		log.Printf("[SPAWN-MEETING] buat gagal conv=%s: %v", convID, err)
		return
	}
	log.Printf("[SPAWN-MEETING] #%d dibuat (pending, via=su) conv=%s datetime=%v topic=%q", mid, convID, proposed, topic)
}

// notifySpawnFailed memberi tahu SU jika pesan pembuka gagal dikirim.
func (h *Handler) notifySpawnFailed(targetName, target, reason string) {
	if h.SUPhone == "" {
		return
	}
	who := strings.TrimSpace(targetName)
	if who == "" {
		who = target
	}
	msg := fmt.Sprintf("⚠️ Pesan pembuka ke %s GAGAL dikirim: %s (tujuan: %s). Mohon periksa kembali nomornya, Pak.",
		who, reason, target)
	if err := h.Waha.SendText(h.SUPhone, msg); err != nil {
		log.Printf("[SPAWN] notifikasi gagal-kirim ke SU error: %v", err)
	}
}

// buildFollowupInject untuk lanjutan percakapan ke kontak yang sudah dikenal.
func buildFollowupInject(task string) string {
	return "[TUGAS INTERNAL DARI ORCHESTRATOR — lanjutkan percakapan WhatsApp dengan kontak yang SUDAH kamu kenal, atas nama Pak Sudianto]\n" +
		"Instruksi: " + task + "\n\n" +
		"Susun SATU pesan WhatsApp yang sopan & profesional sesuai instruksi. JANGAN memperkenalkan diri lagi " +
		"(kontak sudah mengenalmu). Tulis isi pesan pada field \"response\" — JANGAN menyapa orchestrator. " +
		"Bila instruksi meminta menegosiasikan jadwal baru: setelah ada kesepakatan waktu dengan kontak, ajukan " +
		"untuk persetujuan Pak Sudianto (requiresApproval: true + objek meeting berisi waktu yang disepakati)."
}

// buildSpawnInject membungkus tugas dari orchestrator menjadi instruksi yang jelas
// bagi agent penghubung untuk menyusun satu pesan pembuka WhatsApp.
func buildSpawnInject(task string) string {
	return "[TUGAS INTERNAL DARI ORCHESTRATOR — kamu diminta MEMULAI percakapan WhatsApp dengan kontak baru atas nama Pak Sudianto]\n" +
		"Instruksi: " + task + "\n\n" +
		"Susun SATU pesan pembuka WhatsApp yang sopan, singkat, dan profesional sesuai instruksi di atas. " +
		"Perkenalkan dirimu sebagai asisten Pak Sudianto. Tulis isi pesan pembuka itu pada field \"response\" — " +
		"JANGAN menyapa orchestrator; tulis langsung teks yang akan dikirim ke kontak."
}

// isDigits true bila s hanya berisi angka (dan tidak kosong).
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// normalisasi & validasi dilakukan di extractMSISDNs.
var phoneTokenRe = regexp.MustCompile(`[+0-9][0-9\s().\-]{7,}[0-9]`)

// extractMSISDNs mengambil semua nomor Indonesia yang valid dari teks bebas (mis.
// pesan asli SU) dan menormalkannya ke E.164 tanpa '+': "62XXXXXXXXXX".
func extractMSISDNs(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, tok := range phoneTokenRe.FindAllString(text, -1) {
		digits := nonDigitRe.ReplaceAllString(tok, "")
		if strings.HasPrefix(digits, "0") {
			digits = "62" + digits[1:]
		}
		if !isValidMSISDN(digits) || seen[digits] {
			continue
		}
		seen[digits] = true
		out = append(out, digits)
	}
	return out
}

// nonDigitRe membuang semua karakter selain digit.
var nonDigitRe = regexp.MustCompile(`\D`)

// collectSUNumbers mengumpulkan nomor (MSISDN ter-normalisasi) yang pernah DIKETIK
func (h *Handler) collectSUNumbers(ctx context.Context, initiator *model.Contact, srcText string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(nums []string) {
		for _, n := range nums {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	add(extractMSISDNs(srcText))
	if h.Store != nil && initiator != nil && initiator.Phone != "" {
		suConv := "agent:orchestrator:" + initiator.Phone
		if msgs, err := h.Store.RecentMessages(ctx, suConv, 30); err == nil {
			for _, m := range msgs {
				if m.Role == "user" {
					add(extractMSISDNs(m.Text))
				}
			}
		} else {
			log.Printf("[SPAWN] ambil riwayat SU gagal (%s): %v", suConv, err)
		}
	}
	return out
}

// reconcileMSISDN menyesuaikan nomor LLM ke nomor manusia bila cocok jelas.
func reconcileMSISDN(llm string, humanNums []string) (string, bool) {
	if len(humanNums) == 0 {
		return llm, false
	}
	for _, hn := range humanNums {
		if hn == llm {
			return llm, false // LLM sudah benar
		}
	}
	var near []string
	for _, hn := range humanNums {
		if editDistanceLE1(llm, hn) {
			near = append(near, hn)
		}
	}
	if len(near) == 1 {
		return near[0], true
	}
	if len(near) == 0 && len(humanNums) == 1 {
		return humanNums[0], true
	}
	return llm, false
}

// editDistanceLE1 true bila a dan b berbeda paling banyak satu operasi (sisip,
// hapus, atau ganti satu karakter) — cukup untuk mendeteksi digit-drop/typo nomor.
func editDistanceLE1(a, b string) bool {
	la, lb := len(a), len(b)
	if la > lb {
		a, b = b, a
		la, lb = lb, la
	}
	if lb-la > 1 {
		return false
	}
	i, j, edits := 0, 0, 0
	for i < la && j < lb {
		if a[i] == b[j] {
			i++
			j++
			continue
		}
		edits++
		if edits > 1 {
			return false
		}
		if la == lb {
			i++ // ganti
		}
		j++
	}
	edits += lb - j
	return edits <= 1
}

// isValidMSISDN memvalidasi nomor tujuan (E.164 Indonesia: 62 + 9-13 digit).
// Menolak format rusak: bukan digit, panjang invalid, atau bukan E.164.
func isValidMSISDN(s string) bool {
	if !isDigits(s) {
		return false
	}
	if len(s) < 10 || len(s) > 15 {
		return false
	}
	// Nomor Indonesia diharapkan berawalan 62 (E.164)
	if strings.HasPrefix(s, "62") {
		return len(s) >= 11 && len(s) <= 15
	}
	return true
}

// nameHonorifics: gelar/sapaan yang diabaikan saat mencocokkan nama kontak.
var nameHonorifics = map[string]bool{
	"bu": true, "ibu": true, "pak": true, "bapak": true, "bpk": true,
	"mas": true, "mbak": true, "sdr": true, "sdri": true,
	"mr": true, "mrs": true, "ms": true, "dr": true,
}

// normalizeNameTokens memecah nama menjadi token huruf-kecil tanpa honorifik.
// "Bu Nova" → ["nova"]; "Pak Andrew Wijaya" → ["andrew","wijaya"].
func normalizeNameTokens(name string) []string {
	var out []string
	for _, f := range strings.Fields(strings.ToLower(strings.TrimSpace(name))) {
		f = strings.Trim(f, ".,()")
		if f == "" || nameHonorifics[f] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// resolveContactByLooseName mencari kontak whitelist dengan nama yang cocok, abaikan honorifik.
// Mengembalikan kontak bila tepat SATU yang cocok.
func (h *Handler) resolveContactByLooseName(ctx context.Context, name string) *model.Contact {
	cand := normalizeNameTokens(name)
	if len(cand) == 0 || h.Store == nil {
		return nil
	}
	contacts, err := h.Store.ListContacts(ctx, false)
	if err != nil {
		log.Printf("[SPAWN] list kontak utk resolve %q gagal: %v", name, err)
		return nil
	}
	var match *model.Contact
	for i := range contacts {
		stored := normalizeNameTokens(contacts[i].Name)
		if len(stored) == 0 {
			continue
		}
		ok := true
		for _, ct := range cand {
			found := false
			for _, st := range stored {
				if ct == st {
					found = true
					break
				}
			}
			if !found {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if match != nil && match.ID != contacts[i].ID {
			log.Printf("[SPAWN] resolve nama %q ambigu (>1 kontak cocok) — dibatalkan", name)
			return nil
		}
		c := contacts[i]
		match = &c
	}
	return match
}

// confirmExistingMeeting menyelesaikan approval meeting yang sudah ada.
// Hanya inisiator ber-trust 'su' yang diizinkan.
func (h *Handler) confirmExistingMeeting(ctx context.Context, initiator *model.Contact, a model.Action) {
	if initiator == nil || initiator.TrustLevel != "su" {
		trust := "(nil)"
		if initiator != nil {
			trust = initiator.TrustLevel
		}
		log.Printf("[CONFIRM] DITOLAK: inisiator non-SU (trust=%s) approvalId=%d", trust, a.ApprovalID)
		return
	}
	apID := a.ApprovalID
	if apID <= 0 {
		log.Printf("[CONFIRM] approvalId tidak valid (%d) — diabaikan", apID)
		return
	}
	ap, err := h.Store.GetApproval(ctx, apID)
	if err != nil || ap == nil {
		log.Printf("[CONFIRM] approval #%d tidak ditemukan: %v", apID, err)
		return
	}
	if ap.Status != "pending" {
		log.Printf("[CONFIRM] approval #%d sudah '%s' — tidak diproses ulang", apID, ap.Status)
		return
	}
	msg, derr := h.DecideApproval(ctx, apID, true)
	if derr != nil {
		log.Printf("[CONFIRM] decide approval #%d gagal: %v", apID, derr)
		return
	}
	log.Printf("[CONFIRM] approval #%d dikonfirmasi via orchestrator — %s", apID, msg)
}

// externalChatIDForMeeting menentukan chatId WAHA pihak eksternal sebuah meeting.
func (h *Handler) externalChatIDForMeeting(ctx context.Context, m *model.MeetingRequest) string {
	if m.ApprovalID != nil {
		if ap, err := h.Store.GetApproval(ctx, *m.ApprovalID); err == nil && ap != nil {
			if t := strings.TrimSpace(ap.TargetChat); t != "" {
				return t
			}
		}
	}
	parts := strings.SplitN(m.ConversationID, ":", 3)
	if len(parts) == 3 {
		if kid := strings.TrimSpace(parts[2]); kid != "" {
			if strings.Contains(kid, "@") {
				return kid
			}
			return waha.NormalizeChatID(kid) // anggap nomor telepon → "...@c.us"
		}
	}
	return ""
}

// notifyExternalWA mengirim pesan WhatsApp ke pihak eksternal sebuah meeting (fallback
// template; jalur utama komunikasi reschedule/cancel adalah lewat PA Communicator).
func (h *Handler) notifyExternalWA(ctx context.Context, m *model.MeetingRequest, msg string) error {
	chat := h.externalChatIDForMeeting(ctx, m)
	if chat == "" {
		return fmt.Errorf("tidak ada chat eksternal untuk meeting #%d", m.ID)
	}
	return h.Waha.SendToChat(chat, msg)
}

// dispatchCommunicator menugaskan agent penghubung untuk mengirim pesan ke pihak eksternal.
func (h *Handler) dispatchCommunicator(ctx context.Context, m *model.MeetingRequest, task string) error {
	agentID := strings.TrimSpace(m.AgentID)
	if agentID != "pa_communicator" && agentID != "support" {
		agentID = "pa_communicator"
	}
	convID := strings.TrimSpace(m.ConversationID)
	chatID := h.externalChatIDForMeeting(ctx, m)
	if convID == "" || chatID == "" {
		return fmt.Errorf("konteks eksternal meeting #%d tak lengkap (conv=%q chat=%q)", m.ID, convID, chatID)
	}

	var det meetingDetails
	_ = json.Unmarshal(m.Details, &det)
	phone := ""
	if parts := strings.SplitN(convID, ":", 3); len(parts) == 3 {
		phone = parts[2]
	}
	contact := &model.Contact{
		Name: m.ExternalName, Company: m.ExternalCompany,
		Email: det.AttendeeEmail, Phone: phone, TrustLevel: "external",
	}
	if m.ContactID != nil {
		contact.ID = *m.ContactID
	}

	injectMsg := buildFollowupInject(task)
	if h.Memory != nil {
		if mc, aerr := h.Memory.Assemble(ctx, convID, contact); aerr != nil {
			log.Printf("[DISPATCH] assemble konteks gagal conv=%s: %v (lanjut tanpa konteks)", convID, aerr)
		} else {
			mc.LiveStatus = buildDateAnchor() // acuan tanggal saat negosiasi waktu baru
			injectMsg = mc.BuildInjectMessage(injectMsg)
		}
	}

	reply, meta, err := h.injectWithRecovery(ctx, agentID, convID, injectMsg)
	if errors.Is(err, openclaw.ErrNoReply) {
		h.logExecution(ctx, convID, agentID, contact, injectMsg, nil, meta, "no_reply", "")
		return fmt.Errorf("agent %s memilih diam", agentID)
	}
	if err != nil {
		h.logExecution(ctx, convID, agentID, contact, injectMsg, nil, meta, outcomeFromErr(err), err.Error())
		return err
	}
	execID := h.logExecution(ctx, convID, agentID, contact, injectMsg, reply, meta, "ok", "")
	h.applyActions(ctx, convID, contact, reply.Actions, execID, "")

	// Bila agent langsung mengajukan kesepakatan jadwal, tahan di approval gate.
	if reply.RequiresApproval {
		h.holdForApproval(ctx, convID, agentID, contact, chatID, "["+task+"]", reply, execID)
		return nil
	}

	if h.Memory != nil {
		if werr := h.Memory.Write(ctx, convID, contact, agentID, "[Inisiasi Pak Sudianto] "+task, reply.Response, reply.NewFacts); werr != nil {
			log.Printf("[DISPATCH] memory write gagal conv=%s: %v", convID, werr)
		}
	}
	h.sendAndRecord(ctx, func() error { return h.Waha.SendToChat(chatID, reply.Response) },
		model.OutboundMessage{
			ExecutionID: execPtr(execID), ConversationID: convID, ContactID: cidPtr(contact),
			AgentID: agentID, TargetChat: chatID, Kind: "agent_reply", Text: reply.Response,
		})
	return nil
}

// dispatchVenueRecoordination menugaskan agent 'support' menghubungi Bu Nova untuk
// mengkoordinasikan ULANG venue meeting offline yang sedang di-reschedule. Bu Nova boleh
// mempertahankan atau MENGGANTI lokasi (bila slot waktu baru bentrok); setelah lokasi
// pasti ia mengonfirmasi via CONFIRM_VENUE (sertakan tanggal meeting), yang lalu memicu
// pengajuan paket lengkap (waktu baru + venue) ke SU. Berjalan di goroutine sendiri.
func (h *Handler) dispatchVenueRecoordination(meetingID int64, ed meetingDetails, newTime *time.Time) {
	if strings.TrimSpace(h.NovaPhone) == "" {
		log.Printf("[VENUE-RECOORD] NovaPhone kosong — tidak bisa koordinasi ulang venue meeting #%d", meetingID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	m, err := h.Store.MeetingByID(ctx, meetingID)
	if err != nil || m == nil {
		log.Printf("[VENUE-RECOORD] meeting #%d tidak ditemukan: %v", meetingID, err)
		return
	}

	convID := "agent:support:" + h.NovaPhone
	chatID := waha.NormalizeChatID(h.NovaPhone)
	nova := &model.Contact{Name: "Nova", Phone: h.NovaPhone, TrustLevel: "semi_trusted"}

	who := firstNonEmptyStr(m.ExternalName, ed.AttendeeName, "pihak eksternal")
	prevVenue := firstNonEmptyStr(strings.TrimSpace(m.Venue), ed.VenueName)
	var b strings.Builder
	b.WriteString("Pak Sudianto MENJADWAL ULANG pertemuan tatap muka dengan " + who + ".")
	if newTime != nil {
		b.WriteString(" Waktu baru yang sudah disepakati: " + formatWIBLong(*newTime) + ".")
	}
	if prevVenue != "" {
		b.WriteString(" Lokasi sebelumnya: " + prevVenue + ".")
	}
	b.WriteString(" Mohon pastikan apakah lokasi tersebut masih tersedia pada waktu baru; bila TIDAK, " +
		"carikan alternatif lokasi yang sesuai. Setelah lokasi PASTI, konfirmasikan venue " +
		"(CONFIRM_VENUE) dan sertakan tanggal meeting waktu baru agar terpetakan ke pertemuan yang benar. " +
		"Tanpa membahas biaya.")

	injectMsg := buildFollowupInject(b.String())
	if h.Memory != nil {
		if mc, aerr := h.Memory.Assemble(ctx, convID, nova); aerr != nil {
			log.Printf("[VENUE-RECOORD] assemble konteks gagal conv=%s: %v (lanjut tanpa konteks)", convID, aerr)
		} else {
			mc.LiveStatus = buildDateAnchor()
			injectMsg = mc.BuildInjectMessage(injectMsg)
		}
	}

	reply, meta, ierr := h.injectWithRecovery(ctx, "support", convID, injectMsg)
	if errors.Is(ierr, openclaw.ErrNoReply) {
		h.logExecution(ctx, convID, "support", nova, injectMsg, nil, meta, "no_reply", "")
		log.Printf("[VENUE-RECOORD] support memilih diam untuk meeting #%d", m.ID)
		return
	}
	if ierr != nil {
		h.logExecution(ctx, convID, "support", nova, injectMsg, nil, meta, outcomeFromErr(ierr), ierr.Error())
		log.Printf("[VENUE-RECOORD] inject support gagal meeting #%d: %v", m.ID, ierr)
		return
	}
	execID := h.logExecution(ctx, convID, "support", nova, injectMsg, reply, meta, "ok", "")
	// Bila support langsung CONFIRM_VENUE (mis. lokasi lama masih tersedia), aksi ini akan
	// mengonfirmasi venue & memicu pengajuan paket ke SU.
	h.applyActions(ctx, convID, nova, reply.Actions, execID, "")

	if h.Memory != nil {
		if werr := h.Memory.Write(ctx, convID, nova, "support", "[Koordinasi ulang venue reschedule] "+b.String(), reply.Response, reply.NewFacts); werr != nil {
			log.Printf("[VENUE-RECOORD] memory write gagal conv=%s: %v", convID, werr)
		}
	}
	h.sendAndRecord(ctx, func() error { return h.Waha.SendToChat(chatID, reply.Response) },
		model.OutboundMessage{
			ExecutionID: execPtr(execID), ConversationID: convID, ContactID: cidPtr(nova),
			AgentID: "support", TargetChat: chatID, Kind: "agent_reply", Text: reply.Response,
		})
	log.Printf("[VENUE-RECOORD] Bu Nova ditugaskan koordinasi ulang venue meeting #%d (waktu baru=%v)", m.ID, newTime)
}

// rescheduleDispatch (RESCHEDULE_MEETING, SU): tandai meeting sedang dijadwal ulang,
// lalu tugaskan PA Communicator menanyakan kesediaan pihak eksternal.
func (h *Handler) rescheduleDispatch(initiator *model.Contact, a model.Action) {
	if initiator == nil || initiator.TrustLevel != "su" {
		trust := "(nil)"
		if initiator != nil {
			trust = initiator.TrustLevel
		}
		log.Printf("[RESCHEDULE] DITOLAK: inisiator non-SU (trust=%s) meetingId=%d", trust, a.MeetingID)
		return
	}
	if a.MeetingID <= 0 {
		log.Printf("[RESCHEDULE] meetingId tidak valid (%d) — diabaikan", a.MeetingID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	m, err := h.Store.MeetingByID(ctx, a.MeetingID)
	if err != nil || m == nil {
		log.Printf("[RESCHEDULE] meeting #%d tidak ditemukan: %v", a.MeetingID, err)
		h.notifySU(fmt.Sprintf("Maaf, meeting #%d tidak ditemukan. Reschedule dibatalkan.", a.MeetingID))
		return
	}
	if m.Status == "cancelled" || m.Status == "rejected" {
		h.notifySU(fmt.Sprintf("Meeting #%d sudah %s — tidak dapat dijadwalkan ulang.", m.ID, meetingStatusID(m.Status)))
		return
	}

	var det meetingDetails
	_ = json.Unmarshal(m.Details, &det)
	oldTime := ""
	if m.ProposedDatetime != nil {
		oldTime = m.ProposedDatetime.Format(time.RFC3339)
	}
	det.ReschedulePending = true
	det.RescheduleFrom = oldTime

	// Meeting offline (venue-coordinated): venue harus dikoordinasikan ULANG untuk waktu
	// baru — Bu Nova bisa mempertahankan atau MENGGANTI lokasi bila slot baru bentrok.
	// Reset penanda venue & lepas approval lama agar loop venue + approval berjalan ulang;
	// paket lengkap (waktu baru + venue) baru diajukan ke SU setelah Nova mengonfirmasi.
	offlineRecoord := det.VenueCoordination
	if offlineRecoord {
		det.VenueConfirmed = false
		det.TimeAgreed = false
		merged, _ := json.Marshal(det)
		if uerr := h.Store.ReopenMeetingForReschedule(ctx, m.ID, merged, "su",
			"reschedule meeting offline — koordinasi ulang venue + waktu"); uerr != nil {
			log.Printf("[RESCHEDULE] reopen offline #%d gagal: %v", m.ID, uerr)
		}
	} else {
		merged, _ := json.Marshal(det)
		if uerr := h.Store.UpdateMeetingDetails(ctx, m.ID, merged, "su", "reschedule diminta SU — menunggu konfirmasi pihak eksternal"); uerr != nil {
			log.Printf("[RESCHEDULE] tandai pending #%d gagal: %v", m.ID, uerr)
		}
	}

	newAt, perr := time.Parse(time.RFC3339, strings.TrimSpace(a.NewDatetime))
	venue := strings.TrimSpace(a.MeetingVenue)
	var b strings.Builder
	b.WriteString("Pak Sudianto ingin MENJADWAL ULANG pertemuan yang sudah disepakati dengan kontak ini.")
	if m.ProposedDatetime != nil {
		b.WriteString(" Jadwal saat ini: " + formatWIBLong(*m.ProposedDatetime) + ".")
	}
	if perr == nil {
		b.WriteString(" Usulan jadwal baru dari Pak Sudianto: " + formatWIBLong(newAt) + ".")
	}
	if venue != "" {
		b.WriteString(" Lokasi: " + venue + ".")
	}
	if r := strings.TrimSpace(a.Reason); r != "" {
		b.WriteString(" Alasan: " + r + ".")
	}
	b.WriteString(" Tanyakan dengan sopan apakah kontak BERSEDIA pada jadwal baru tersebut. " +
		"Bila TIDAK bisa, tanyakan kapan waktu yang memungkinkan bagi mereka. JANGAN memastikan " +
		"jadwal sebagai final — begitu ada kesepakatan waktu, ajukan untuk persetujuan Pak Sudianto " +
		"(requiresApproval: true + objek meeting dengan waktu yang disepakati).")
	if offlineRecoord {
		b.WriteString(" Catatan: ini pertemuan tatap muka — cukup sepakati WAKTU dulu; lokasi akan " +
			"kami koordinasikan ulang secara terpisah dan dikonfirmasikan kembali bersama jadwal final.")
	}

	if derr := h.dispatchCommunicator(ctx, m, b.String()); derr != nil {
		log.Printf("[RESCHEDULE] dispatch PA Communicator #%d gagal: %v", m.ID, derr)
		h.notifySU(fmt.Sprintf("⚠️ Gagal menghubungi pihak terkait untuk reschedule meeting #%d. Silakan coba lagi sebentar.", m.ID))
		return
	}
	log.Printf("[RESCHEDULE] PA Communicator ditugaskan menegosiasikan waktu baru meeting #%d", m.ID)
}

// cancelDispatch: tugaskan PA Communicator untuk mengabari pihak eksternal lalu finalisasi pembatalan.
func (h *Handler) cancelDispatch(initiator *model.Contact, a model.Action) {
	if initiator == nil || initiator.TrustLevel != "su" {
		trust := "(nil)"
		if initiator != nil {
			trust = initiator.TrustLevel
		}
		log.Printf("[CANCEL] DITOLAK: inisiator non-SU (trust=%s) meetingId=%d", trust, a.MeetingID)
		return
	}
	if a.MeetingID <= 0 {
		log.Printf("[CANCEL] meetingId tidak valid (%d) — diabaikan", a.MeetingID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	m, err := h.Store.MeetingByID(ctx, a.MeetingID)
	if err != nil || m == nil {
		log.Printf("[CANCEL] meeting #%d tidak ditemukan: %v", a.MeetingID, err)
		h.notifySU(fmt.Sprintf("Maaf, meeting #%d tidak ditemukan. Pembatalan dibatalkan.", a.MeetingID))
		return
	}
	if m.Status == "cancelled" || m.Status == "rejected" {
		h.notifySU(fmt.Sprintf("Meeting #%d memang sudah %s.", m.ID, meetingStatusID(m.Status)))
		return
	}

	var det meetingDetails
	_ = json.Unmarshal(m.Details, &det)
	title := firstNonEmptyStr(det.Title, m.Topic, "pertemuan")
	duration := det.DurationMinutes
	if duration <= 0 {
		duration = 60
	}
	reason := strings.TrimSpace(a.Reason)

	// 1) PA Communicator mengabari pihak eksternal (pesan natural). Fallback ke
	// template bila dispatch gagal, agar pihak eksternal tetap diberi tahu.
	var b strings.Builder
	b.WriteString("Pak Sudianto perlu MEMBATALKAN pertemuan dengan kontak ini.")
	if m.ProposedDatetime != nil {
		b.WriteString(" Jadwal yang dibatalkan: " + formatWIBLong(*m.ProposedDatetime) + ".")
	}
	if reason != "" {
		b.WriteString(" Alasan: " + reason + ".")
	}
	b.WriteString(" Sampaikan pembatalan ini dengan sopan beserta permohonan maaf, dan sebutkan bahwa " +
		"kami akan menghubungi kembali bila ada kesempatan penjadwalan ulang. JANGAN menjanjikan jadwal pengganti yang spesifik.")
	if derr := h.dispatchCommunicator(ctx, m, b.String()); derr != nil {
		log.Printf("[CANCEL] dispatch PA Communicator #%d gagal: %v — fallback WA template", m.ID, derr)
		who := firstNonEmptyStr(m.ExternalName, det.AttendeeName, "Bapak/Ibu")
		fb := fmt.Sprintf("Halo %s, mohon maaf — pertemuan \"%s\"", who, title)
		if m.ProposedDatetime != nil {
			fb += fmt.Sprintf(" yang dijadwalkan %s", formatWIBLong(*m.ProposedDatetime))
		}
		fb += " terpaksa dibatalkan. Mohon maaf atas ketidaknyamanannya; kami akan menghubungi kembali bila ada jadwal pengganti."
		if werr := h.notifyExternalWA(ctx, m, fb); werr != nil {
			log.Printf("[CANCEL] fallback WA #%d gagal: %v", m.ID, werr)
		}
	}

	// 2) Finalisasi deterministik: hapus event, status cancelled, email pembatalan.
	calNote := ""
	if h.Services.Enabled() && det.EventID != "" {
		if cerr := h.Services.CancelEvent(ctx, det.EventID); cerr != nil {
			log.Printf("[CANCEL] hapus event meeting #%d gagal: %v", m.ID, cerr)
			calNote = " (gagal menghapus event kalender — periksa manual)"
		} else {
			log.Printf("[CANCEL] event %s meeting #%d dihapus", det.EventID, m.ID)
		}
	}
	if uerr := h.Store.UpdateMeetingStatus(ctx, m.ID, "cancelled", "su", firstNonEmptyStr(reason, "dibatalkan oleh Pak Sudianto")); uerr != nil {
		log.Printf("[CANCEL] update status meeting #%d gagal: %v", m.ID, uerr)
	}
	// Batalkan pengingat otomatis yang masih menunggu untuk meeting ini.
	if cerr := h.Store.CancelTasksForMeeting(ctx, m.ID); cerr != nil {
		log.Printf("[CANCEL] batalkan pengingat meeting #%d gagal: %v", m.ID, cerr)
	}
	emailNote := ""
	if h.Services.Enabled() && det.AttendeeEmail != "" && m.ProposedDatetime != nil {
		if serr := h.Services.SendCancellation(ctx, services.CancellationReq{
			To: det.AttendeeEmail, ToName: firstNonEmptyStr(det.AttendeeName, m.ExternalName, det.AttendeeEmail),
			Title: title, Datetime: m.ProposedDatetime.Format(time.RFC3339), DurationMinutes: duration,
			Reason: reason,
		}); serr != nil {
			log.Printf("[CANCEL] email pembatalan meeting #%d gagal: %v", m.ID, serr)
			emailNote = " (email gagal terkirim)"
		} else {
			log.Printf("[CANCEL] email pembatalan terkirim ke %s meeting #%d", det.AttendeeEmail, m.ID)
		}
	}

	who := firstNonEmptyStr(m.ExternalName, det.AttendeeName, "pihak terkait")
	h.notifySU(fmt.Sprintf("✅ Meeting #%d dengan %s dibatalkan. PA Communicator sudah mengabari pihak terkait%s%s.",
		m.ID, who, emailNote, calNote))
}

// requestMeetingChange meneruskan permintaan perubahan jadwal dari pihak eksternal ke Pak Sudianto.
func (h *Handler) requestMeetingChange(ctx context.Context, convID string, contact *model.Contact, a model.Action) {
	// Inisiator harus pihak eksternal (bukan SU) — SU memakai aksi eksekusi langsung.
	if contact != nil && contact.TrustLevel == "su" {
		log.Printf("[CHANGE-REQ] diabaikan: inisiator SU (pakai RESCHEDULE/CANCEL langsung)")
		return
	}
	m, err := h.Store.ActiveMeetingByConversation(ctx, convID)
	if err != nil {
		log.Printf("[CHANGE-REQ] cari meeting aktif conv=%s gagal: %v", convID, err)
		return
	}
	if m == nil {
		log.Printf("[CHANGE-REQ] tidak ada meeting aktif untuk conv=%s — diabaikan", convID)
		return
	}
	kind := strings.ToLower(strings.TrimSpace(a.ChangeKind))
	who := firstNonEmptyStr(m.ExternalName, "Pihak eksternal")
	when := ""
	if m.ProposedDatetime != nil {
		when = " (jadwal saat ini: " + formatWIBLong(*m.ProposedDatetime) + ")"
	}
	reason := strings.TrimSpace(a.Reason)

	var msg string
	switch kind {
	case "cancel":
		msg = fmt.Sprintf("📩 %s meminta *pembatalan* meeting #%d%s.", who, m.ID, when)
		if reason != "" {
			msg += " Alasan: " + reason + "."
		}
		msg += fmt.Sprintf("\n\nUntuk membatalkan, beri tahu saya: \"batalkan meeting #%d\". Untuk menolak permintaan, abaikan saja.", m.ID)
	case "reschedule":
		msg = fmt.Sprintf("📩 %s meminta *perubahan jadwal* meeting #%d%s.", who, m.ID, when)
		if nt := strings.TrimSpace(a.NewDatetime); nt != "" {
			if t, perr := time.Parse(time.RFC3339, nt); perr == nil {
				msg += " Usulan waktu baru: " + formatWIBLong(t) + "."
			}
		}
		if reason != "" {
			msg += " Alasan: " + reason + "."
		}
		msg += fmt.Sprintf("\n\nUntuk menyetujui, beri tahu saya: \"ubah meeting #%d ke <tanggal & jam>\".", m.ID)
	default:
		log.Printf("[CHANGE-REQ] changeKind tak dikenal (%q) conv=%s — diabaikan", a.ChangeKind, convID)
		return
	}
	h.notifySU(msg)
	log.Printf("[CHANGE-REQ] permintaan %q meeting #%d dari %s diteruskan ke SU", kind, m.ID, who)
}

// notifySU mengirim satu pesan WhatsApp ringkas ke Pak Sudianto (helper kecil).
func (h *Handler) notifySU(msg string) {
	if h.SUPhone == "" {
		log.Printf("[NOTIFY-SU] SU phone kosong — pesan dilewati: %s", msg)
		return
	}
	if err := h.Waha.SendText(h.SUPhone, msg); err != nil {
		log.Printf("[NOTIFY-SU] kirim ke SU gagal: %v", err)
	}
}

// meetingStatusID menerjemahkan status terminal ke kata Indonesia singkat.
func meetingStatusID(status string) string {
	switch status {
	case "cancelled":
		return "dibatalkan"
	case "rejected":
		return "ditolak"
	case "completed":
		return "selesai"
	default:
		return status
	}
}

// notifyOrchestrator meneruskan payload NOTIFY_ORCHESTRATOR ke Pak Sudianto via WA
// dan mencatatnya ke outbound_messages.
func (h *Handler) notifyOrchestrator(ctx context.Context, convID string, contact *model.Contact, payload json.RawMessage, execID int64) {
	if h.SUPhone == "" {
		log.Printf("[ACTION] notify_orchestrator: SU phone kosong, dilewati")
		return
	}
	who := "kontak"
	if contact != nil && contact.Name != "" {
		who = contact.Name
	}
	body := strings.TrimSpace(string(payload))
	if body == "" {
		body = "(tanpa detail)"
	}
	// SU hanya notifySUApproval yang dikirim NOTIFY_ORCHESTRATOR hanya untuk audit/trace.
	log.Printf("[ACTION] notify_orchestrator (TIDAK diteruskan ke SU) conv=%s dari=%s payload=%s",
		convID, who, body)
}

// markVenueCoordination memastikan sebuah meeting offline ditandai venue-coordination
// pada details-nya (idempoten), tanpa mengubah status/jadwal. Menandakan finalisasi
// harus ditahan sampai lokasi pasti dikonfirmasi Bu Nova.
func (h *Handler) markVenueCoordination(ctx context.Context, m *model.MeetingRequest) {
	if h.Store == nil || m == nil {
		return
	}
	var ed meetingDetails
	_ = json.Unmarshal(m.Details, &ed)
	if ed.VenueCoordination {
		return
	}
	ed.VenueCoordination = true
	det, _ := json.Marshal(ed)
	if err := h.Store.UpdateMeetingDetails(ctx, m.ID, det, "su", "tandai meeting offline — menunggu konfirmasi venue"); err != nil {
		log.Printf("[VENUE] tandai venue-coordination #%d gagal: %v", m.ID, err)
	}
}

// confirmVenue menyimpan SATU venue pasti hasil koordinasi support↔Bu Nova ke meeting
// offline yang sedang menunggu lokasi.
// Setelah lokasi tersimpan, mencoba mengajukan paket lengkap (waktu+lokasi) ke SU.
func (h *Handler) confirmVenue(ctx context.Context, contact *model.Contact, a model.Action) {
	if h.Store == nil {
		return
	}
	name := firstNonEmptyStr(strings.TrimSpace(a.VenueName), strings.TrimSpace(a.MeetingVenue))
	addr := strings.TrimSpace(a.VenueAddress)
	if name == "" {
		log.Printf("[VENUE] CONFIRM_VENUE tanpa nama venue — diabaikan")
		return
	}
	var (
		m   *model.MeetingRequest
		err error
	)
	wibDate := ""
	if dt := strings.TrimSpace(a.MeetingDatetime); dt != "" {
		if t, perr := time.Parse(time.RFC3339, dt); perr == nil {
			wibDate = t.In(wibZone).Format("2006-01-02")
		} else {
			log.Printf("[VENUE] CONFIRM_VENUE datetime tak valid (%q): %v — pakai fallback", dt, perr)
		}
	}
	if wibDate != "" {
		m, err = h.Store.FindVenuePendingMeetingByDate(ctx, wibDate)
	} else {
		m, err = h.Store.FindVenuePendingMeeting(ctx)
	}
	if err != nil {
		log.Printf("[VENUE] cari meeting menunggu venue gagal: %v", err)
		return
	}
	if m == nil {
		log.Printf("[VENUE] CONFIRM_VENUE (%q, tanggal=%q) tapi tidak ada meeting offline yang menunggu venue — diabaikan", name, wibDate)
		return
	}
	var ed meetingDetails
	_ = json.Unmarshal(m.Details, &ed)
	ed.VenueCoordination = true
	ed.VenueConfirmed = true
	ed.VenueName = name
	ed.VenueAddress = addr
	venueFull := name
	if addr != "" {
		venueFull = name + " — " + addr
	}
	det, _ := json.Marshal(ed)
	// Simpan lokasi pasti ke kolom venue (dipakai event kalender + email RSVP).
	if err := h.Store.UpdateMeetingPlan(ctx, m.ID, "", "onsite", venueFull, nil, "support", "venue dikonfirmasi Bu Nova"); err != nil {
		log.Printf("[VENUE] simpan venue meeting #%d gagal: %v", m.ID, err)
		return
	}
	if err := h.Store.UpdateMeetingDetails(ctx, m.ID, det, "support", "venueConfirmed + lokasi pasti tersimpan"); err != nil {
		log.Printf("[VENUE] simpan detail venue meeting #%d gagal: %v", m.ID, err)
	}
	log.Printf("[VENUE] meeting #%d venue dikonfirmasi: %q (timeAgreed=%v)", m.ID, venueFull, ed.TimeAgreed)
	h.tryPresentVenuePackage(ctx, m.ID)
}

// deferMeetingForVenue dipanggil saat pihak eksternal menyepakati WAKTU untuk meeting
// offline yang lokasinya masih dikoordinasikan. Mencatat waktu yang disepakati ke meeting
// (TimeAgreed) TANPA mengajukan ke SU, memberi tahu pihak eksternal bahwa lokasi menyusul,
// lalu mencoba mengajukan paket lengkap bila venue ternyata sudah dikonfirmasi lebih dulu.
// buildVenueTimeReinforcement menyuntik penegasan ke PA Communicator bila percakapan ini
// punya meeting offline yang masih menunggu kesepakatan WAKTU. Tanpa ini, saat pihak
// eksternal menyetujui waktu, agent kadang hanya membalas biasa (requiresApproval=false,
// tanpa objek meeting) sehingga gateway tak pernah menandai timeAgreed dan SU tak pernah
// diberi tahu (akar kasus meeting #10). Penegasan ini mewajibkan sinyal terstruktur saat
// waktu disepakati sehingga deferMeetingForVenue → tryPresentVenuePackage terpicu.
func (h *Handler) buildVenueTimeReinforcement(ctx context.Context, convID string) string {
	if h.Store == nil {
		return ""
	}
	m, err := h.Store.ActiveVenueMeetingAwaitingTime(ctx, convID)
	if err != nil || m == nil {
		return ""
	}
	when := ""
	if m.ProposedDatetime != nil {
		when = " (usulan waktu: " + formatWIBLong(*m.ProposedDatetime) + ")"
	}
	var b strings.Builder
	b.WriteString("[MEETING MENUNGGU KESEPAKATAN WAKTU — PENTING]\n")
	b.WriteString("Ada pertemuan offline yang sedang dikoordinasikan dengan kontak ini")
	b.WriteString(when + ". Lokasi diurus terpisah oleh tim internal.\n")
	b.WriteString("WAJIB: Bila pada pesan ini kontak MENYETUJUI/menyepakati waktu pertemuan ")
	b.WriteString("(mis. \"ya bersedia\", \"boleh\", \"setuju\", \"oke, saya ikut\"), JANGAN hanya membalas biasa. ")
	b.WriteString("Kembalikan requiresApproval=true DAN sertakan objek meeting berisi datetime ")
	b.WriteString("(RFC3339 dengan offset +07:00) yang disepakati, plus attendeeName bila diketahui. ")
	b.WriteString("Ini menandai waktu telah disepakati agar paket lengkap (waktu + lokasi) bisa diajukan ke Pak Sudianto.")
	return b.String()
}

func (h *Handler) deferMeetingForVenue(ctx context.Context, existing *model.MeetingRequest, ed meetingDetails, reply *openclaw.AgentReply, contact *model.Contact, agentID, externalChat string) {
	var proposed *time.Time
	if m := reply.Meeting; m != nil {
		if m.Datetime != "" {
			if t, perr := time.Parse(time.RFC3339, m.Datetime); perr == nil {
				proposed = &t
			} else {
				log.Printf("[VENUE] datetime agent tak valid (%q): %v", m.Datetime, perr)
			}
		}
		ed.AttendeeName = firstNonEmptyStr(m.AttendeeName, ed.AttendeeName, existing.ExternalName)
		ed.AttendeeEmail = firstNonEmptyStr(m.AttendeeEmail, ed.AttendeeEmail)
		ed.Title = firstNonEmptyStr(m.Title, ed.Title, existing.Topic)
		if m.DurationMinutes > 0 {
			ed.DurationMinutes = m.DurationMinutes
		}
	}
	ed.TimeAgreed = true
	det, _ := json.Marshal(ed)
	if err := h.Store.UpdateMeetingPlan(ctx, existing.ID, ed.Title, "onsite", "", proposed, agentID, "waktu disepakati — menunggu konfirmasi venue"); err != nil {
		log.Printf("[VENUE] simpan waktu meeting #%d gagal: %v", existing.ID, err)
	}
	if err := h.Store.UpdateMeetingDetails(ctx, existing.ID, det, agentID, "tandai timeAgreed (lokasi masih dikoordinasikan)"); err != nil {
		log.Printf("[VENUE] simpan detail timeAgreed meeting #%d gagal: %v", existing.ID, err)
	}
	log.Printf("[VENUE] meeting #%d waktu disepakati (%v) — ditahan menunggu venue", existing.ID, proposed)

	// Beri tahu pihak eksternal: waktu dicatat, lokasi menyusul. Konfirmasi final dengan
	// lokasi PASTI dikirim setelah SU menyetujui paket lengkap (bukan sekarang).
	if externalChat != "" && contact != nil && contact.TrustLevel != "su" {
		when := ""
		if proposed != nil {
			when = " untuk " + formatWIBLong(*proposed)
		}
		msg := "Baik, waktu pertemuan" + when + " kami catat. Lokasi pastinya sedang kami " +
			"finalkan dan akan segera kami konfirmasikan kembali kepada Anda. Terima kasih. 🙏"
		h.sendAndRecord(ctx, func() error { return h.Waha.SendToChat(externalChat, msg) },
			model.OutboundMessage{
				ConversationID: existing.ConversationID, ContactID: existing.ContactID, AgentID: agentID,
				TargetChat: externalChat, Kind: "interim_ack", Text: msg,
			})
	}

	// Reschedule meeting offline: waktu baru sudah disepakati eksternal → tugaskan Bu Nova
	// mengkoordinasikan ULANG venue untuk waktu baru (boleh pindah lokasi bila bentrok).
	// Paket lengkap (waktu baru + venue) diajukan ke SU setelah Nova konfirmasi via
	// CONFIRM_VENUE. Goroutine sendiri karena ada inject LLM.
	if ed.ReschedulePending {
		go h.dispatchVenueRecoordination(existing.ID, ed, proposed)
	}

	// Bila venue ternyata sudah dikonfirmasi lebih dulu, paket bisa langsung diajukan.
	h.tryPresentVenuePackage(ctx, existing.ID)
}

// tryPresentVenuePackage mengajukan paket lengkap (waktu + lokasi pasti) ke SU untuk SATU
// persetujuan, HANYA bila waktu sudah disepakati DAN venue sudah dikonfirmasi. Idempoten:
// tidak mengajukan ulang bila meeting sudah tertaut approval. Pesan tertahan = konfirmasi
// final ke pihak eksternal (memuat lokasi pasti), dikirim saat SU menyetujui.
func (h *Handler) tryPresentVenuePackage(ctx context.Context, meetingID int64) {
	if h.Store == nil {
		return
	}
	m, err := h.Store.MeetingByID(ctx, meetingID)
	if err != nil || m == nil {
		log.Printf("[VENUE] ambil meeting #%d gagal: %v", meetingID, err)
		return
	}
	var ed meetingDetails
	_ = json.Unmarshal(m.Details, &ed)
	if !ed.TimeAgreed || !ed.VenueConfirmed {
		log.Printf("[VENUE] meeting #%d paket belum lengkap (timeAgreed=%v venueConfirmed=%v) — ditahan", m.ID, ed.TimeAgreed, ed.VenueConfirmed)
		return
	}
	if m.ApprovalID != nil {
		log.Printf("[VENUE] meeting #%d sudah tertaut approval #%d — tidak diajukan ulang", m.ID, *m.ApprovalID)
		return
	}
	if m.ProposedDatetime == nil {
		log.Printf("[VENUE] meeting #%d tanpa waktu — tidak bisa diajukan", m.ID)
		return
	}
	externalChat := h.externalChatIDForMeeting(ctx, m)
	if externalChat == "" {
		log.Printf("[VENUE] meeting #%d tidak ada chat eksternal — tidak bisa diajukan", m.ID)
		return
	}
	who := firstNonEmptyStr(ed.AttendeeName, m.ExternalName, "")
	title := firstNonEmptyStr(ed.Title, m.Topic, "pertemuan")
	greet := "Halo"
	if who != "" {
		greet = "Halo " + who
	}
	// Konfirmasi final ke pihak eksternal — memuat WAKTU & LOKASI pasti (tanpa biaya).
	extMsg := fmt.Sprintf("%s, menyusul koordinasi sebelumnya — pertemuan dengan Pak Sudianto sudah "+
		"dikonfirmasi:\n🗓️ %s\n📍 %s\nTopik: %s.\nSampai jumpa di sana, terima kasih. 🙏",
		greet, formatWIBLong(*m.ProposedDatetime), m.Venue, title)

	agentID := firstNonEmptyStr(m.AgentID, "pa_communicator")
	facts, _ := json.Marshal([]string{})
	apID, err := h.Store.CreateApproval(ctx, model.Approval{
		ConversationID: m.ConversationID, AgentID: agentID, ContactID: m.ContactID,
		TargetChat: externalChat, UserText: "[paket meeting offline: waktu + lokasi]",
		ResponseText: extMsg, ApprovalReason: "Konfirmasi meeting offline (waktu + lokasi pasti)",
		NewFacts: facts,
	})
	if err != nil {
		log.Printf("[VENUE] buat approval paket meeting #%d gagal: %v", m.ID, err)
		return
	}
	ap := apID
	h.sendAndRecord(ctx, nil, model.OutboundMessage{
		ConversationID: m.ConversationID, ContactID: m.ContactID, AgentID: agentID,
		TargetChat: externalChat, Kind: "agent_reply", Text: extMsg, Status: "held", ApprovalID: &ap,
	})
	if err := h.Store.LinkMeetingApproval(ctx, m.ID, apID, "su", "paket lengkap (waktu+venue) diajukan ke SU"); err != nil {
		log.Printf("[VENUE] tautkan meeting #%d ke approval #%d gagal: %v", m.ID, apID, err)
	}
	log.Printf("[VENUE] meeting #%d paket lengkap diajukan ke SU (approval #%d)", m.ID, apID)
	h.notifySUVenuePackage(ctx, m, ed, apID)
}

// notifySUVenuePackage mengirim ringkasan paket lengkap (siapa + waktu + lokasi pasti +
// topik) ke SU untuk satu persetujuan. TIDAK menampilkan biaya/estimasi harga (kebijakan).
func (h *Handler) notifySUVenuePackage(ctx context.Context, m *model.MeetingRequest, ed meetingDetails, apID int64) {
	if h.SUPhone == "" {
		return
	}
	who := firstNonEmptyStr(ed.AttendeeName, m.ExternalName, "Pihak eksternal")
	if c := strings.TrimSpace(m.ExternalCompany); c != "" {
		who += " (" + c + ")"
	}
	title := firstNonEmptyStr(ed.Title, m.Topic, "(tanpa topik)")
	when := "(waktu belum pasti)"
	if m.ProposedDatetime != nil {
		when = formatWIBLong(*m.ProposedDatetime)
	}
	// Reschedule meeting offline: tampilkan transisi jadwal lama → baru dan tegaskan
	// venue sudah dikoordinasikan ulang (bisa berubah lokasi).
	header := "🔔 *Konfirmasi meeting offline diperlukan*"
	venueNote := "Lokasi sudah dikonfirmasi."
	if rf := strings.TrimSpace(ed.RescheduleFrom); rf != "" {
		header = "🔔 *Konfirmasi reschedule meeting offline diperlukan*"
		venueNote = "Venue sudah dikoordinasikan ulang & dikonfirmasi."
		if oldAt, perr := time.Parse(time.RFC3339, rf); perr == nil {
			when = formatWIBLong(oldAt) + " → " + when
		}
	}
	body := fmt.Sprintf("%s (#%d)\n"+
		"Dengan: %s\n🗓️ %s\n📍 %s\nTopik: %s\n\n"+
		"%s Balas *SETUJU %d* untuk mengonfirmasi (undangan kalender + "+
		"email + konfirmasi ke pihak eksternal akan dikirim dengan lokasi ini), atau *TOLAK %d* untuk batal.",
		header, apID, who, when, m.Venue, title, venueNote, apID, apID)
	ap := apID
	h.sendAndRecord(ctx, func() error { return h.Waha.SendText(h.SUPhone, body) },
		model.OutboundMessage{
			ConversationID: m.ConversationID, ContactID: m.ContactID, TargetChat: h.SUPhone,
			Kind: "approval_notify", Text: body, ApprovalID: &ap,
		})
}

// findDuplicateMeeting mengembalikan meeting aktif dengan pihak & jadwal yang sama, atau nil.
func (h *Handler) findDuplicateMeeting(ctx context.Context, convID string, contact *model.Contact, reply *openclaw.AgentReply) *model.MeetingRequest {
	if h.Store == nil || reply.Meeting == nil || strings.TrimSpace(reply.Meeting.Datetime) == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, reply.Meeting.Datetime)
	if err != nil {
		return nil
	}
	party := strings.TrimSpace(reply.Meeting.AttendeeName)
	if party == "" && contact != nil {
		party = strings.TrimSpace(contact.Name)
	}
	if party == "" {
		return nil
	}
	dup, err := h.Store.FindActiveMeetingByPartyAt(ctx, party, t, convID)
	if err != nil {
		log.Printf("[DEDUP] cek meeting duplikat gagal conv=%s: %v", convID, err)
		return nil
	}
	return dup
}

// notifyDuplicateMeeting memberi tahu SU bahwa permintaan meeting yang baru saja
// "diusulkan" sebenarnya sudah tercatat — dan mengarahkan ke tindakan yang tepat
// (konfirmasi approval yang sudah ada, atau info bahwa sudah disetujui/terjadwal).
func (h *Handler) notifyDuplicateMeeting(ctx context.Context, dup *model.MeetingRequest) {
	if h.SUPhone == "" {
		return
	}
	who := strings.TrimSpace(dup.ExternalName)
	if who == "" {
		var det meetingDetails
		_ = json.Unmarshal(dup.Details, &det)
		who = firstNonEmptyStr(strings.TrimSpace(det.AttendeeName), "pihak tersebut")
	}
	when := ""
	if dup.ProposedDatetime != nil {
		when = " (" + formatWIBLong(*dup.ProposedDatetime) + ")"
	}
	var msg string
	switch dup.Status {
	case "pending":
		if dup.ApprovalID != nil {
			msg = fmt.Sprintf("Meeting dengan %s%s sudah tercatat dan menunggu konfirmasi Anda. "+
				"Balas *SETUJU %d* untuk mengonfirmasi, atau *TOLAK %d* untuk membatalkan. "+
				"(Saya tidak membuat permintaan baru agar tidak terjadi jadwal ganda.)",
				who, when, *dup.ApprovalID, *dup.ApprovalID)
		} else {
			msg = fmt.Sprintf("Meeting dengan %s%s sudah tercatat dan masih dalam proses. "+
				"Saya tidak membuat permintaan baru agar tidak terjadi jadwal ganda.", who, when)
		}
	case "approved":
		msg = fmt.Sprintf("Meeting dengan %s%s sudah Anda setujui dan sedang dijadwalkan. "+
			"Tidak ada permintaan baru yang dibuat.", who, when)
	case "scheduled":
		msg = fmt.Sprintf("Meeting dengan %s%s sudah terjadwal & terkonfirmasi (event kalender sudah dibuat). "+
			"Tidak ada permintaan baru yang dibuat.", who, when)
	default:
		msg = fmt.Sprintf("Meeting dengan %s%s sudah tercatat. Tidak ada permintaan baru yang dibuat.", who, when)
	}
	if err := h.Waha.SendText(h.SUPhone, msg); err != nil {
		log.Printf("[DEDUP] notifikasi duplikat ke SU gagal: %v", err)
	}
}

// holdForApproval menyimpan pesan keluar yang mengikat ke approval_pending dan
// memberi notifikasi ke Pak Sudianto (Fase 8 Step 8.5).
func (h *Handler) holdForApproval(ctx context.Context, convID, agentID string, contact *model.Contact, from, userText string, reply *openclaw.AgentReply, execID int64) {
	// Lapis A — penjaga anti-duplikat.
	if dup := h.findDuplicateMeeting(ctx, convID, contact, reply); dup != nil {
		log.Printf("[DEDUP] conv=%s: meeting duplikat terdeteksi (cocok #%d status=%s) — approval baru DIBATALKAN", convID, dup.ID, dup.Status)
		h.notifyDuplicateMeeting(ctx, dup)
		return
	}

	// Tahan dulu meeting offline yang masih koordinasi venue; ajukan ke SU setelah venue pasti.
	if reply.Meeting != nil && h.Store != nil {
		if existing, eerr := h.Store.ActiveMeetingByConversation(ctx, convID); eerr == nil && existing != nil {
			var ed meetingDetails
			_ = json.Unmarshal(existing.Details, &ed)
			if ed.VenueCoordination {
				h.deferMeetingForVenue(ctx, existing, ed, reply, contact, agentID, from)
				return
			}
		}
	}

	facts, _ := json.Marshal(reply.NewFacts)
	id, err := h.Store.CreateApproval(ctx, model.Approval{
		ConversationID: convID, AgentID: agentID, ContactID: cidPtr(contact),
		TargetChat: from, UserText: userText, ResponseText: reply.Response,
		ApprovalReason: reply.ApprovalReason, NewFacts: facts,
	})
	if err != nil {
		log.Printf("[APPROVAL] simpan gagal conv=%s: %v", convID, err)
		return
	}
	log.Printf("[APPROVAL] #%d ditahan conv=%s target=%s reason=%q", id, convID, from, reply.ApprovalReason)

	// Catat pesan yang DITAHAN (belum terkirim) ke outbound_messages (status=held).
	apID := id
	h.sendAndRecord(ctx, nil, model.OutboundMessage{
		ExecutionID: execPtr(execID), ConversationID: convID, ContactID: cidPtr(contact),
		AgentID: agentID, TargetChat: from, Kind: "agent_reply", Text: reply.Response,
		Status: "held", ApprovalID: &apID,
	})
	// Buat meeting_request otomatis (Fase 8.5) tertaut approval ini.
	h.createMeetingFromApproval(ctx, convID, agentID, contact, id, reply)

	// Beri tahu pihak eksternal SEGERA bahwa permintaannya sedang menunggu konfirmasi
	// SU (jangan biarkan mereka menunggu dalam sunyi). Pesan final/konfirmasi baru
	// dikirim saat SU menyetujui.
	h.sendInterimAck(ctx, convID, agentID, contact, from, id)

	h.notifySUApproval(ctx, convID, id, contact, reply)
}

// sendInterimAck memberi tahu pihak eksternal bahwa permintaannya menunggu
// konfirmasi SU. Hanya untuk kontak eksternal, bukan untuk SU sendiri.
func (h *Handler) sendInterimAck(ctx context.Context, convID, agentID string, contact *model.Contact, toChat string, apID int64) {
	if contact == nil || contact.TrustLevel == "su" || toChat == "" {
		return
	}
	greet := "Terima kasih"
	if name := strings.TrimSpace(contact.Name); name != "" {
		greet = "Terima kasih, " + name
	}
	msg := greet + ". Permintaan pertemuan Anda sedang kami sampaikan kepada Pak Sudianto " +
		"untuk konfirmasi. Kami akan segera mengabari Anda kembali setelah beliau " +
		"mengonfirmasi jadwalnya. Mohon menunggu sebentar. 🙏"
	ap := apID
	h.sendAndRecord(ctx, func() error { return h.Waha.SendToChat(toChat, msg) },
		model.OutboundMessage{
			ConversationID: convID, ContactID: cidPtr(contact), AgentID: agentID,
			TargetChat: toChat, Kind: "interim_ack", Text: msg, ApprovalID: &ap,
		})
}

// notifySUApproval mengirim ringkasan pesan tertahan + instruksi approve/reject ke SU
// dan mencatatnya ke outbound_messages.
func (h *Handler) notifySUApproval(ctx context.Context, convID string, id int64, contact *model.Contact, reply *openclaw.AgentReply) {
	if h.SUPhone == "" {
		log.Printf("[APPROVAL] #%d: SU phone kosong, tidak bisa notifikasi", id)
		return
	}
	who := "Pihak eksternal"
	if contact != nil && contact.Name != "" {
		who = contact.Name
		if contact.Company != "" {
			who += " (" + contact.Company + ")"
		}
	}
	// Laporan ke SU = ringkasan bersih (siapa + jadwal + topik + media). TIDAK lagi
	// menampilkan draf pesan ke pihak eksternal ("Pesan menunggu") — SU hanya
	// mengonfirmasi meeting yang sudah disepakati pihak eksternal.
	header := "🔔 *Persetujuan diperlukan*"
	var body string
	if reply.Meeting != nil {
		header = "🔔 *Konfirmasi meeting diperlukan*"
		body = formatMeetingReportSU(who, reply)
	} else {
		reason := strings.TrimSpace(reply.ApprovalReason)
		if reason == "" {
			reason = "(tidak disebutkan)"
		}
		body = who + " — perlu persetujuan Anda:\n" + reason + "\n"
	}
	// Untuk RESCHEDULE: sertakan rekomendasi slot kosong kalender Pak Sudianto pada
	// hari yang diusulkan, agar beliau bisa langsung menilai/menawarkan alternatif.
	slotNote := ""
	if reply.Meeting != nil {
		if mr, merr := h.Store.MeetingByApproval(ctx, id); merr == nil && mr != nil && mr.ProposedDatetime != nil {
			var md meetingDetails
			_ = json.Unmarshal(mr.Details, &md)
			if strings.TrimSpace(md.RescheduleFrom) != "" {
				dur := md.DurationMinutes
				if dur <= 0 {
					dur = 60
				}
				slotNote = h.freeSlotSuggestion(ctx, *mr.ProposedDatetime, dur)
			}
		}
	}

	msg := fmt.Sprintf("%s (#%d)\n%s%s\nBalas *SETUJU %d* untuk konfirmasi, atau *TOLAK %d* untuk batal.",
		header, id, body, slotNote, id, id)
	apID := id
	h.sendAndRecord(ctx, func() error { return h.Waha.SendText(h.SUPhone, msg) },
		model.OutboundMessage{
			ConversationID: convID, ContactID: cidPtr(contact), TargetChat: h.SUPhone,
			Kind: "approval_notify", Text: msg, ApprovalID: &apID,
		})
}

// freeSlotSuggestion merangkai daftar slot kosong kalender Pak Sudianto (mailbox PA)
// pada hari `day`, dalam jam kerja 08.00–18.00 WIB, untuk durasi `durationMin`. Dipakai
// membantu keputusan reschedule. "" bila layanan nonaktif/gagal/penuh.
func (h *Handler) freeSlotSuggestion(ctx context.Context, day time.Time, durationMin int) string {
	if !h.Services.Enabled() {
		return ""
	}
	wibDay := day.In(wibZone)
	date := wibDay.Format("2006-01-02")
	evs, err := h.Services.Availability(ctx, date)
	if err != nil {
		log.Printf("[SLOT] ambil ketersediaan %s gagal: %v", date, err)
		return ""
	}
	// Bangun interval sibuk (waktu lokal WIB).
	type rng struct{ start, end time.Time }
	var busy []rng
	for _, e := range evs {
		s := parseGraphLocal(e.Start)
		en := parseGraphLocal(e.End)
		if s.IsZero() || en.IsZero() {
			continue
		}
		busy = append(busy, rng{s, en})
	}
	overlaps := func(s, e time.Time) bool {
		for _, b := range busy {
			if s.Before(b.end) && e.After(b.start) {
				return true
			}
		}
		return false
	}
	dur := time.Duration(durationMin) * time.Minute
	var free []string
	for hour := 8; hour <= 17; hour++ {
		s := time.Date(wibDay.Year(), wibDay.Month(), wibDay.Day(), hour, 0, 0, 0, wibZone)
		e := s.Add(dur)
		endLimit := time.Date(wibDay.Year(), wibDay.Month(), wibDay.Day(), 18, 0, 0, 0, wibZone)
		if e.After(endLimit) {
			break
		}
		if !overlaps(s, e) {
			free = append(free, s.Format("15.04"))
		}
		if len(free) >= 4 {
			break
		}
	}
	if len(free) == 0 {
		return fmt.Sprintf("\n🗓️ Kalender Anda %s tampak padat — tidak ada slot %d menit yang kosong di jam kerja.\n",
			formatWIBDate(wibDay), durationMin)
	}
	return fmt.Sprintf("\n🗓️ Slot kosong Anda pada %s: %s WIB.\n",
		formatWIBDate(wibDay), strings.Join(free, ", "))
}

// parseGraphLocal mem-parse string waktu dari MS Graph calendarView (Asia/Jakarta,
// mis. "2026-06-30T10:00:00.0000000") menjadi time di zona WIB. Zero time bila gagal.
func parseGraphLocal(s string) time.Time {
	s = strings.TrimSpace(s)
	if len(s) >= 19 {
		if t, err := time.ParseInLocation("2006-01-02T15:04:05", s[:19], wibZone); err == nil {
			return t
		}
	}
	return time.Time{}
}

// handleApprovalCommand memproses perintah "SETUJU <id>" / "TOLAK <id>" dari SU
// (jalur WhatsApp), lalu membalas hasilnya ke chat SU.
func (h *Handler) handleApprovalCommand(suChat, verb, idStr string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		_ = h.Waha.SendToChat(suChat, "Format tidak dikenali. Gunakan: SETUJU <id> atau TOLAK <id>.")
		return
	}
	msg, _ := h.DecideApproval(ctx, id, isApprove(verb))
	_ = h.Waha.SendToChat(suChat, msg)
}

// DecideApproval menjalankan keputusan approve/reject atas satu approval dan
// (bila approve) mengirim pesan tertahan + menyimpan memori. Dipakai oleh jalur
// WhatsApp SU maupun endpoint admin. Mengembalikan pesan status ramah-pengguna.
func (h *Handler) DecideApproval(ctx context.Context, id int64, approve bool) (string, error) {
	status := "rejected"
	if approve {
		status = "approved"
	}

	ap, err := h.Store.DecideApproval(ctx, id, status)
	if errors.Is(err, db.ErrApprovalNotFound) {
		return fmt.Sprintf("Approval #%d tidak ditemukan atau sudah diputuskan.", id), err
	}
	if err != nil {
		log.Printf("[APPROVAL] decide gagal #%d: %v", id, err)
		return fmt.Sprintf("Gagal memproses approval #%d.", id), err
	}

	if !approve {
		log.Printf("[APPROVAL] #%d DITOLAK", id)
		if err := h.Store.UpdateMeetingStatusByApproval(ctx, id, "rejected", "su", "ditolak via approval gate"); err != nil {
			log.Printf("[MEETING] update rejected approval #%d gagal: %v", id, err)
		}
		return fmt.Sprintf("❌ Approval #%d ditolak. Pesan tidak dikirim.", id), nil
	}

	// Disetujui
	sendErr := h.Waha.SendToChat(ap.TargetChat, ap.ResponseText)
	// catat pengiriman aktual (sent/failed) ke outbound_messages.
	apID := id
	h.sendAndRecord(ctx, nil, model.OutboundMessage{
		ConversationID: ap.ConversationID, ContactID: ap.ContactID, AgentID: ap.AgentID,
		TargetChat: ap.TargetChat, Kind: "agent_reply", Text: ap.ResponseText,
		Status: statusOf(sendErr), ApprovalID: &apID, ErrorText: errText(sendErr),
	})
	if sendErr != nil {
		log.Printf("[ERROR] kirim pesan approved #%d gagal: %v", id, sendErr)
		return fmt.Sprintf("⚠️ Approval #%d disetujui tapi gagal mengirim pesan: %v", id, sendErr), sendErr
	}
	if h.Memory != nil {
		var facts []string
		_ = json.Unmarshal(ap.NewFacts, &facts)
		contact := &model.Contact{}
		if ap.ContactID != nil {
			contact.ID = *ap.ContactID
		}
		if err := h.Memory.Write(ctx, ap.ConversationID, contact, ap.AgentID, ap.UserText, ap.ResponseText, facts); err != nil {
			log.Printf("[MEMORY] write approved #%d gagal: %v", id, err)
		}
	}
	// Fase 8.5: tandai meeting tertaut sebagai approved (riwayat tercatat).
	if err := h.Store.UpdateMeetingStatusByApproval(ctx, id, "approved", "su", "disetujui via approval gate"); err != nil {
		log.Printf("[MEETING] update approved approval #%d gagal: %v", id, err)
	}
	log.Printf("[APPROVAL] #%d DISETUJUI, pesan terkirim ke %s", id, ap.TargetChat)

	// jadwalkan otomatis (Calendar event + RSVP email) bila ada meeting
	// tertaut dengan datetime valid
	schedMsg := h.scheduleApprovedMeeting(ctx, id)

	return fmt.Sprintf("✅ Approval #%d disetujui. Pesan telah dikirim.%s", id, schedMsg), nil
}

// scheduleApprovedMeeting membuat O365 Calendar event (+Teams link via isOnline)
// lalu mengirim RSVP email ke kontak, dan menandai meeting 'scheduled'. Dipanggil
// setelah approve. Mengembalikan suffix pesan status (kosong bila tidak ada aksi).
func (h *Handler) scheduleApprovedMeeting(ctx context.Context, approvalID int64) string {
	if !h.Services.Enabled() {
		return ""
	}
	m, err := h.Store.MeetingByApproval(ctx, approvalID)
	if err != nil {
		log.Printf("[SCHEDULE] ambil meeting approval #%d gagal: %v", approvalID, err)
		return ""
	}
	if m == nil {
		return "" // tak ada meeting tertaut (approval non-meeting)
	}
	if m.ProposedDatetime == nil {
		log.Printf("[SCHEDULE] meeting #%d tanpa datetime — lewati penjadwalan otomatis", m.ID)
		return "\n⚠️ Meeting belum punya tanggal/jam pasti, jadi belum dijadwalkan otomatis."
	}

	var det meetingDetails
	_ = json.Unmarshal(m.Details, &det)

	// Pengaman venue-coordination: meeting offline TIDAK boleh difinalisasi sebelum
	// lokasi pasti dikonfirmasi (mestinya sudah dijamin alur paket, ini jaring pengaman).
	if det.VenueCoordination && !det.VenueConfirmed {
		log.Printf("[SCHEDULE] meeting #%d venue belum dikonfirmasi — finalisasi ditahan", m.ID)
		return "\n⚠️ Lokasi meeting belum dikonfirmasi — finalisasi ditahan sampai venue pasti tersimpan."
	}

	title := firstNonEmptyStr(det.Title, m.Topic, "Meeting")
	duration := det.DurationMinutes
	if duration <= 0 {
		duration = 60
	}
	isOnline := strings.TrimSpace(m.Venue) == ""
	dt := m.ProposedDatetime.Format(time.RFC3339)

	// Cabang RESCHEDULE: meeting sudah punya event O365 & menyimpan jadwal lama →
	// PATCH event yang ada + kirim email perubahan jadwal (bukan membuat event baru).
	if det.EventID != "" && strings.TrimSpace(det.RescheduleFrom) != "" {
		return h.finalizeReschedule(ctx, m, det, title, duration)
	}

	var attendees []string
	if det.AttendeeEmail != "" {
		attendees = append(attendees, det.AttendeeEmail)
	}

	// 1) Buat Calendar event (online → dapat Teams joinUrl otomatis).
	ev, err := h.Services.CreateEvent(ctx, services.CreateEventReq{
		Title: title, Datetime: dt, DurationMinutes: duration, Venue: m.Venue,
		Attendees: attendees, IsOnline: isOnline,
	})
	if err != nil {
		log.Printf("[SCHEDULE] buat event meeting #%d gagal: %v", m.ID, err)
		return "\n⚠️ Gagal membuat event kalender — silakan jadwalkan manual."
	}
	det.EventID = ev.EventID
	det.CalendarLink = ev.CalendarLink
	det.TeamsLink = ev.OnlineMeetingURL
	log.Printf("[SCHEDULE] meeting #%d event=%s teams=%s", m.ID, ev.EventID, ev.OnlineMeetingURL)

	// 2) Kirim RSVP email ke kontak (bila email diketahui).
	emailNote := ""
	if det.AttendeeEmail != "" {
		if err := h.Services.SendRSVP(ctx, services.RSVPReq{
			To: det.AttendeeEmail, ToName: firstNonEmptyStr(det.AttendeeName, m.ExternalName, det.AttendeeEmail),
			Title: title, Datetime: dt, DurationMinutes: duration, Venue: m.Venue,
			CalendarLink: ev.CalendarLink, TeamsLink: ev.OnlineMeetingURL,
		}); err != nil {
			log.Printf("[SCHEDULE] kirim RSVP meeting #%d gagal: %v", m.ID, err)
			emailNote = " (email undangan gagal terkirim)"
		} else {
			log.Printf("[SCHEDULE] RSVP terkirim ke %s meeting #%d", det.AttendeeEmail, m.ID)
		}
	} else {
		emailNote = " (email kontak tidak diketahui, undangan tidak dikirim)"
	}

	// 3) Tandai meeting scheduled + simpan link.
	newDetails, _ := json.Marshal(det)
	if err := h.Store.ScheduleMeeting(ctx, m.ID, newDetails, "su", "dijadwalkan otomatis via Fase 9"); err != nil {
		log.Printf("[SCHEDULE] tandai scheduled meeting #%d gagal: %v", m.ID, err)
	}
	// Pengingat otomatis beberapa menit sebelum meeting mulai.
	h.scheduleMeetingReminder(ctx, m, det)
	return fmt.Sprintf("\n📅 Meeting dijadwalkan (event kalender dibuat)%s.", emailNote)
}

// ResendMeetingRSVP mengirim ulang undangan RSVP + .ics untuk meeting yang sudah dijadwalkan,
// memakai detail tersimpan tanpa membuat event kalender baru. Mengembalikan error jika email
// tidak diketahui, layanan nonaktif, atau pengiriman gagal.
func (h *Handler) ResendMeetingRSVP(ctx context.Context, meetingID int64) error {
	if h.Services == nil || !h.Services.Enabled() {
		return fmt.Errorf("layanan email/kalender nonaktif")
	}
	m, err := h.Store.MeetingByID(ctx, meetingID)
	if err != nil {
		return fmt.Errorf("ambil meeting #%d gagal: %w", meetingID, err)
	}
	if m == nil {
		return fmt.Errorf("meeting #%d tidak ditemukan", meetingID)
	}
	if m.ProposedDatetime == nil {
		return fmt.Errorf("meeting #%d tanpa waktu — tak bisa kirim undangan", meetingID)
	}
	var det meetingDetails
	_ = json.Unmarshal(m.Details, &det)
	if strings.TrimSpace(det.AttendeeEmail) == "" {
		return fmt.Errorf("meeting #%d tanpa email kontak — tak bisa kirim undangan", meetingID)
	}
	title := firstNonEmptyStr(det.Title, m.Topic, "Meeting")
	duration := det.DurationMinutes
	if duration <= 0 {
		duration = 60
	}
	if err := h.Services.SendRSVP(ctx, services.RSVPReq{
		To: det.AttendeeEmail, ToName: firstNonEmptyStr(det.AttendeeName, m.ExternalName, det.AttendeeEmail),
		Title: title, Datetime: m.ProposedDatetime.Format(time.RFC3339), DurationMinutes: duration,
		Venue: m.Venue, CalendarLink: det.CalendarLink, TeamsLink: det.TeamsLink,
	}); err != nil {
		log.Printf("[RSVP-RESEND] meeting #%d ke %s gagal: %v", meetingID, det.AttendeeEmail, err)
		return fmt.Errorf("kirim undangan gagal: %w", err)
	}
	log.Printf("[RSVP-RESEND] undangan meeting #%d terkirim ulang ke %s", meetingID, det.AttendeeEmail)
	return nil
}

// finalizeReschedule menyelesaikan reschedule yang sudah disetujui SU: PATCH event O365
// yang ADA ke jadwal baru (link Teams tetap), kirim email "jadwal lama → baru", lalu
// tandai meeting 'scheduled' kembali dengan link terbaru. Pesan konfirmasi WA ke pihak
// eksternal sudah dikirim DecideApproval (pesan tertahan). Mengembalikan suffix status.
func (h *Handler) finalizeReschedule(ctx context.Context, m *model.MeetingRequest, det meetingDetails, title string, duration int) string {
	dtNew := m.ProposedDatetime.Format(time.RFC3339)
	venue := m.Venue

	ev, err := h.Services.RescheduleEvent(ctx, services.UpdateEventReq{
		EventID: det.EventID, Title: title, Datetime: dtNew, DurationMinutes: duration, Venue: venue,
	})
	if err != nil {
		log.Printf("[RESCHEDULE] PATCH event meeting #%d gagal: %v", m.ID, err)
		return "\n⚠️ Gagal memperbarui event kalender — silakan periksa manual."
	}
	det.CalendarLink = ev.CalendarLink
	if ev.OnlineMeetingURL != "" {
		det.TeamsLink = ev.OnlineMeetingURL
	}
	log.Printf("[RESCHEDULE] meeting #%d event %s diperbarui ke %s", m.ID, det.EventID, dtNew)

	emailNote := ""
	if det.AttendeeEmail != "" {
		if serr := h.Services.SendReschedule(ctx, services.RescheduleReq{
			To: det.AttendeeEmail, ToName: firstNonEmptyStr(det.AttendeeName, m.ExternalName, det.AttendeeEmail),
			Title: title, OldDatetime: det.RescheduleFrom, NewDatetime: dtNew,
			DurationMinutes: duration, Venue: venue, CalendarLink: det.CalendarLink, TeamsLink: det.TeamsLink,
		}); serr != nil {
			log.Printf("[RESCHEDULE] email reschedule meeting #%d gagal: %v", m.ID, serr)
			emailNote = " (email perubahan jadwal gagal terkirim)"
		} else {
			log.Printf("[RESCHEDULE] email reschedule terkirim ke %s meeting #%d", det.AttendeeEmail, m.ID)
		}
	} else {
		emailNote = " (email kontak tidak diketahui)"
	}

	det.RescheduleFrom = "" // reschedule selesai
	det.ReschedulePending = false
	newDetails, _ := json.Marshal(det)
	if err := h.Store.ScheduleMeeting(ctx, m.ID, newDetails, "su", "reschedule disetujui — event kalender diperbarui"); err != nil {
		log.Printf("[RESCHEDULE] tandai scheduled meeting #%d gagal: %v", m.ID, err)
	}
	// Perbarui pengingat otomatis ke jadwal baru (batalkan yang lama + buat baru).
	h.scheduleMeetingReminder(ctx, m, det)
	return fmt.Sprintf("\n📅 Jadwal meeting diperbarui (event kalender di-update)%s.", emailNote)
}

// statusOf & errText: helper kecil untuk pencatatan outbound saat kirim manual.
func statusOf(err error) string {
	if err != nil {
		return "failed"
	}
	return "sent"
}

func errText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

// openClawOutput merepresentasikan output OpenClaw untuk jalur push (alternatif).
type openClawOutput struct {
	AgentID        string `json:"agentId"`
	ConversationID string `json:"conversationId"`
	TargetContact  string `json:"targetContact"`
	Response       string `json:"response"`
}

// OpenClawOutput menangani POST /webhook/openclaw-output.
// Jalur internal alternatif: bila ada komponen lain yang mem-push hasil agent,
// endpoint ini mengirimkannya ke WhatsApp. Jalur utama Fase 6 adalah shell-out
// sinkron di WahaInbound. Approval gate diimplementasikan di Fase 8.
func (h *Handler) OpenClawOutput(c *gin.Context) {
	var out openClawOutput
	if err := c.ShouldBindJSON(&out); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payload tidak valid"})
		return
	}
	if out.TargetContact == "" || out.Response == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "targetContact dan response wajib diisi"})
		return
	}

	log.Printf("[OPENCLAW-OUTPUT] agent=%s conv=%s to=%s response=%q",
		out.AgentID, out.ConversationID, out.TargetContact, out.Response)

	if err := h.Waha.SendText(out.TargetContact, out.Response); err != nil {
		log.Printf("[ERROR] openclaw-output gagal kirim ke %s: %v", out.TargetContact, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "gagal kirim ke WhatsApp"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "sent"})
}
