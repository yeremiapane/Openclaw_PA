// Package routes berisi HTTP handler untuk API Gateway.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"pa-ai/api-gateway/src/db"
	"pa-ai/api-gateway/src/google"
	"pa-ai/api-gateway/src/memory"
	"pa-ai/api-gateway/src/middleware"
	"pa-ai/api-gateway/src/model"
	"pa-ai/api-gateway/src/openclaw"
	"pa-ai/api-gateway/src/services"
	"pa-ai/api-gateway/src/waha"
)

// Handler menampung dependency untuk route (WAHA client, store, OpenClaw client, memory).
type Handler struct {
	Waha      *waha.Client
	Store     *db.Store
	OpenClaw  *openclaw.Client
	Memory    *memory.Service
	Services  *services.Client // Calendar + Email (penjadwalan saat approve)
	SUPhone   string           // nomor Pak Sudianto PRIMARY — tujuan notifikasi approval & orchestrator
	NovaPhone string
	// AdminPhone = nomor agent admin PRIMARY (trust=admin). Tujuan push hasil ADMIN_FETCH &
	// jangkar sesi agent admin. Terpisah dari SU. Kosong = agent admin nonaktif.
	AdminPhone string
	// SUPhones/AdminPhones = SEMUA nomor peran (primary + tambahan). Dipakai HANYA untuk
	// cek keanggotaan (mis. proteksi kontak agar admin tak menghapus nomor SU/admin);
	// kirim proaktif tetap ke *Phone primary agar tak ada balapan/dobel.
	SUPhones    []string
	AdminPhones []string
	// ReminderLeadMinutes = berapa menit sebelum meeting mulai pengingat otomatis
	// dikirim ke SU (default 15 bila <= 0).
	ReminderLeadMinutes int

	// ReadDelayMin/Max = rentang jeda ACAK sebelum menandai pesan masuk sudah dibaca
	// (centang biru), agar terlihat manusiawi. Berlaku semua trust. Max<=0 = seketika.
	ReadDelayMin time.Duration
	ReadDelayMax time.Duration

	// PresenceDelayMin/Max = jeda ACAK antara centang biru dan indikator "mengetik…".
	// Max<=0 = seketika.
	PresenceDelayMin time.Duration
	PresenceDelayMax time.Duration

	// LongReplyDelayMin/Max = jeda ACAK sebelum mengirim balasan panjang (>= LongReplyThreshold
	// karakter); indikator "mengetik…" tetap tampil selama jeda. Max<=0 = nonaktif.
	LongReplyDelayMin  time.Duration
	LongReplyDelayMax  time.Duration
	LongReplyThreshold int

	// BurstWindow = jendela debounce penggabungan pesan beruntun + serialisasi per-chat.
	BurstWindow time.Duration
	queueMu     sync.Mutex
	queues      map[string]*chatQueue

	// SpawnStaggerInterval = jarak minimum antar-kontak keluar (SPAWN_AGENT).
	SpawnStaggerInterval time.Duration
	// Gate slot-kirim proses-global untuk SPAWN_AGENT. spawnNextSlot = waktu paling awal
	// kontak berikutnya boleh dikirim; diproteksi spawnGateMu. Lihat reserveSpawnSlot.
	spawnGateMu   sync.Mutex
	spawnNextSlot time.Time

	// PreflightCheckNumber = cek nomor tujuan terdaftar di WhatsApp (via WAHA
	// check-exists) sebelum kontak keluar.
	PreflightCheckNumber bool

	// Google = client People API (opsional). Bila Enabled, nomor BARU disimpan ke
	// kontak Google SU lalu ditunggu GoogleContactSyncDelay agar tersinkron ke HP
	// (primary device WA) sebelum dihubungi — membantu menekan error 463. Fail-open.
	Google                 *google.Client
	GoogleContactSyncDelay time.Duration

	// Alerting : tujuan email notifikasi alert & Bearer token webhook.
	AlertEmailTo      string
	AlertWebhookToken string

	// DocWorkDir = direktori kerja bersama gateway↔agent untuk SEND_DOCUMENT jalur
	// `docPath` (berkas biner: XLSX/PDF/PPTX). Sekaligus batas keamanan: gateway
	// menolak membaca path di luar direktori ini. Kosong = jalur docPath nonaktif.
	DocWorkDir string

	engagedMu sync.Mutex
	engaged   map[string]int

	// AttentionGate = gerbang "perhatian" GLOBAL (kapasitas 1).
	AttentionGate chan struct{}
	
	AttentionMaxWait time.Duration
}

// attentionExempt menandai agent yang TAK PERNAH menunggu gerbang perhatian global.
func attentionExempt(agentID string) bool {
	return agentID == "orchestrator" || agentID == "admin"
}

// attentionMaxWait = batas tunggu giliran di gerbang perhatian (default 8 menit bila
// tak diset).
func (h *Handler) attentionMaxWait() time.Duration {
	if h.AttentionMaxWait > 0 {
		return h.AttentionMaxWait
	}
	return 8 * time.Minute
}

// agentForTrust memetakan trust_level kontak ke agent OpenClaw (Fase 8 routing).
//
//	su            -> orchestrator (jalur langsung Pak Sudianto)
//	admin         -> admin        (agent kendali: tarik data & kontrol operasional)
//	semi_trusted  -> support      (koordinasi internal, mis. Bu Nova)
//	lainnya       -> pa_communicator (pihak eksternal)
func agentForTrust(trust string) string {
	switch trust {
	case "su":
		return "orchestrator"
	case "admin":
		return "admin"
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

// WahaInbound menangani POST /webhook/waha dan memproses pesan di background.
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

	// routing multi-agent berdasarkan trust_level kontak.
	agentID := agentForTrust(contact.TrustLevel)

	// conversationId = session key OpenClaw = PK conversations (satu string konsisten).
	// Disertakan agentID agar sesi tiap agent terisolasi per kontak.
	keyID := contact.Phone
	if keyID == "" {
		keyID = id.Value
	}
	convID := "agent:" + agentID + ":" + keyID
	from := ev.Payload.From

	// Approval gate: SETUJU/TOLAK via WhatsApp sebelum routing ke agent.
	if contact.TrustLevel == "su" {
		if m := approvalCmdRe.FindStringSubmatch(strings.TrimSpace(text)); m != nil {
			// Perintah SU: tandai dibaca seketika (SU tak pernah menunggu gerbang) lalu
			// tangani — jalur ini pulang tanpa process(), jadi centang biru dikirim di sini.
			go func(chatID, msgID string) {
				if err := h.Waha.SendSeen(chatID, msgID); err != nil {
					log.Printf("[PRESENCE] sendSeen %s gagal: %v", chatID, err)
				}
			}(from, ev.Payload.ID)
			go h.handleApprovalCommand(from, m[1], m[2])
			c.JSON(http.StatusOK, gin.H{"status": "approval_command"})
			return
		}
	}

	// Konteks pesan yang sedang dibalas (quote). Hanya dipakai orchestrator (SU);
	// agent lain mengabaikannya. Diekstrak di sini selagi event masih tersedia.
	replyTo := ev.ReplyToText()

	// Lampiran media (dokumen/gambar). Diekstrak selagi event tersedia; unduhan
	// & analisa dilakukan di process(). Hanya file dari SU yang diproses (gerbang
	// di process) — file eksternal diabaikan demi keamanan (anti prompt-injection).
	var file *inboundFile
	if ev.HasFile() {
		file = &inboundFile{
			URL:      ev.Payload.Media.URL,
			Mimetype: ev.Payload.Media.Mimetype,
			Filename: ev.MediaFilename(),
		}
	}

	// Susulan (BurstWindow): bila chat ini SEDANG aktif dilayani (giliran berjalan &
	// gerbang perhatian dipegang → engagedNow, di-set sejak awal process SEBELUM jeda-baca),
	// pesan berikutnya LANGSUNG ditandai dibaca di sini — meniru manusia yang sudah membuka
	// & menyimak chat ini. Tanpa ini, susulan hanya akan dibaca saat giliran BERIKUTNYA jalan
	// (bisa ~1 menit setelah agen selesai), karena process() menandai baca sekali per giliran.
	// PENTING utk [[attention-queue-global]]: pesan PERTAMA chat idle & pesan dari chat yang
	// MENUNGGU giliran (belum engaged) TIDAK ditandai di sini — dibiarkan process() menandai
	// setelah gerbang perhatian memberi giliran, agar percakapan lain tetap tampak belum dibaca.
	if h.BurstWindow > 0 && h.engagedNow(from) {
		go func(chatID, msgID string) {
			if err := h.Waha.SendSeen(chatID, msgID); err != nil {
				log.Printf("[PRESENCE] sendSeen (susulan) %s gagal: %v", chatID, err)
			}
		}(from, ev.Payload.ID)
	}

	// Proses inject + kirim balasan di luar request lifecycle WAHA.
	if h.BurstWindow > 0 {
		h.enqueueTurn(contact, agentID, convID, from, text, replyTo, file, ev.Payload.ID)
	} else {
		go h.process(contact, agentID, convID, from, text, replyTo, file, ev.Payload.ID, "")
	}

	c.JSON(http.StatusOK, gin.H{"status": "received"})
}

func (h *Handler) process(contact *model.Contact, agentID, convID, from, text, replyTo string, file *inboundFile, lastMsgID, quoteID string) {
	if h.AttentionGate != nil && !attentionExempt(agentID) {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), h.attentionMaxWait())
		select {
		case h.AttentionGate <- struct{}{}:
			waitCancel()
			defer func() { <-h.AttentionGate }()
			log.Printf("[ATTN] conv=%s mulai dilayani (gerbang perhatian dipegang)", convID)
		case <-waitCtx.Done():
			waitCancel()
			log.Printf("[ATTN] conv=%s menyerah menunggu giliran (> %v) — pesan dilewati", convID, h.attentionMaxWait())
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// Presence: tandai pesan masuk sudah dibaca (centang biru) SETELAH jeda acak
	// (READ_DELAY_MIN..MAX, semua trust)
	skipReadDelay := h.engagedNow(from)
	// Tandai chat "sedang dilayani" SEKARANG — tepat setelah gerbang perhatian dipegang
	// (atau agen exempt), SEBELUM jeda-baca. Krusial utk read susulan: bila engaged baru
	// di-set setelah readDone (jeda 1–30s), pesan susulan yang tiba selama jeda-baca tak
	// tertandai dibaca sampai giliran berikutnya (~semenit). Chat yang masih MENUNGGU
	// gerbang belum sampai sini → tetap engaged=0 → antrean perhatian tetap utuh.
	h.markEngaged(from)
	defer h.unmarkEngaged(from)
	readDone := make(chan struct{})
	go func(chatID, msgID string, skip bool) {
		defer close(readDone)
		if !skip {
			if d := randDelay(h.ReadDelayMin, h.ReadDelayMax); d > 0 {
				select {
				case <-time.After(d):
				case <-ctx.Done():
					return
				}
			}
		}
		if err := h.Waha.SendSeen(chatID, msgID); err != nil {
			log.Printf("[PRESENCE] sendSeen %s gagal: %v", chatID, err)
		}
	}(from, lastMsgID, skipReadDelay)

	// Indikator "mengetik…" baru muncul SETELAH pesan ditandai dibaca (readDone),
	// agar urutannya natural: centang biru dulu, baru "mengetik…", baru balasan.
	typingCtx, stopTyping := context.WithCancel(ctx)
	go h.typingKeepAlive(typingCtx, from, readDone)
	defer stopTyping()

	// Lampiran file: unduh, simpan ke direktori kerja, dan ubah `text` menjadi
	// instruksi baca-berkas untuk orchestrator. HANYA untuk SU (keamanan: dokumen
	// eksternal bisa memuat prompt-injection dan tak boleh diumpankan otomatis).
	if file != nil {
		if contact.TrustLevel == "su" {
			text = h.ingestInboundFile(ctx, convID, text, file)
		} else {
			log.Printf("[FILE] lampiran dari non-SU (trust=%s) diabaikan", contact.TrustLevel)
		}
	}

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
				if snap := h.buildCalendarSnapshot(ctx); snap != "" {
					mc.LiveStatus += "\n\n" + snap
				}
				if snap := h.buildReminderSnapshot(ctx); snap != "" {
					mc.LiveStatus += "\n\n" + snap
				}
				if snap := h.buildWatchSnapshot(ctx); snap != "" {
					mc.LiveStatus += "\n\n" + snap
				}
				if snap := h.buildPersonaSnapshot(ctx); snap != "" {
					mc.LiveStatus += "\n\n" + snap
				}
				if snap := h.buildDocWorkDirSnapshot(); snap != "" {
					mc.LiveStatus += "\n\n" + snap
				}
				if replyTo != "" {
					mc.LiveStatus += "\n\n[PESAN YANG SEDANG DIBALAS PAK SUDIANTO]\n" +
						"Beliau menanggapi pesan ini: \"" + replyTo + "\"\n" +
						"Pakai kutipan ini sebagai acuan konteks balasan beliau."
				}
			} else if agentID == "admin" {
				// Agent admin: suntik status operasional menyeluruh (READ) tiap giliran.
				if snap := h.buildAdminSnapshot(ctx); snap != "" {
					mc.LiveStatus += "\n\n" + snap
				}
				if replyTo != "" {
					mc.LiveStatus += "\n\n[PESAN YANG SEDANG DIBALAS ADMIN]\n" +
						"Admin menanggapi pesan ini: \"" + replyTo + "\"\n" +
						"Pakai kutipan ini sebagai acuan konteks balasan."
				}
			} else if agentID == "pa_communicator" {
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

	// terapkan actions terstruktur (UPDATE_STATE, NOTIFY_ORCHESTRATOR).
	// `text` diteruskan agar SPAWN_AGENT bisa koreksi nomor tujuan bila LLM salah ketik.
	h.applyActions(ctx, convID, contact, reply.Actions, execID, text)

	// Simpan profil yang BARU dipelajari (email/nama) ke tabel contacts agar diingat
	// lintas-percakapan — sehingga bot tak menanyakan ulang data yang sudah diberikan.
	h.persistContactProfile(ctx, convID, contact, reply)

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
	// Pastikan centang biru sudah terkirim sebelum balasan keluar — agar balasan tak
	// pernah mendahului tanda "dibaca" saat agent merespons lebih cepat dari jeda baca.
	select {
	case <-readDone:
	case <-ctx.Done():
	}
	// Jeda "mengetik" manusiawi untuk balasan PANJANG: bila balasan >= ambang karakter,
	// tahan sesaat (acak) sambil indikator "mengetik…" tetap tampil, meniru waktu yang
	// dibutuhkan manusia mengetik pesan panjang. Balasan pendek dikirim tanpa jeda.
	if h.LongReplyThreshold >= 0 && len([]rune(reply.Response)) >= h.LongReplyThreshold {
		if d := randDelay(h.LongReplyDelayMin, h.LongReplyDelayMax); d > 0 {
			log.Printf("[PRESENCE] balasan panjang (%d char) — jeda mengetik %v sebelum kirim (chat=%s)",
				len([]rune(reply.Response)), d, from)
			select {
			case <-time.After(d):
			case <-ctx.Done():
			}
		}
	}
	stopTyping() // hentikan indikator "mengetik…" tepat sebelum balasan terkirim
	h.sendAndRecord(ctx, func() error { return h.Waha.SendToChatQuoted(from, reply.Response, quoteID) },
		model.OutboundMessage{
			ExecutionID: execPtr(execID), ConversationID: convID, ContactID: cidPtr(contact),
			AgentID: agentID, TargetChat: from, Kind: "agent_reply", Text: reply.Response,
		})
}

// typingKeepAlive menampilkan indikator "sedang mengetik…", menyegarkannya
// tiap ~8 dtk sampai ctx dibatalkan, lalu menghentikannya.
func (h *Handler) typingKeepAlive(ctx context.Context, chatID string, readDone <-chan struct{}) {
	// Tunggu pesan ditandai dibaca (centang biru) dulu; baru tampilkan "mengetik…".
	select {
	case <-readDone:
	case <-ctx.Done():
		return
	}
	// Catatan: penanda "sedang dilayani" (markEngaged) kini di-set di process() sejak
	// gerbang perhatian dipegang, SEBELUM jeda-baca — agar pesan susulan yang tiba selama
	// jeda-baca tetap langsung ditandai dibaca. Tak lagi di-set di sini.
	// Jeda manusiawi antara "centang biru" dan indikator "mengetik…": manusia butuh
	// sesaat untuk beralih dari membaca ke mengetik. Dibatalkan bila ctx selesai.
	if d := randDelay(h.PresenceDelayMin, h.PresenceDelayMax); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return
		}
	}
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

// markEngaged menaikkan penghitung "sedang dilayani" untuk chat (dipanggil saat bot
// mulai mengetik). Penghitung (bukan bool) agar aman bila beberapa balasan untuk chat
// yang sama tumpang-tindih.
func (h *Handler) markEngaged(chatID string) {
	h.engagedMu.Lock()
	defer h.engagedMu.Unlock()
	if h.engaged == nil {
		h.engaged = make(map[string]int)
	}
	h.engaged[chatID]++
}

// unmarkEngaged menurunkan penghitung; entri dihapus saat mencapai 0 (chat kembali idle).
func (h *Handler) unmarkEngaged(chatID string) {
	h.engagedMu.Lock()
	defer h.engagedMu.Unlock()
	if h.engaged[chatID] > 0 {
		h.engaged[chatID]--
	}
	if h.engaged[chatID] <= 0 {
		delete(h.engaged, chatID)
	}
}

// engagedNow melaporkan apakah bot sedang melayani (mengetik) untuk chat ini — dipakai
// untuk memutuskan apakah pesan masuk perlu jeda-baca acak (idle) atau langsung dibaca
// (susulan saat sedang dilayani).
func (h *Handler) engagedNow(chatID string) bool {
	h.engagedMu.Lock()
	defer h.engagedMu.Unlock()
	return h.engaged[chatID] > 0
}

// chatQueue = antrean serial per-chat. Menahan pesan yang menunggu digabung menjadi
// giliran BERIKUTNYA: pesan yang datang berdekatan (debounce BurstWindow) atau selagi
// giliran sekarang masih berjalan (`active`). Semua field diproteksi Handler.queueMu.
type chatQueue struct {
	contact   *model.Contact
	agentID   string
	convID    string
	from      string
	texts     []string     // teks pesan tertunda, urut kedatangan
	replyTo   string       // konteks quote pesan TERAKHIR (bila user membalas)
	lastMsgID string       // ID pesan terakhir — target centang biru & kutipan balasan
	file      *inboundFile // lampiran terakhir (bila ada) — giliran diproses sbg file
	active    bool         // sebuah giliran sedang diproses untuk chat ini
	timer     *time.Timer  // debounce sebelum mulai; hanya aktif saat !active
}

// enqueueTurn menumpuk satu pesan ke antrean chat. Bila belum ada giliran berjalan,
// (ulang) setel timer debounce; saat habis, startTurnLocked menggabung & memproses.
// Bila giliran sedang berjalan, pesan cukup ditumpuk — finishTurn akan mengambilnya
// begitu giliran sekarang selesai (mencegah proses paralel → balasan ganda).
func (h *Handler) enqueueTurn(contact *model.Contact, agentID, convID, from, text, replyTo string, file *inboundFile, msgID string) {
	h.queueMu.Lock()
	defer h.queueMu.Unlock()
	if h.queues == nil {
		h.queues = make(map[string]*chatQueue)
	}
	q := h.queues[from]
	if q == nil {
		q = &chatQueue{contact: contact, agentID: agentID, convID: convID, from: from}
		h.queues[from] = q
	}
	// Selalu segarkan konteks ke pesan terbaru.
	q.contact = contact
	q.agentID = agentID
	q.convID = convID
	q.texts = append(q.texts, text)
	q.replyTo = replyTo
	q.lastMsgID = msgID
	if file != nil {
		q.file = file
	}
	// Debounce hanya bermakna saat tak ada giliran berjalan; selagi giliran berjalan,
	// pesan menunggu finishTurn tanpa timer.
	if q.active {
		if q.timer != nil {
			q.timer.Stop()
			q.timer = nil
		}
		return
	}
	if q.timer != nil {
		q.timer.Reset(h.BurstWindow)
	} else {
		q.timer = time.AfterFunc(h.BurstWindow, func() { h.startTurn(from) })
	}
}

// startTurn dipanggil saat timer debounce habis: mulai satu giliran bila belum ada yang
// berjalan dan masih ada pesan tertunda.
func (h *Handler) startTurn(from string) {
	h.queueMu.Lock()
	defer h.queueMu.Unlock()
	h.startTurnLocked(from)
}

// startTurnLocked (dipanggil dengan queueMu terkunci) menggabung pesan tertunda menjadi
// satu giliran dan menandai chat `active`. Bila tak ada pesan tertunda / giliran sudah
// berjalan, no-op. Setelah process selesai, finishTurn dipanggil untuk mengambil pesan
// yang menumpuk selama pemrosesan (bila ada).
func (h *Handler) startTurnLocked(from string) {
	q := h.queues[from]
	if q == nil || q.active || len(q.texts) == 0 {
		return
	}
	if q.timer != nil {
		q.timer.Stop()
		q.timer = nil
	}
	lastMsgID := q.lastMsgID
	combined, quoteID := burstCombine(q.texts, lastMsgID)
	contact, agentID, convID := q.contact, q.agentID, q.convID
	replyTo, file := q.replyTo, q.file
	// Konsumsi buffer: giliran ini mengambil semua yang tertunda.
	q.texts = nil
	q.replyTo = ""
	q.lastMsgID = ""
	q.file = nil
	q.active = true
	go func() {
		h.process(contact, agentID, convID, from, combined, replyTo, file, lastMsgID, quoteID)
		h.finishTurn(from)
	}()
}

// finishTurn menandai giliran chat selesai. Bila pesan menumpuk selama pemrosesan,
// (ulang) setel debounce singkat untuk menggabung sisa burst lalu memulai giliran
// berikutnya; bila tak ada, buang entri antrean agar map tak menggelembung.
func (h *Handler) finishTurn(from string) {
	h.queueMu.Lock()
	defer h.queueMu.Unlock()
	q := h.queues[from]
	if q == nil {
		return
	}
	q.active = false
	if len(q.texts) == 0 {
		if q.timer != nil {
			q.timer.Stop()
		}
		delete(h.queues, from)
		return
	}
	// Ada pesan tertunda: debounce singkat agar burst yang masih berdatangan ikut
	// tergabung, lalu mulai giliran berikutnya.
	if q.timer != nil {
		q.timer.Reset(h.BurstWindow)
	} else {
		q.timer = time.AfterFunc(h.BurstWindow, func() { h.startTurn(from) })
	}
}

// burstCombine menggabung teks-teks burst menjadi satu giliran (dipisah baris) dan
// menentukan ID pesan yang dikutip: HANYA bila burst berisi >= 2 pesan; pesan tunggal
// dibalas normal tanpa kutipan (agar percakapan biasa tidak dipenuhi kutipan).
func burstCombine(texts []string, lastMsgID string) (combined, quoteID string) {
	combined = strings.Join(texts, "\n")
	if len(texts) >= 2 {
		quoteID = lastMsgID
	}
	return combined, quoteID
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

// Cache singkat snapshot kalender O365 agar tidak memanggil MS Graph pada SETIAP giliran
// SU (menghemat latensi & kuota). Kalender jarang berubah dalam hitungan menit, jadi TTL
// pendek aman; perubahan baru akan tampak setelah TTL kedaluwarsa.
const calSnapTTL = 2 * time.Minute

var (
	calSnapMu      sync.Mutex
	calSnapValue   string
	calSnapFetched time.Time
)

// buildCalendarSnapshot menyusun ringkasan jadwal O365 (pa@hypernet.co.id)
// untuk hari ini + besok. Digunakan bersama [STATUS MEETING] agar orchestrator
// bisa menjawab pertanyaan jadwal ad-hoc. Hasil dicache singkat (calSnapTTL)
// untuk mengurangi panggilan MS Graph. Mengembalikan "" jika layanan nonaktif
// atau pengambilan kedua hari gagal.
func (h *Handler) buildCalendarSnapshot(ctx context.Context) string {
	if !h.Services.Enabled() {
		return ""
	}
	calSnapMu.Lock()
	if !calSnapFetched.IsZero() && time.Since(calSnapFetched) < calSnapTTL {
		v := calSnapValue
		calSnapMu.Unlock()
		return v
	}
	calSnapMu.Unlock()

	now := time.Now().In(wibZone)
	days := []struct {
		label string
		day   time.Time
	}{
		{"Hari ini (" + formatWIBDate(now) + ")", now},
		{"Besok (" + formatWIBDate(now.AddDate(0, 0, 1)) + ")", now.AddDate(0, 0, 1)},
	}
	var sections []string
	for _, d := range days {
		evs, err := h.Services.Availability(ctx, d.day.Format("2006-01-02"))
		if err != nil {
			log.Printf("[SNAPSHOT] kalender %s gagal: %v", d.day.Format("2006-01-02"), err)
			continue
		}
		sections = append(sections, d.label+":\n"+formatCalendarAgenda(evs))
	}

	snap := ""
	if len(sections) > 0 {
		snap = "[KALENDER O365 — jadwal terkonfirmasi di kalender Pak Sudianto (pa@hypernet.co.id), " +
			"data LANGSUNG dari sistem. Ini MELENGKAPI [STATUS MEETING] (pipeline PA): kalender = " +
			"acara terkonfirmasi (termasuk janji pribadi & undangan dari luar); STATUS MEETING = " +
			"meeting yang dikelola PA (termasuk yang masih menunggu/belum masuk kalender). Untuk " +
			"pertanyaan jadwal, gabungkan keduanya; bila sebuah acara muncul di kedua sumber, " +
			"sebutkan SEKALI saja. \"beberapa jam ke depan\" = saring dari waktu sekarang. Waktu WIB.]\n" +
			strings.Join(sections, "\n\n")
	}

	calSnapMu.Lock()
	calSnapValue = snap
	calSnapFetched = time.Now()
	calSnapMu.Unlock()
	return snap
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

// injectWithRecovery: coba normal, retry sekali dengan dorongan JSON, lalu reset sesi jika parse_error.
func (h *Handler) injectWithRecovery(ctx context.Context, agentID, convID, message string) (*openclaw.AgentReply, *openclaw.RunMeta, error) {
	return h.injectWithRecoveryVia(ctx, h.OpenClaw, agentID, convID, message)
}

// injectWithRecoveryVia sama dengan injectWithRecovery tetapi memakai client yang
// diberikan — jalur kerja berat (DEFER_TASK) menyerahkan client ber-timeout panjang.
func (h *Handler) injectWithRecoveryVia(ctx context.Context, cl *openclaw.Client, agentID, convID, message string) (*openclaw.AgentReply, *openclaw.RunMeta, error) {
	sk := convID
	if h.Memory != nil {
		sk = h.Memory.OCSessionKey(ctx, convID)
	}
	reply, meta, err := cl.InjectAgent(ctx, agentID, sk, message)
	if !isParseErr(err) {
		return reply, meta, err
	}

	// (2) retry pada sesi sama dengan dorongan kepatuhan JSON.
	// Bila bootstrap terpotong, itu penyebab paling mungkin: kontrak JSON ada di
	// ekor SOUL.md yang hilang. Cantumkan di log agar korelasinya kelihatan langsung.
	trunc := ""
	if meta != nil && len(meta.TruncatedBootstrap) > 0 {
		trunc = " — DIDUGA bootstrap terpotong: " + strings.Join(meta.TruncatedBootstrap, ", ")
	}
	log.Printf("[RECOVERY] parse_error conv=%s sk=%s — retry dorongan JSON (sesi sama)%s", convID, sk, trunc)
	reply, meta, err = cl.InjectAgent(ctx, agentID, sk, message+jsonNudge)
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
	reply, meta, err = cl.InjectAgent(ctx, agentID, sk, message+jsonNudge)
	if isParseErr(err) {
		log.Printf("[RECOVERY] GAGAL conv=%s — masih parse_error setelah retry+reset", convID)
	} else if err == nil {
		log.Printf("[RECOVERY] PULIH conv=%s pada sesi baru", convID)
	}
	return reply, meta, err
}

// ── Helper observability (token usage, outbound, meeting) ──────────

// cidPtr mengembalikan pointer ID kontak (nil bila tidak ada).
func cidPtr(contact *model.Contact) *int {
	if contact != nil && contact.ID > 0 {
		id := contact.ID
		return &id
	}
	return nil
}

// nextSpawnSlot menghitung slot kirim untuk satu kontak keluar (SPAWN_AGENT) dari
// waktu `now` dan slot terpesan sebelumnya `prevSlot`. Mengembalikan jeda dari now
// sampai slot ini + slot terpesan berikutnya (slot+interval). Menjamin jarak >=
// interval antar kontak. interval<=0 → tanpa jeda (prevSlot tak berubah). Bila slot
// sebelumnya sudah lewat (prevSlot<=now), kirim seketika tanpa menumpuk jeda. Pure
// (tanpa state/clock) agar mudah diuji deterministik.
func nextSpawnSlot(now, prevSlot time.Time, interval time.Duration) (delay time.Duration, newSlot time.Time) {
	if interval <= 0 {
		return 0, prevSlot
	}
	slot := now
	if prevSlot.After(now) {
		slot = prevSlot
	}
	return slot.Sub(now), slot.Add(interval)
}

// reserveSpawnSlot memesan slot kirim berikutnya untuk kontak keluar dan mengembalikan
// jeda dari sekarang sampai slot itu. State proses-global (diproteksi mutex) sehingga
// spacing berlaku LINTAS batch dan lintas giliran/pesan. interval<=0 → 0 (serempak).
func (h *Handler) reserveSpawnSlot(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	h.spawnGateMu.Lock()
	defer h.spawnGateMu.Unlock()
	delay, next := nextSpawnSlot(time.Now(), h.spawnNextSlot, interval)
	h.spawnNextSlot = next
	return delay
}

// randDelay mengembalikan durasi acak di rentang [min, max] (inklusif). Dipakai untuk
// semua jeda "manusiawi" (baca, presence, balasan panjang). Bila max<=0 atau max<min→0
// (tanpa jeda). Bila min==max, kembalikan tepat nilai itu. Memakai rand global
// (auto-seeded, Go 1.20+).
func randDelay(min, max time.Duration) time.Duration {
	if max <= 0 || max < min {
		return 0
	}
	if min < 0 {
		min = 0
	}
	if max == min {
		return min
	}
	return min + time.Duration(rand.Int63n(int64(max-min)+1))
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
	// Metrik Prometheus (Fase M5): rekam giliran agent (outcome/latensi/token/fallback/
	// refusal) — independen dari tulis DB agar tetap tercatat walau DB gagal.
	middleware.RecordAgentCall(agentID, outcome, meta.DurationMs,
		meta.Usage.Input, meta.Usage.Output, meta.Usage.CacheRead, meta.Usage.CacheWrite,
		meta.FallbackUsed, meta.Refusal)

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
	// Metrik Prometheus (Fase M5): hitung pesan keluar per jenis & status.
	middleware.RecordOutbound(o.Kind, o.Status)
	if _, err := h.Store.LogOutbound(ctx, o); err != nil {
		log.Printf("[OBS] log outbound gagal: %v", err)
	}
}

// meetingDetails = isi kolom JSON details meeting_requests. Menyimpan data yang
// tidak punya kolom sendiri + hasil penjadwalan (eventId/link) saat scheduled.
type meetingDetails struct {
	ApprovalReason    string   `json:"approvalReason,omitempty"`
	NewFacts          []string `json:"newFacts,omitempty"`
	Title             string   `json:"title,omitempty"`
	DurationMinutes   int      `json:"durationMinutes,omitempty"`
	AttendeeEmail     string   `json:"attendeeEmail,omitempty"`
	AttendeeName      string   `json:"attendeeName,omitempty"`
	EventID           string   `json:"eventId,omitempty"`
	CalendarLink      string   `json:"calendarLink,omitempty"`
	TeamsLink         string   `json:"teamsLink,omitempty"`
	ReschedulePending bool     `json:"reschedulePending,omitempty"`
	RescheduleFrom    string   `json:"rescheduleFrom,omitempty"`
	VenueCoordination bool     `json:"venueCoordination,omitempty"`
	TimeAgreed        bool     `json:"timeAgreed,omitempty"`
	VenueConfirmed    bool     `json:"venueConfirmed,omitempty"`
	VenueName         string   `json:"venueName,omitempty"`
	VenueAddress      string   `json:"venueAddress,omitempty"`
	TimePresentedAt   string   `json:"timePresentedAt,omitempty"`
	// Meeting grup (Fase 1 konsolidasi): bila SU menginisiasi satu pertemuan dengan
	// >=2 orang dalam satu perintah, seluruh baris meeting per-peserta berbagi GroupID
	// yang sama. GroupSize = jumlah peserta yang diharapkan (jumlah SPAWN_AGENT sebatch).
	// Dipakai untuk: menahan notifikasi SU sampai SEMUA peserta setuju (satu approval),
	// satu koordinasi venue, dan satu laporan gabungan. Kosong = meeting solo (perilaku lama).
	GroupID   string `json:"groupId,omitempty"`
	GroupSize int    `json:"groupSize,omitempty"`
	// GroupApprovalNotified ditandai true (sekali) saat notifikasi persetujuan grup
	// GABUNGAN sudah dikirim ke SU — mencegah notifikasi ganda ketika peserta terakhir
	// menyetujui secara bersamaan (dua percakapan berbeda melintasi ambang serentak).
	GroupApprovalNotified bool `json:"groupApprovalNotified,omitempty"`
	// InitiatedBy = nomor SU yang MENGINISIASI meeting ini (SU multi-nomor).
	InitiatedBy string `json:"initiatedBy,omitempty"`
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

	// Reschedule existing meeting: update baris yang ada, bukan buat baru.
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

	// Rekonsiliasi dengan proposal SPAWN milik SU (mencegah dobel + mewarisi label
	// SU-initiated). Coba cocok nama dulu; bila gagal, fallback ke PERCAKAPAN yang sama —
	// jauh lebih tahan karena nama pihak eksternal kerap berubah saat approval (mis.
	// "Putra HYP" → "Pak Dwi Putra"), sedangkan conversation_id proposal SPAWN dan
	// balasan eksternal selalu sama. Tanpa fallback ini, meeting SU-initiated bisa salah
	// label 'external' → undangan email TIDAK terkirim (KEBIJAKAN eksternal-hosted).
	var spawnProp *model.MeetingRequest
	if det.AttendeeName != "" {
		if prop, ferr := h.Store.FindPendingSpawnMeeting(ctx, det.AttendeeName, ""); ferr != nil {
			log.Printf("[MEETING] cari proposal SPAWN utk %q gagal: %v", det.AttendeeName, ferr)
		} else {
			spawnProp = prop
		}
	}
	if spawnProp == nil {
		if prop, ferr := h.Store.FindPendingSpawnMeetingByConversation(ctx, convID); ferr != nil {
			log.Printf("[MEETING] cari proposal SPAWN by-conv %q gagal: %v", convID, ferr)
		} else if prop != nil {
			spawnProp = prop
			log.Printf("[MEETING] proposal SPAWN #%d dicocokkan via PERCAKAPAN (nama tak cocok: %q≠%q)",
				prop.ID, det.AttendeeName, prop.ExternalName)
		}
	}
	if spawnProp != nil {
		if via == "external" {
			via = "su"
			log.Printf("[MEETING] approval eksternal cocok proposal SPAWN #%d — warisi via=su (SU-initiated)", spawnProp.ID)
		}
		// Wariskan email/nama dari proposal SPAWN bila giliran ini tak membawanya,
		// supaya undangan tak jatuh ke cabang "email tidak diketahui". Juga wariskan
		// keanggotaan grup (Fase 1) agar baris final tetap terkait konsolidasi grup.
		var pd meetingDetails
		if json.Unmarshal(spawnProp.Details, &pd) == nil {
			if strings.TrimSpace(det.AttendeeEmail) == "" {
				det.AttendeeEmail = firstNonEmptyStr(det.AttendeeEmail, pd.AttendeeEmail)
			}
			if strings.TrimSpace(det.AttendeeName) == "" {
				det.AttendeeName = firstNonEmptyStr(det.AttendeeName, pd.AttendeeName)
			}
			if det.GroupID == "" && pd.GroupID != "" {
				det.GroupID = pd.GroupID
				det.GroupSize = pd.GroupSize
			}
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
	log.Printf("[MEETING] #%d dibuat (pending, via=%s) dari approval #%d conv=%s datetime=%v", mid, via, approvalID, convID, proposed)

	// Rekonsiliasi: pensiunkan proposal SPAWN yang sudah ditemukan agar tak dobel.
	if spawnProp != nil && spawnProp.ID != mid {
		if uerr := h.Store.UpdateMeetingStatus(ctx, spawnProp.ID, "superseded", "su",
			fmt.Sprintf("digantikan oleh meeting final #%d (masuk approval gate)", mid)); uerr != nil {
			log.Printf("[MEETING] pensiun proposal SPAWN #%d gagal: %v", spawnProp.ID, uerr)
		} else {
			log.Printf("[MEETING] proposal SPAWN #%d → superseded (digantikan #%d)", spawnProp.ID, mid)
		}
	}
}


var contactEmailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

func firstEmailInFacts(facts []string) string {
	seen := ""
	for _, f := range facts {
		for _, m := range contactEmailRe.FindAllString(f, -1) {
			e := strings.ToLower(strings.TrimSpace(m))
			if e == "" {
				continue
			}
			if seen == "" {
				seen = e
			} else if seen != e {
				return "" // >1 email berbeda → ambigu
			}
		}
	}
	return seen
}

// persistContactProfile menyimpan email/nama baru yang dipelajari dari percakapan ke
// tabel contacts, tanpa menimpa data yang sudah ada. Sumber email: objek meeting bila
// ada; jika tidak, diekstrak dari newFacts (kasus kontak memberi email di turn biasa
// tanpa penjadwalan — dulu email hanya jadi fakta dan TIDAK pernah masuk contacts.email).
func (h *Handler) persistContactProfile(ctx context.Context, convID string, contact *model.Contact, reply *openclaw.AgentReply) {
	if h.Store == nil || contact == nil || contact.Phone == "" {
		return
	}
	var in db.ContactInput
	upd := false
	if contact.Email == "" {
		email := ""
		if reply.Meeting != nil {
			email = strings.TrimSpace(reply.Meeting.AttendeeEmail)
		}
		if email == "" {
			email = firstEmailInFacts(reply.NewFacts)
		}
		if email != "" {
			in.Email = email
			upd = true
		}
	}
	if contact.Name == "" && reply.Meeting != nil {
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

	// Bila email BARU saja diketahui, tutup celah "email datang setelah finalisasi":
	// meeting SU-initiated bisa sudah scheduled tanpa undangan terkirim (AttendeeEmail
	// kosong saat venue dikonfirmasi). Kirim undangan sekarang setelah email tercatat.
	if in.Email != "" {
		h.completePendingInvitation(ctx, convID, in.Email)
	}
}

// completePendingInvitation menutup celah urutan: undangan meeting memakai
// det.AttendeeEmail (JSON details), bukan contacts.email. Bila kontak memberi email
// SETELAH meeting difinalisasi (venue dikonfirmasi lebih dulu, email menyusul), undangan
// tak pernah terkirim. Fungsi ini mencari meeting scheduled pada percakapan ini yang
// belum punya AttendeeEmail, mem-backfill email, lalu mengirim RSVP + .ics. Hanya untuk
// meeting SU-initiated (non-external); meeting yang diinisiasi eksternal → pihak eksternal
// yang mengirim undangan ke pa@hypernet.co.id, jadi dilewati.
func (h *Handler) completePendingInvitation(ctx context.Context, convID, email string) {
	if h.Services == nil || !h.Services.Enabled() || strings.TrimSpace(convID) == "" || strings.TrimSpace(email) == "" {
		return
	}
	m, err := h.Store.ActiveMeetingByConversation(ctx, convID)
	if err != nil || m == nil || m.Status != "scheduled" {
		return
	}
	if m.RequestedVia == "external" {
		return // pihak eksternal yang mengirim undangan/link
	}
	var det meetingDetails
	_ = json.Unmarshal(m.Details, &det)
	if strings.TrimSpace(det.AttendeeEmail) != "" {
		return // sudah punya email → undangan sudah tertangani saat finalisasi
	}
	title := firstNonEmptyStr(det.Title, m.Topic, "Meeting")
	duration := det.DurationMinutes
	if duration <= 0 {
		duration = 60
	}
	dt := ""
	if m.ProposedDatetime != nil {
		dt = m.ProposedDatetime.Format(time.RFC3339)
	}
	if err := h.Services.SendRSVP(ctx, services.RSVPReq{
		To: email, ToName: firstNonEmptyStr(det.AttendeeName, m.ExternalName, email),
		Title: title, Datetime: dt, DurationMinutes: duration, Venue: m.Venue,
		CalendarLink: det.CalendarLink, TeamsLink: det.TeamsLink,
	}); err != nil {
		log.Printf("[INVITE-BACKFILL] meeting #%d kirim RSVP ke %s gagal: %v", m.ID, email, err)
		return
	}
	det.AttendeeEmail = email
	newDetails, _ := json.Marshal(det)
	if err := h.Store.ScheduleMeeting(ctx, m.ID, newDetails, "su", "email peserta diterima setelah finalisasi — undangan dikirim"); err != nil {
		log.Printf("[INVITE-BACKFILL] simpan email meeting #%d gagal: %v", m.ID, err)
	}
	log.Printf("[INVITE-BACKFILL] meeting #%d: email %s di-backfill, undangan RSVP terkirim", m.ID, email)
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

// applyActions menjalankan instruksi terstruktur dari agent.
// srcText adalah pesan asli pemicu turn ini untuk koreksi nomor tujuan.
func (h *Handler) applyActions(ctx context.Context, convID string, contact *model.Contact, actions []model.Action, execID int64, srcText string) {
	// Meeting grup (Fase 1): bila SU meminta menghubungi >=2 orang untuk SATU pertemuan
	// dalam satu balasan, seluruh peserta berbagi groupID yang sama agar dikonsolidasikan
	// (satu approval, satu koordinasi venue, satu laporan). Dihitung dari jumlah SPAWN_AGENT.
	spawnCount := 0
	for _, a := range actions {
		if strings.EqualFold(strings.TrimSpace(a.Type), "SPAWN_AGENT") {
			spawnCount++
		}
	}
	groupID, groupSize := "", 0
	if spawnCount >= 2 {
		groupID = newGroupID()
		groupSize = spawnCount
		log.Printf("[GROUP] %d SPAWN_AGENT dalam satu balasan → meeting grup %s (size=%d)", spawnCount, groupID, groupSize)
	}

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
			h.confirmExistingMeeting(ctx, contact, a)
		case "CONFIRM_VENUE":
			// Support: Bu Nova sudah memastikan SATU venue. Simpan lokasi pasti ke
			// meeting offline yang sedang menunggu venue;
			h.confirmVenue(ctx, contact, a)
		case "RESCHEDULE_MEETING":
			// SU (lewat orchestrator) ingin menjadwal ulang: TUGASKAN PA Communicator
			// menegosiasikan waktu baru dengan pihak eksternal lebih dulu
			act := a
			go h.rescheduleDispatch(contact, act)
		case "SPLIT_GROUP_MEETING":
			// SU (lewat orchestrator) memilih SPLIT saat peserta grup divergen (Fase 2):
			// lepaskan SATU peserta dari grup (peserta lain tetap di jadwal semula),
			// opsional jadwalkan peserta itu ke waktu terpisah.
			act := a
			go h.splitGroupDispatch(contact, act)
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
			// eksternal; selalu lewat approval gate, dijalankan di goroutine sendiri.
			// Bila SU menyuruh menghubungi BEBERAPA orang, stagger antar-kontak: tiap
			// kontak keluar memesan slot berikutnya yang berjarak >= SpawnStaggerInterval
			// dari yang sebelumnya. Berlaku LINTAS batch (beberapa SPAWN_AGENT dalam satu
			// balasan) MAUPUN lintas giliran/pesan (state proses-global di Handler).
			delay := h.reserveSpawnSlot(h.SpawnStaggerInterval)
			act := a
			go h.spawnOutbound(contact, act, srcText, delay, groupID, groupSize)
		case "SET_REMINDER":
			// SU (lewat orchestrator) minta pengingat pada waktu tertentu (sekali atau
			// berulang harian/mingguan). Disimpan ke scheduled_tasks; worker latar
			// belakang menyuruh orchestrator menyampaikannya ke SU saat jatuh tempo.
			// Hanya boleh dari percakapan SU (gerbang di setReminder).
			h.setReminder(ctx, contact, a)
		case "CANCEL_REMINDER":
			// SU (lewat orchestrator) menghentikan pengingat aktif berdasarkan reminderId
			// (dari snapshot [PENGINGAT AKTIF]). Untuk berulang, menghentikan seri.
			// Juga membatalkan digest kalender (created_by='su'). Hanya dari percakapan SU.
			h.cancelReminder(ctx, contact, a)
		case "SCHEDULE_CALENDAR_DIGEST":
			// SU (lewat orchestrator) minta pengecekan kalender terjadwal (mis. tiap hari
			// jam 08:00) yang menarik agenda hari itu lalu menyusun rencana kerja.
			// Disimpan ke scheduled_tasks (kind=calendar_digest). Hanya boleh dari
			// percakapan SU (gerbang di scheduleCalendarDigest).
			h.scheduleCalendarDigest(ctx, contact, a)
		case "WATCH_EMAIL":
			// SU (lewat orchestrator) minta pemantauan inbox: lapor proaktif bila ada email
			// masuk yang cocok kriteria. Disimpan ke email_watches; worker latar belakang
			// menilai & melapor. Hanya dari percakapan SU (gerbang di watchEmail).
			h.watchEmail(ctx, contact, a)
		case "CANCEL_WATCH":
			// SU (lewat orchestrator) menghentikan pantauan email aktif berdasarkan watchId
			// (dari snapshot [PANTAUAN EMAIL AKTIF]). Hanya dari percakapan SU.
			h.cancelWatch(ctx, contact, a)
		case "READ_EMAILS":
			// SU (lewat orchestrator) minta cek inbox SAAT ITU JUGA (on-demand): tarik email
			// terbaru, saring (belum dibaca/belum dibalas/semua), lalu suntik hasil balik ke
			// orchestrator untuk dilaporkan ke SU. Goroutine sendiri karena penarikan Graph +
			// giliran LLM bisa lama. Hanya dari percakapan SU (gerbang di readEmails).
			act := a
			go h.readEmails(contact, act)
		case "SEND_DOCUMENT":
			// SU (lewat orchestrator) minta sebuah laporan/dokumen. Agent menyusun
			// SENDIRI isi & format-nya; gateway hanya mengemas jadi file & mengirim ke
			// SU. Hanya boleh dari percakapan SU (gerbang di sendDocument).
			h.sendDocument(ctx, convID, contact, a, execID)
		case "DEFER_TASK":
			// SU (lewat orchestrator) minta pekerjaan BERAT — riset, penelusuran banyak
			// sumber, penyusunan laporan — dikerjakan di latar belakang.
			h.deferTask(ctx, contact, a)
		case "UPDATE_AGENT_PERSONA":
			// SU (lewat orchestrator) menyesuaikan GAYA & sebagian perilaku ringan agent.
			// Disimpan sebagai overlay & disuntik sebagai konteks tiap giliran;
			h.updateAgentPersona(ctx, contact, a)
		case "ADMIN_FETCH":
			// Admin (lewat agent admin) minta tarik data operasional on-demand (read-only):
			// contacts/executions/usage/approvals/meetings/agents/health.
			act := a
			go h.adminFetch(contact, act)
		case "ADMIN_SPAWN":
			// Admin (lewat agent admin) men-direktif agent lain (orchestrator/support)
			// dengan OTORITAS PENUH lewat sesi kontrol terpisah;
			act := a
			go h.adminSpawn(contact, act)
		case "ADMIN_ADD_CONTACT", "ADMIN_SET_TRUST", "ADMIN_DEL_CONTACT":
			// Admin (lewat agent admin) mengelola whitelist: tambah kontak, ubah trust,
			// atau hapus (soft-delete).
			act := a
			go h.adminManageContact(contact, act)
		case "ADMIN_RESTART_AGENT":
			// Admin (lewat agent admin) me-reset sesi OpenClaw agent target yang
			// mungkin terkontaminasi.
			act := a
			go h.adminRestartAgent(contact, act)
		case "ADMIN_APPROVE", "ADMIN_REJECT":
			// Admin (lewat agent admin) menyetujui/menolak satu approval yang menunggu.
			// Aksi destruktif (approve = lepas pesan ke eksternal) → wajib Confirm=true.
			act := a
			go h.adminDecideApproval(contact, act)
		case "ADMIN_BLOCK_EXTERNAL", "ADMIN_PROMOTE_EXTERNAL":
			// Admin (lewat agent admin) memblokir / mempromosikan kontak external.
			// Mengubah batas kepercayaan → wajib Confirm=true.
			act := a
			go h.adminModerateExternal(contact, act)
		case "ADMIN_RESEND_RSVP":
			// Admin (lewat agent admin) mengirim ulang undangan RSVP meeting terjadwal
			// (pemulihan email gagal). Idempoten & non-destruktif → tanpa konfirmasi.
			act := a
			go h.adminResendRSVP(contact, act)
		case "ADMIN_CANCEL_MEETING":
			// Admin (lewat agent admin) MEMBATALKAN meeting secara SENYAP — status cancelled,
			// hapus event kalender, batalkan pengingat, tolak approval tertaut — TANPA
			// notifikasi ke SU/eksternal.
			act := a
			go h.adminCancelMeeting(contact, act)
		case "ADMIN_MESSAGE_SU":
			// Admin (lewat agent admin) meneruskan pesan/pertanyaan ke Pak Sudianto dan
			// MENGIRIMNYA NYATA ke WhatsApp SU lewat orchestrator di percakapan SU asli
			// (pushToOrchestrator).
			act := a
			go h.adminMessageSU(contact, act)
		case "ADMIN_MESSAGE_SUPPORT":
			// Admin (lewat agent admin) meneruskan pesan/instruksi ke Bu Nova dan
			// MENGIRIMNYA NYATA ke WhatsApp Nova di percakapan support asli
			// (pushToSupport).
			act := a
			go h.adminMessageSupport(contact, act)
		default:
			if a.Type != "" {
				log.Printf("[ACTION] tipe tidak dikenal: %q (diabaikan)", a.Type)
			}
		}
	}
}

// spawnOutbound menjalankan action SPAWN_AGENT untuk kirim WhatsApp keluar.
// Poin singkat:
//   - Hanya SU/orchestrator yang boleh memulai.
//   - Hanya ke pa_communicator/support.
//   - Target di-whitelist sebagai external.
//   - Agent menyusun pesan pembuka.
//   - Pesan wajib lewat approval gate SU.
//   - Jalan di goroutine terpisah karena proses bisa lama.
//   - startDelay > 0 menunda seluruh proses (kontak ke-N pada spawn massal) agar
//     pesan pembuka ke tiap orang tidak terkirim serempak.
func (h *Handler) spawnOutbound(initiator *model.Contact, act model.Action, srcText string, startDelay time.Duration, groupID string, groupSize int) {
	// (1) Keamanan: hanya SU/orchestrator yang boleh memulai kontak keluar.
	if initiator == nil || initiator.TrustLevel != "su" {
		trust := "(nil)"
		if initiator != nil {
			trust = initiator.TrustLevel
		}
		log.Printf("[SPAWN] DITOLAK: inisiator non-SU (trust=%s) target=%s", trust, act.Target)
		return
	}

	// (1b) Stagger antar-kontak: tunda SEBELUM kerja berat (inject/compose) dan sebelum
	// membuat ctx ber-timeout, agar timeout 4 menit tidak tergerus jeda ini.
	if startDelay > 0 {
		log.Printf("[SPAWN] stagger: menunda %v sebelum menghubungi target=%q", startDelay, firstNonEmptyStr(act.TargetName, act.Target))
		time.Sleep(startDelay)
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

	// Koordinasi venue ke Bu Nova otomatis setelah SU menyetujui waktu — spawn manual
	// yang MEMBAWA field meeting (topic/venue/datetime) diblokir agar pesan venue tidak
	// terkirim terlalu awal/dobel. Tugas lain ke Bu Nova yang TIDAK terkait venue meeting
	// (mis. minta bantuan administratif) tetap diizinkan lewat jalur ini.
	if agentID == "support" && (strings.TrimSpace(act.MeetingTopic) != "" ||
		strings.TrimSpace(act.MeetingVenue) != "" || strings.TrimSpace(act.MeetingDatetime) != "") {
		log.Printf("[SPAWN] support DITOLAK: tampak koordinasi venue meeting — itu kini otomatis setelah SU menyetujui waktu (spawn manual diabaikan)")
		return
	}

	// Timeout mencakup jeda tunggu sinkron Google (bila aktif) agar budget 4 menit
	// untuk inject/compose tidak tergerus jeda itu.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute+h.GoogleContactSyncDelay)
	defer cancel()

	task := strings.TrimSpace(act.Task)
	chatID := waha.NormalizeChatID(act.Target) // "628...@c.us"
	phone := strings.TrimSuffix(chatID, "@c.us")

	// (2b) Normalisasi nomor: Input dari user
	humanNums := h.collectSUNumbers(ctx, initiator, srcText)
	// Untuk support, target sudah DIPAKSA = Nova di atas; jangan koreksi/pinjam nomor
	// manusia dari pesan SU (akar bug pesan venue nyasar ke kontak lain).
	if corrected, ok := reconcileMSISDN(phone, humanNums); ok && agentID != "support" {
		log.Printf("[SPAWN] koreksi nomor: target LLM=%q → %s (sumber: pesan SU, kandidat=%v)", phone, corrected, humanNums)
		phone = corrected
		chatID = waha.NormalizeChatID(phone)
	}

	// (2c) Cocokkan nama kontak tersimpan jika target bukan nomor valid.
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

	// (3) Cocokkan target ke kontak tersimpan; jika cocok, pakai nomor yang ada.
	var contact *model.Contact
	var err error
	if agentID == "support" {
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

	// (3b) Pre-flight: pastikan nomor tujuan benar-benar terdaftar di WhatsApp SEBELUM
	// menyusun & mengirim pesan. Memangkas "error 463" kelas nomor-invalid dan mencegah
	// laporan sukses semu. Hanya untuk kontak eksternal (Nova/support = kontak internal
	// tepercaya, dilewati). Fail-open: bila cek-nya sendiri error (WAHA down/timeout),
	// tetap lanjut kirim agar gangguan cek tak memblokir seluruh outbound.
	if h.PreflightCheckNumber && agentID != "support" && h.Waha != nil {
		exists, canonical, cerr := h.Waha.CheckNumberExists(phone)
		if cerr != nil {
			log.Printf("[SPAWN] pre-flight check-exists error utk %s: %v — fail-open, lanjut kirim", phone, cerr)
		} else if !exists {
			log.Printf("[SPAWN] pre-flight: nomor %s TIDAK terdaftar di WhatsApp — batalkan (anti-463)", phone)
			h.notifySpawnFailed(act.TargetName, phone, "nomor tidak terdaftar di WhatsApp")
			return
		} else {
			log.Printf("[SPAWN] pre-flight: nomor %s terdaftar di WhatsApp (chatId=%s) — lanjut", phone, canonical)
		}
	}

	// (3c) Simpan kontak ke Google People API SEBELUM menghubungi, lalu tunggu sinkron
	// ke HP (primary device WA) agar tujuan jadi "kontak dikenal" (bantu tekan error 463).
	// Best-effort & fail-open: kegagalan apa pun di sini TIDAK menghentikan pengiriman.
	// Idempoten: nomor yang sudah pernah disinkron (google_resource_name terisi) dilewati
	// tanpa membuat kontak ganda maupun menunggu ulang. Hanya kontak eksternal.
	if h.Google != nil && h.Google.Enabled() && agentID != "support" {
		already := ""
		if h.Store != nil {
			if rn, gerr := h.Store.GoogleResourceName(ctx, phone); gerr != nil {
				log.Printf("[SPAWN] cek status sinkron Google utk %s gagal: %v — anggap belum sinkron", phone, gerr)
			} else {
				already = rn
			}
		}
		if already != "" {
			log.Printf("[SPAWN] Google: %s sudah tersinkron (%s) — lewati simpan & tunggu", phone, already)
		} else {
			name := firstNonEmptyStr(act.TargetName, contact.Name)
			if rn, gerr := h.Google.CreateContact(ctx, name, phone); gerr != nil {
				log.Printf("[SPAWN] Google createContact utk %s gagal: %v — fail-open, lanjut kirim tanpa tunggu sinkron", phone, gerr)
			} else {
				if h.Store != nil {
					if serr := h.Store.SetGoogleResourceName(ctx, phone, rn); serr != nil {
						log.Printf("[SPAWN] simpan penanda google_resource_name utk %s gagal: %v", phone, serr)
					}
				}
				if h.GoogleContactSyncDelay > 0 {
					log.Printf("[SPAWN] Google: kontak %s tersimpan (%s) — tunggu %v untuk sinkron ke device sebelum kirim", phone, rn, h.GoogleContactSyncDelay)
					select {
					case <-time.After(h.GoogleContactSyncDelay):
					case <-ctx.Done():
						log.Printf("[SPAWN] Google: tunggu sinkron %s dibatalkan: %v", phone, ctx.Err())
						return
					}
				}
			}
		}
	}

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
	initiatedBy := ""
	if initiator != nil {
		initiatedBy = initiator.Phone
	}
	h.recordSpawnMeeting(ctx, convID, agentID, contact, act, groupID, groupSize, initiatedBy)
}

// spawnMeetingReusable cek apakah meeting pending yang ada boleh dipakai ulang.
// Reuse hanya jika tanggalnya cocok atau salah satu belum ada tanggal.
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

// newGroupID membuat penanda grup unik untuk mengaitkan beberapa baris meeting yang
// diinisiasi bersamaan (SU menghubungi >=2 orang untuk satu pertemuan). Tidak perlu
// kriptografis — cukup unik antar-batch: waktu (nano) + angka acak.
func newGroupID() string {
	return fmt.Sprintf("grp:%d:%04x", time.Now().UnixNano(), rand.Intn(0x10000))
}

// recordSpawnMeeting membuat (atau memperbarui) baris meeting_requests berstatus
// 'pending' untuk meeting yang DIINISIASI SU lewat SPAWN_AGENT.
func (h *Handler) recordSpawnMeeting(ctx context.Context, convID, agentID string, contact *model.Contact, act model.Action, groupID string, groupSize int, initiatedBy string) {
	if h.Store == nil {
		return
	}
	// Spawn ke 'support' = koordinasi venue internal (ke Bu Nova)
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

	// Lapis anti-duplikat (jalur SPAWN). Menutup bug "resend konfirmasi → orchestrator
	// membuat meeting baru": bila sudah ada meeting AKTIF (pending/approved/scheduled)
	// dengan pihak + waktu sama di percakapan MANA PUN (termasuk yang sudah 'scheduled'
	// di percakapan ini — yang sengaja tidak "reusable" di atas),
	if proposed != nil && strings.TrimSpace(name) != "" {
		if dup, derr := h.Store.FindActiveMeetingByPartyAt(ctx, name, *proposed, ""); derr != nil {
			log.Printf("[SPAWN-MEETING] cek duplikat conv=%s gagal: %v", convID, derr)
		} else if dup != nil {
			log.Printf("[SPAWN-MEETING] duplikat DICEGAH conv=%s: sudah ada meeting #%d (status=%s) pihak=%q datetime=%v — tidak membuat baris baru",
				convID, dup.ID, dup.Status, name, proposed)
			return
		}
	}

	det := meetingDetails{Title: topic, AttendeeName: name, AttendeeEmail: email, InitiatedBy: strings.TrimSpace(initiatedBy)}
	// Meeting grup (Fase 1): tandai keanggotaan grup agar konsolidasi (satu approval,
	// satu koordinasi venue, satu laporan) bisa dilakukan lintas peserta.
	if strings.TrimSpace(groupID) != "" && groupSize >= 2 {
		det.GroupID = groupID
		det.GroupSize = groupSize
	}
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

	// Konsolidasi lintas-turn: bila balasan ini BUKAN grup batch (groupID kosong) namun
	// bertopik + berwaktu sama dengan meeting SU-initiated lain di percakapan berbeda,
	// tautkan keduanya ke satu groupId.
	if strings.TrimSpace(groupID) == "" && proposed != nil && topic != "" {
		h.linkSpawnMeetingToSiblingGroup(ctx, mid, convID, topic, *proposed)
	}
}

// linkSpawnMeetingToSiblingGroup mencari meeting SU-initiated bertopik+berwaktu sama di
// percakapan lain dan menautkan meeting baru (newID) ke grup yang sama.
func (h *Handler) linkSpawnMeetingToSiblingGroup(ctx context.Context, newID int64, convID, topic string, at time.Time) {
	sibling, err := h.Store.FindGroupableSpawnSibling(ctx, topic, at, convID)
	if err != nil {
		log.Printf("[GROUP] cari saudara utk #%d gagal: %v", newID, err)
		return
	}
	if sibling == nil || sibling.ID == newID {
		return
	}
	var sd meetingDetails
	_ = json.Unmarshal(sibling.Details, &sd)
	groupID := strings.TrimSpace(sd.GroupID)
	setIDs := []int64{newID}
	if groupID == "" {
		groupID = newGroupID()
		setIDs = append(setIDs, sibling.ID)
	}
	size, err := h.Store.AttachMeetingsToGroup(ctx, groupID, setIDs)
	if err != nil {
		log.Printf("[GROUP] tautkan #%d ke grup %s (saudara #%d) gagal: %v", newID, groupID, sibling.ID, err)
		return
	}
	log.Printf("[GROUP] penambahan susulan: #%d ditautkan ke grup %s bersama #%d (size=%d)",
		newID, groupID, sibling.ID, size)
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
		"Perkenalkan dirimu dengan menyebut NAMAMU sendiri sebagai asisten Pak Sudianto (nama sesuai identitas di SOUL-mu). " +
		"Tulis isi pesan pembuka itu pada field \"response\" — " +
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

// dispatchNovaVenue mengoordinasikan venue meeting offline via agent support.
// Dipakai untuk koordinasi awal dan reschedule, lalu konfirmasi lokasi lewat CONFIRM_VENUE.
func (h *Handler) dispatchNovaVenue(meetingID int64, instruction string) {
	if strings.TrimSpace(h.NovaPhone) == "" {
		log.Printf("[VENUE-COORD] NovaPhone kosong — tidak bisa koordinasi venue meeting #%d", meetingID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	m, err := h.Store.MeetingByID(ctx, meetingID)
	if err != nil || m == nil {
		log.Printf("[VENUE-COORD] meeting #%d tidak ditemukan: %v", meetingID, err)
		return
	}

	convID := "agent:support:" + h.NovaPhone
	chatID := waha.NormalizeChatID(h.NovaPhone)
	nova := &model.Contact{Name: "Nova", Phone: h.NovaPhone, TrustLevel: "semi_trusted"}

	injectMsg := buildFollowupInject(instruction)
	if h.Memory != nil {
		if mc, aerr := h.Memory.Assemble(ctx, convID, nova); aerr != nil {
			log.Printf("[VENUE-COORD] assemble konteks gagal conv=%s: %v (lanjut tanpa konteks)", convID, aerr)
		} else {
			mc.LiveStatus = buildDateAnchor()
			injectMsg = mc.BuildInjectMessage(injectMsg)
		}
	}

	reply, meta, ierr := h.injectWithRecovery(ctx, "support", convID, injectMsg)
	if errors.Is(ierr, openclaw.ErrNoReply) {
		h.logExecution(ctx, convID, "support", nova, injectMsg, nil, meta, "no_reply", "")
		log.Printf("[VENUE-COORD] support memilih diam untuk meeting #%d", m.ID)
		return
	}
	if ierr != nil {
		h.logExecution(ctx, convID, "support", nova, injectMsg, nil, meta, outcomeFromErr(ierr), ierr.Error())
		log.Printf("[VENUE-COORD] inject support gagal meeting #%d: %v", m.ID, ierr)
		return
	}
	execID := h.logExecution(ctx, convID, "support", nova, injectMsg, reply, meta, "ok", "")
	// Bila support langsung CONFIRM_VENUE (mis. lokasi tersedia), aksi ini akan
	// mengonfirmasi venue & memicu finalisasi meeting.
	h.applyActions(ctx, convID, nova, reply.Actions, execID, "")

	if h.Memory != nil {
		if werr := h.Memory.Write(ctx, convID, nova, "support", "[Koordinasi venue] "+instruction, reply.Response, reply.NewFacts); werr != nil {
			log.Printf("[VENUE-COORD] memory write gagal conv=%s: %v", convID, werr)
		}
	}
	h.sendAndRecord(ctx, func() error { return h.Waha.SendToChat(chatID, reply.Response) },
		model.OutboundMessage{
			ExecutionID: execPtr(execID), ConversationID: convID, ContactID: cidPtr(nova),
			AgentID: "support", TargetChat: chatID, Kind: "agent_reply", Text: reply.Response,
		})
	log.Printf("[VENUE-COORD] Bu Nova ditugaskan koordinasi venue meeting #%d", m.ID)
}

// buildVenueRecommendations merangkum rekomendasi lokasi dari riwayat meeting untuk Bu Nova.
func (h *Handler) buildVenueRecommendations(ctx context.Context, externalName string) string {
	if h.Store == nil {
		return ""
	}
	fmtList := func(vs []db.VenueSuggestion) string {
		parts := make([]string, 0, len(vs))
		for _, v := range vs {
			s := v.Name
			if v.Address != "" {
				s += " (" + v.Address + ")"
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, "; ")
	}
	if hist, err := h.Store.VenueHistoryForExternal(ctx, externalName, 3); err == nil && len(hist) > 0 {
		return " Lokasi yang PERNAH dipakai dengan " + firstNonEmptyStr(strings.TrimSpace(externalName), "pihak ini") +
			": " + fmtList(hist) + " — utamakan bila masih cocok."
	}
	if freq, err := h.Store.FrequentVenues(ctx, 3); err == nil && len(freq) > 0 {
		return " Lokasi yang sering dipakai Pak Sudianto: " + fmtList(freq) + " — boleh dijadikan referensi."
	}
	return ""
}

// venueCompletionRule = instruksi baku (dipakai koordinasi awal & ulang) agar Bu Nova selalu
// memberi alamat LENGKAP; bila hanya menyebut nama tempat, support melengkapi alamat (boleh via
// web) lalu MENGONFIRMASINYA ke Bu Nova sebelum difinalkan.
const venueCompletionRule = " Tolong koordinasikan lokasi yang sesuai (boleh cari via web bila perlu). " +
	"Pastikan ALAMAT LENGKAP: bila Bu Nova hanya menyebut nama tempat tanpa alamat jelas, lengkapi " +
	"alamatnya lalu KONFIRMASIKAN ke Bu Nova dulu — jangan CONFIRM_VENUE sebelum Bu Nova membenarkan " +
	"alamat itu. Setelah lokasi & alamat pasti, kirim CONFIRM_VENUE beserta tanggal meeting. Tanpa membahas biaya."

// dispatchVenueCoordination mengoordinasikan venue awal meeting offline setelah SU menyetujui waktu.
func (h *Handler) dispatchVenueCoordination(meetingID int64, ed meetingDetails, when *time.Time) {
	lctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	m, err := h.Store.MeetingByID(lctx, meetingID)
	if err != nil || m == nil {
		cancel()
		log.Printf("[VENUE-COORD] meeting #%d tidak ditemukan untuk koordinasi awal: %v", meetingID, err)
		return
	}
	who := firstNonEmptyStr(m.ExternalName, ed.AttendeeName, "pihak eksternal")
	recs := h.buildVenueRecommendations(lctx, m.ExternalName)
	pref := strings.TrimSpace(m.Venue)
	topic := firstNonEmptyStr(ed.Title, m.Topic)
	cancel()

	var b strings.Builder
	b.WriteString("Pak Sudianto sudah menyetujui pertemuan tatap muka dengan " + who + ".")
	if when != nil {
		b.WriteString(" Waktu: " + formatWIBLong(*when) + ".")
	}
	if topic != "" {
		b.WriteString(" Topik: " + topic + ".")
	}
	if pref != "" {
		b.WriteString(" Preferensi area/lokasi dari Pak Sudianto: " + pref + ".")
	}
	b.WriteString(recs)
	b.WriteString(venueCompletionRule)
	h.dispatchNovaVenue(meetingID, b.String())
}

// dispatchVenueRecoordination koordinasi ulang venue meeting offline setelah reschedule.
func (h *Handler) dispatchVenueRecoordination(meetingID int64, ed meetingDetails, newTime *time.Time) {
	lctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	m, err := h.Store.MeetingByID(lctx, meetingID)
	if err != nil || m == nil {
		cancel()
		log.Printf("[VENUE-RECOORD] meeting #%d tidak ditemukan: %v", meetingID, err)
		return
	}
	who := firstNonEmptyStr(m.ExternalName, ed.AttendeeName, "pihak eksternal")
	prevVenue := firstNonEmptyStr(strings.TrimSpace(m.Venue), ed.VenueName)
	recs := h.buildVenueRecommendations(lctx, m.ExternalName)
	cancel()

	var b strings.Builder
	b.WriteString("Pak Sudianto menjadwal ulang pertemuan tatap muka dengan " + who + ".")
	// Tegaskan pembatalan lokasi & waktu LAMA agar Nova melepaskan booking sebelumnya.
	if prevVenue != "" || strings.TrimSpace(ed.RescheduleFrom) != "" {
		b.WriteString(" Mohon batalkan booking sebelumnya:")
		if prevVenue != "" {
			b.WriteString(" lokasi " + prevVenue)
		}
		if rf := strings.TrimSpace(ed.RescheduleFrom); rf != "" {
			if oldAt, perr := time.Parse(time.RFC3339, rf); perr == nil {
				b.WriteString(" pada " + formatWIBLong(oldAt))
			}
		}
		b.WriteString(".")
	}
	if newTime != nil {
		b.WriteString(" Waktu baru: " + formatWIBLong(*newTime) + ".")
	}
	b.WriteString(recs)
	b.WriteString(" Boleh pertahankan lokasi lama bila masih tersedia, atau carikan alternatif yang sesuai." +
		venueCompletionRule)
	h.dispatchNovaVenue(meetingID, b.String())
}

// dispatchVenueCancellation memberi tahu Bu Nova bahwa meeting offline DIBATALKAN, agar ia
// MELEPASKAN/membatalkan booking lokasi (bila sudah ada) dan berhenti mencari venue untuk
// pertemuan ini.
func (h *Handler) dispatchVenueCancellation(meetingID int64, ed meetingDetails, when *time.Time) {
	lctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	m, err := h.Store.MeetingByID(lctx, meetingID)
	cancel()
	if err != nil || m == nil {
		log.Printf("[VENUE-CANCEL] meeting #%d tidak ditemukan: %v", meetingID, err)
		return
	}
	who := firstNonEmptyStr(m.ExternalName, ed.AttendeeName, "pihak eksternal")
	prevVenue := firstNonEmptyStr(strings.TrimSpace(m.Venue), ed.VenueName)
	var b strings.Builder
	b.WriteString("Pak Sudianto MEMBATALKAN pertemuan tatap muka dengan " + who + ".")
	if when != nil {
		b.WriteString(" Jadwal yang dibatalkan: " + formatWIBLong(*when) + ".")
	}
	if prevVenue != "" {
		b.WriteString(" Lokasi yang sudah/sedang dikoordinasikan: " + prevVenue + ".")
	}
	b.WriteString(" Mohon LEPASKAN/BATALKAN pemesanan lokasi tersebut bila ada, dan tidak perlu " +
		"melanjutkan pencarian venue untuk pertemuan ini. Tidak perlu CONFIRM_VENUE. Sampaikan dengan sopan. " +
		"Tanpa membahas biaya.")
	h.dispatchNovaVenue(meetingID, b.String())
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
		h.notifySU("Maaf, meeting yang dimaksud tidak ditemukan. Reschedule dibatalkan.")
		return
	}
	if m.Status == "cancelled" || m.Status == "rejected" {
		h.notifySU(fmt.Sprintf("Meeting dengan %s sudah %s — tidak dapat dijadwalkan ulang.", meetingWho(m), meetingStatusID(m.Status)))
		return
	}

	var det meetingDetails
	_ = json.Unmarshal(m.Details, &det)

	// Meeting grup
	if strings.TrimSpace(det.GroupID) != "" && det.GroupSize >= 2 {
		h.rescheduleGroupDispatch(ctx, det.GroupID, a)
		return
	}

	if derr := h.dispatchRescheduleNegotiation(ctx, m, a); derr != nil {
		log.Printf("[RESCHEDULE] dispatch PA Communicator #%d gagal: %v", m.ID, derr)
		h.notifySU(fmt.Sprintf("⚠️ Gagal menghubungi pihak terkait untuk reschedule meeting dengan %s. Silakan coba lagi sebentar.", meetingWho(m)))
		return
	}
	log.Printf("[RESCHEDULE] PA Communicator ditugaskan menegosiasikan waktu baru meeting #%d", m.ID)
}

// dispatchRescheduleNegotiation menyiapkan SATU meeting untuk penjadwalan ulang (tandai
// ReschedulePending + RescheduleFrom, reset venue/timeAgreed bila offline) lalu menugaskan
// PA Communicator menegosiasikan waktu baru dengan pihak eksternal.
func (h *Handler) dispatchRescheduleNegotiation(ctx context.Context, m *model.MeetingRequest, a model.Action) error {
	var det meetingDetails
	_ = json.Unmarshal(m.Details, &det)
	oldTime := ""
	if m.ProposedDatetime != nil {
		oldTime = m.ProposedDatetime.Format(time.RFC3339)
	}
	det.ReschedulePending = true
	det.RescheduleFrom = oldTime

	// Offline meeting: koordinasi ulang venue untuk waktu baru.
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
	return h.dispatchCommunicator(ctx, m, b.String())
}

func (h *Handler) rescheduleGroupDispatch(ctx context.Context, groupID string, a model.Action) {
	if h.Store == nil {
		return
	}
	if err := h.Store.ResetGroupAgreementForReschedule(ctx, groupID); err != nil {
		log.Printf("[RESCHEDULE-GRP] reset kesepakatan grup %s gagal: %v", groupID, err)
		h.notifySU("⚠️ Gagal menyiapkan penjadwalan ulang meeting grup. Silakan coba lagi sebentar.")
		return
	}
	meetings, err := h.Store.MeetingsByGroup(ctx, groupID)
	if err != nil || len(meetings) == 0 {
		log.Printf("[RESCHEDULE-GRP] ambil peserta grup %s gagal: %v", groupID, err)
		return
	}
	newAt, perr := time.Parse(time.RFC3339, strings.TrimSpace(a.NewDatetime))
	ok := 0
	for _, m := range meetings {
		if derr := h.dispatchRescheduleNegotiation(ctx, m, a); derr != nil {
			log.Printf("[RESCHEDULE-GRP] dispatch #%d (grup %s) gagal: %v", m.ID, groupID, derr)
			continue
		}
		ok++
	}
	when := "(waktu baru)"
	if perr == nil {
		when = formatWIBLong(newAt)
	}
	log.Printf("[RESCHEDULE-GRP] grup %s dinegosiasi ulang ke %s (%d/%d peserta)", groupID, when, ok, len(meetings))
	// h.notifySU(fmt.Sprintf("🔄 Menegosiasikan ulang waktu meeting grup ke %s untuk %d peserta. "+
	// 	"Saya akan mengabari bila semua sudah menyepakati waktu baru untuk persetujuan akhir Anda.", when, ok))
}

// splitGroupDispatch melepaskan SATU peserta dari meeting grup.
func (h *Handler) splitGroupDispatch(initiator *model.Contact, a model.Action) {
	if initiator == nil || initiator.TrustLevel != "su" {
		trust := "(nil)"
		if initiator != nil {
			trust = initiator.TrustLevel
		}
		log.Printf("[SPLIT] DITOLAK: inisiator non-SU (trust=%s) meetingId=%d", trust, a.MeetingID)
		return
	}
	if a.MeetingID <= 0 || h.Store == nil {
		log.Printf("[SPLIT] meetingId tidak valid (%d) — diabaikan", a.MeetingID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	m, err := h.Store.MeetingByID(ctx, a.MeetingID)
	if err != nil || m == nil {
		h.notifySU("Maaf, meeting yang dimaksud tidak ditemukan. Pemisahan dibatalkan.")
		return
	}
	if m.Status == "cancelled" || m.Status == "rejected" {
		h.notifySU(fmt.Sprintf("Meeting dengan %s sudah %s — tidak dapat dipisah.", meetingWho(m), meetingStatusID(m.Status)))
		return
	}
	var det meetingDetails
	_ = json.Unmarshal(m.Details, &det)
	if strings.TrimSpace(det.GroupID) == "" || det.GroupSize < 2 {
		h.notifySU(fmt.Sprintf("Meeting dengan %s bukan bagian dari meeting grup — tidak ada yang perlu dipisah.", meetingWho(m)))
		return
	}
	groupID := det.GroupID
	who := firstNonEmptyStr(det.AttendeeName, m.ExternalName, "peserta")

	remaining, derr := h.Store.DetachMeetingFromGroup(ctx, m.ID, groupID)
	if derr != nil {
		log.Printf("[SPLIT] lepas #%d dari grup %s gagal: %v", m.ID, groupID, derr)
		h.notifySU(fmt.Sprintf("⚠️ Gagal memisahkan %s dari meeting grup. Silakan coba lagi sebentar.", who))
		return
	}
	log.Printf("[SPLIT] peserta #%d (%s) dilepas dari grup %s — sisa %d peserta", m.ID, who, groupID, remaining)

	// Peserta yang dilepas: batalkan / negosiasi ulang solo / biarkan.
	switch {
	case strings.EqualFold(strings.TrimSpace(a.ChangeKind), "cancel"):
		// cancelDispatch menangani pemberitahuan eksternal + finalisasi + laporan SU sendiri.
		h.cancelDispatch(initiator, a)
	case strings.TrimSpace(a.NewDatetime) != "":
		if sm, serr := h.Store.MeetingByID(ctx, m.ID); serr == nil && sm != nil {
			if e := h.dispatchRescheduleNegotiation(ctx, sm, a); e != nil {
				log.Printf("[SPLIT] negosiasi ulang solo #%d gagal: %v", m.ID, e)
			}
		}
		h.notifySU(fmt.Sprintf("✅ %s dipisah dari meeting grup dan dinegosiasikan ke waktu terpisah. "+
			"%d peserta lain tetap pada jadwal semula.", who, remaining))
	default:
		h.notifySU(fmt.Sprintf("✅ %s dipisah dari meeting grup (melanjutkan penjadwalan sendiri). "+
			"%d peserta lain tetap pada jadwal semula.", who, remaining))
	}

	// Sisa grup mungkin kini lengkap (semua sudah setuju) → picu notifikasi persetujuan.
	h.maybeNotifyAfterSplit(ctx, groupID)
}

// maybeNotifyAfterSplit memeriksa apakah SISA peserta grup (setelah satu peserta dilepas)
// kini SEMUANYA sudah menyepakati waktu; bila ya, kirim SATU notifikasi persetujuan (gabungan
// bila ≥2 peserta, atau solo bila tinggal.
func (h *Handler) maybeNotifyAfterSplit(ctx context.Context, groupID string) {
	if h.Store == nil {
		return
	}
	meetings, err := h.Store.MeetingsByGroup(ctx, groupID)
	if err != nil || len(meetings) == 0 {
		return
	}
	var primary int64
	for _, m := range meetings {
		if m.ApprovalID == nil {
			return // masih ada peserta yang belum setuju — tahan
		}
		if primary == 0 {
			primary = *m.ApprovalID
		}
	}
	if primary == 0 {
		return
	}
	first, merr := h.Store.MarkGroupApprovalNotified(ctx, groupID)
	if merr != nil {
		log.Printf("[SPLIT] tandai notifikasi grup %s gagal: %v", groupID, merr)
		return
	}
	if !first {
		return
	}
	h.notifySUGroupApproval(ctx, groupID, primary)
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
		h.notifySU("Maaf, meeting yang dimaksud tidak ditemukan. Pembatalan dibatalkan.")
		return
	}
	if m.Status == "cancelled" || m.Status == "rejected" {
		h.notifySU(fmt.Sprintf("Meeting dengan %s memang sudah %s.", meetingWho(m), meetingStatusID(m.Status)))
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

	// 3) Jika meeting OFFLINE dan Bu Nova sudah dilibatkan, beri tahu untuk melepas booking.
	// Nova hanya dihubungi setelah SU menyetujui waktu atau venue sudah dikonfirmasi.
	// Jika masih 'pending', jangan libatkan Nova. m.Status di sini masih nilai sebelum pembatalan.
	novaNote := ""
	novaEngaged := det.VenueCoordination && (det.VenueConfirmed || m.Status == "approved" || m.Status == "scheduled")
	if novaEngaged {
		go h.dispatchVenueCancellation(m.ID, det, m.ProposedDatetime)
		novaNote = " Bu Nova juga diberi tahu untuk melepaskan koordinasi lokasi."
	}

	who := firstNonEmptyStr(m.ExternalName, det.AttendeeName, "pihak terkait")
	h.notifySU(fmt.Sprintf("✅ Meeting dengan %s dibatalkan. PA Communicator sudah mengabari pihak terkait%s%s.%s",
		who, emailNote, calNote, novaNote))
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

	// Meeting grup (Fase 2 — DIVERGENSI): bila peserta grup minta ubah/batal, laporkan
	// dengan konteks grup + tawarkan dua opsi ke SU (konvergensi vs split) — jangan pakai
	// jalur solo yang hanya menjadwal ulang satu peserta.
	var gdet meetingDetails
	_ = json.Unmarshal(m.Details, &gdet)
	if strings.TrimSpace(gdet.GroupID) != "" && gdet.GroupSize >= 2 {
		h.requestGroupMeetingChange(ctx, m, gdet, a)
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
		msg = fmt.Sprintf("📩 %s meminta *pembatalan* meeting%s.", who, when)
		if reason != "" {
			msg += " Alasan: " + reason + "."
		}
		msg += fmt.Sprintf("\n\nUntuk membatalkan, beri tahu saya: \"batalkan meeting dengan %s\". Untuk menolak permintaan, abaikan saja.", who)
	case "reschedule":
		msg = fmt.Sprintf("📩 %s meminta *perubahan jadwal* meeting%s.", who, when)
		if nt := strings.TrimSpace(a.NewDatetime); nt != "" {
			if t, perr := time.Parse(time.RFC3339, nt); perr == nil {
				msg += " Usulan waktu baru: " + formatWIBLong(t) + "."
			}
		}
		if reason != "" {
			msg += " Alasan: " + reason + "."
		}
		msg += fmt.Sprintf("\n\nUntuk menyetujui, beri tahu saya: \"ubah meeting dengan %s ke <tanggal & jam>\".", who)
	default:
		log.Printf("[CHANGE-REQ] changeKind tak dikenal (%q) conv=%s — diabaikan", a.ChangeKind, convID)
		return
	}
	h.notifySU(msg)
	log.Printf("[CHANGE-REQ] permintaan %q meeting #%d dari %s diteruskan ke SU", kind, m.ID, who)
}

// requestGroupMeetingChange melaporkan DIVERGENSI meeting grup (Fase 2) ke SU: satu peserta
// tidak bisa/ingin ubah waktu, sementara peserta lain mungkin sudah setuju. SU memutuskan per
// kejadian antara dua opsi:
//
//	(A) KONVERGENSI — pindahkan SELURUH grup ke waktu baru (semua dinegosiasi ulang).
//	(B) SPLIT       — biarkan peserta lain di waktu semula; jadwalkan peserta divergen sendiri.
//
// Fungsi ini HANYA melapor + memberi instruksi; eksekusi menunggu keputusan SU (RESCHEDULE
// grup untuk konvergensi, atau SPLIT_GROUP_MEETING untuk split).
func (h *Handler) requestGroupMeetingChange(ctx context.Context, m *model.MeetingRequest, det meetingDetails, a model.Action) {
	kind := strings.ToLower(strings.TrimSpace(a.ChangeKind))
	who := firstNonEmptyStr(det.AttendeeName, m.ExternalName, "Salah satu peserta")
	reason := strings.TrimSpace(a.Reason)
	newTimeStr := ""
	if nt := strings.TrimSpace(a.NewDatetime); nt != "" {
		if t, perr := time.Parse(time.RFC3339, nt); perr == nil {
			newTimeStr = formatWIBLong(t)
		}
	}

	// Ringkas status peserta grup lain (sudah setuju vs masih menunggu).
	var lines []string
	if h.Store != nil {
		if meetings, err := h.Store.MeetingsByGroup(ctx, det.GroupID); err == nil {
			for _, sib := range meetings {
				var sd meetingDetails
				_ = json.Unmarshal(sib.Details, &sd)
				name := firstNonEmptyStr(sd.AttendeeName, sib.ExternalName, "Peserta")
				status := "masih menunggu konfirmasi"
				if sib.ID == m.ID {
					if kind == "cancel" {
						status = "⚠️ minta *batal*"
					} else {
						status = "⚠️ minta *ubah waktu*"
						if newTimeStr != "" {
							status += " → usul " + newTimeStr
						}
					}
				} else if sib.ApprovalID != nil {
					status = "sudah setuju"
				}
				lines = append(lines, "• "+name+" — "+status)
			}
		}
	}

	curTime := ""
	if m.ProposedDatetime != nil {
		curTime = formatWIBLong(*m.ProposedDatetime)
	}
	var b strings.Builder
	b.WriteString("⚠️ *Perubahan pada meeting grup*\n")
	if kind == "cancel" {
		b.WriteString(fmt.Sprintf("%s meminta *pembatalan* keikutsertaannya", who))
	} else {
		b.WriteString(fmt.Sprintf("%s *tidak bisa* di waktu yang diusulkan", who))
		if newTimeStr != "" {
			b.WriteString(" dan mengusulkan " + newTimeStr)
		}
	}
	if curTime != "" {
		b.WriteString(" (jadwal grup saat ini: " + curTime + ")")
	}
	b.WriteString(".")
	if reason != "" {
		b.WriteString(" Alasan: " + reason + ".")
	}
	if len(lines) > 0 {
		b.WriteString("\n\nStatus peserta:\n" + strings.Join(lines, "\n"))
	}
	b.WriteString("\n\nPilihan Anda:\n")
	b.WriteString("(A) *Pindahkan semua* ke waktu baru — balas: \"ubah meeting grup ke <tanggal & jam>\".\n")
	if kind == "cancel" {
		b.WriteString(fmt.Sprintf("(B) *Lepaskan %s* dari grup (peserta lain tetap) — balas: \"keluarkan %s dari meeting grup\".", who, who))
	} else {
		b.WriteString(fmt.Sprintf("(B) *Jadwalkan %s terpisah* (peserta lain tetap di jadwal semula) — balas: \"jadwalkan %s terpisah", who, who))
		if newTimeStr != "" {
			b.WriteString(" di " + newTimeStr)
		}
		b.WriteString("\".")
	}
	h.notifySU(b.String())
	log.Printf("[CHANGE-REQ-GRP] divergensi grup %s dari %s (kind=%s) dilaporkan ke SU", det.GroupID, who, kind)
}

// meetingWho merangkai nama pihak meeting untuk pesan ke SU tanpa ID mentah
// (mis. "Pak Ikrom (Hypernet)"). Orchestrator tetap memetakan ke meetingId lewat
// snapshot [STATUS MEETING TERKINI], jadi SU cukup menyebut nama/waktu.
func meetingWho(m *model.MeetingRequest) string {
	if m == nil {
		return "pihak terkait"
	}
	var det meetingDetails
	_ = json.Unmarshal(m.Details, &det)
	who := firstNonEmptyStr(strings.TrimSpace(m.ExternalName), strings.TrimSpace(det.AttendeeName), "pihak terkait")
	if c := strings.TrimSpace(m.ExternalCompany); c != "" {
		who += " (" + c + ")"
	}
	return who
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
// pada details-nya (idempoten), tanpa mengubah status/jadwal.
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
	var mdt time.Time
	haveDT := false
	if dt := strings.TrimSpace(a.MeetingDatetime); dt != "" {
		if t, perr := time.Parse(time.RFC3339, dt); perr == nil {
			mdt = t
			haveDT = true
			wibDate = t.In(wibZone).Format("2006-01-02")
		} else {
			log.Printf("[VENUE] CONFIRM_VENUE datetime tak valid (%q): %v — pakai fallback", dt, perr)
		}
	}
	switch {
	case haveDT:
		m, err = h.Store.FindVenuePendingMeetingByDatetime(ctx, mdt)
		if err == nil && m == nil {
			log.Printf("[VENUE] CONFIRM_VENUE jam %s tak cocok persis meeting mana pun — fallback per-tanggal %s",
				mdt.In(wibZone).Format("15:04"), wibDate)
			m, err = h.Store.FindVenuePendingMeetingByDate(ctx, wibDate)
		}
	default:
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

	// Meeting grup (Fase 1): SATU venue dari Bu Nova berlaku untuk SELURUH peserta grup.
	// Terapkan lokasi ke semua peserta offline, lalu finalisasi grup dengan SATU laporan.
	if strings.TrimSpace(ed.GroupID) != "" && ed.GroupSize >= 2 {
		siblings, gerr := h.Store.MeetingsByGroup(ctx, ed.GroupID)
		if gerr != nil {
			log.Printf("[VENUE] grup %s: ambil peserta gagal: %v — fallback ke satu meeting", ed.GroupID, gerr)
		}
		applied := 0
		for _, sib := range siblings {
			var sd meetingDetails
			_ = json.Unmarshal(sib.Details, &sd)
			if !sd.VenueCoordination {
				continue
			}
			if h.applyConfirmedVenue(ctx, sib, name, addr) {
				applied++
			}
		}
		if applied == 0 {
			// Fallback aman: minimal terapkan ke meeting yang ditemukan.
			h.applyConfirmedVenue(ctx, m, name, addr)
		}
		log.Printf("[VENUE] grup %s: venue %q diterapkan ke %d peserta — finalisasi grup", ed.GroupID, name, applied)
		go h.finalizeOfflineGroup(ed.GroupID)
		return
	}

	if !h.applyConfirmedVenue(ctx, m, name, addr) {
		return
	}
	// Alur baru: tidak ada approval SU kedua. Bila waktu sudah disetujui SU (status
	// 'approved'), finalisasi meeting menyeluruh sekarang (kalender + undangan + konfirmasi
	// ke eksternal). finalizeOfflineMeeting idempoten & menahan diri bila belum siap.
	go h.finalizeOfflineMeeting(m.ID)
}

// applyConfirmedVenue menyimpan lokasi pasti dari Bu Nova ke satu meeting (kolom venue +
// detail venueConfirmed). Mengembalikan false bila gagal menyimpan kolom venue.
func (h *Handler) applyConfirmedVenue(ctx context.Context, m *model.MeetingRequest, name, addr string) bool {
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
	if err := h.Store.UpdateMeetingPlan(ctx, m.ID, "", "onsite", venueFull, nil, "support", "venue dikonfirmasi Bu Nova"); err != nil {
		log.Printf("[VENUE] simpan venue meeting #%d gagal: %v", m.ID, err)
		return false
	}
	if err := h.Store.UpdateMeetingDetails(ctx, m.ID, det, "support", "venueConfirmed + lokasi pasti tersimpan"); err != nil {
		log.Printf("[VENUE] simpan detail venue meeting #%d gagal: %v", m.ID, err)
	}
	log.Printf("[VENUE] meeting #%d venue dikonfirmasi: %q (timeAgreed=%v)", m.ID, venueFull, ed.TimeAgreed)
	return true
}

// deferMeetingForVenue dipanggil saat waktu meeting offline sudah disepakati,
// sementara lokasi masih dikoordinasikan. buildVenueTimeReinforcement menegaskan
// agar persetujuan waktu dikirim sebagai sinyal terstruktur ke gateway.
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

	// Alur baru: waktu yang disepakati eksternal diajukan ke SU untuk PERSETUJUAN WAKTU
	// lebih dulu (baik meeting baru maupun reschedule). Koordinasi Bu Nova (venue) baru
	// dilakukan SETELAH SU menyetujui waktu — dipicu dari DecideApproval, bukan di sini.
	h.presentTimeApprovalToSU(ctx, existing.ID)
}

// presentTimeApprovalToSU mengajukan waktu yang disepakati pihak eksternal ke SU untuk
// persetujuan. Lokasi belum dikomunikasikan; koordinasi venue dilakukan setelah SU
// menyetujui. Idempoten: tidak mengajukan ulang bila meeting sudah tertaut approval.
func (h *Handler) presentTimeApprovalToSU(ctx context.Context, meetingID int64) {
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
	if !ed.TimeAgreed {
		log.Printf("[VENUE] meeting #%d belum timeAgreed — tidak diajukan ke SU", m.ID)
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
	greet := "Halo"
	if who != "" {
		greet = "Halo " + who
	}
	// Pesan tertahan ke pihak eksternal: dikirim saat SU MENYETUJUI waktu — mengabari
	// bahwa waktu sudah dikonfirmasi, lokasi menyusul (konfirmasi menyeluruh dgn lokasi
	// dikirim belakangan setelah Bu Nova memastikan venue).
	heldMsg := fmt.Sprintf("%s, kabar baik — waktu pertemuan dengan Pak Sudianto pada %s sudah "+
		"dikonfirmasi. Lokasi pastinya sedang kami finalkan dan akan segera kami kabari kembali. "+
		"Terima kasih. 🙏", greet, formatWIBLong(*m.ProposedDatetime))
	nowPresented := m.ProposedDatetime.Format(time.RFC3339)

	// Sudah ada approval tertaut. Jangan ajukan approval baru — tetapi bila WAKTU berubah
	// (pihak eksternal menegosiasi ulang) sebelum SU memutuskan, segarkan approval yang
	// masih pending agar SU menyetujui waktu TERKINI, bukan waktu usang.
	if m.ApprovalID != nil {
		ap := *m.ApprovalID
		existingAp, gerr := h.Store.GetApproval(ctx, ap)
		if gerr != nil || existingAp == nil {
			log.Printf("[VENUE] meeting #%d: ambil approval #%d gagal: %v — tidak diajukan ulang", m.ID, ap, gerr)
			return
		}
		if existingAp.Status != "pending" {
			log.Printf("[VENUE] meeting #%d approval #%d sudah %s — tidak diubah", m.ID, ap, existingAp.Status)
			return
		}
		if ed.TimePresentedAt == nowPresented {
			log.Printf("[VENUE] meeting #%d approval #%d: waktu tak berubah — tidak notif ulang SU", m.ID, ap)
			return
		}
		if uerr := h.Store.UpdateApprovalResponse(ctx, ap, heldMsg); uerr != nil {
			log.Printf("[VENUE] meeting #%d perbarui teks approval #%d gagal: %v", m.ID, ap, uerr)
			return
		}
		ed.TimePresentedAt = nowPresented
		if det, merr := json.Marshal(ed); merr == nil {
			_ = h.Store.UpdateMeetingDetails(ctx, m.ID, det, "su", "waktu meeting offline diperbarui — approval waktu disegarkan")
		}
		log.Printf("[VENUE] meeting #%d approval #%d: waktu diperbarui ke %s — notif SU ulang", m.ID, ap, nowPresented)
		if h.maybeNotifyGroupApproval(ctx, ap) {
			return
		}
		h.notifySUTimeApproval(ctx, m, ed, ap)
		return
	}

	agentID := firstNonEmptyStr(m.AgentID, "pa_communicator")
	facts, _ := json.Marshal([]string{})
	apID, err := h.Store.CreateApproval(ctx, model.Approval{
		ConversationID: m.ConversationID, AgentID: agentID, ContactID: m.ContactID,
		TargetChat: externalChat, UserText: "[meeting offline: kesepakatan waktu]",
		ResponseText: heldMsg, ApprovalReason: "Persetujuan waktu meeting offline (lokasi menyusul)",
		NewFacts: facts,
	})
	if err != nil {
		log.Printf("[VENUE] buat approval waktu meeting #%d gagal: %v", m.ID, err)
		return
	}
	ap := apID
	h.sendAndRecord(ctx, nil, model.OutboundMessage{
		ConversationID: m.ConversationID, ContactID: m.ContactID, AgentID: agentID,
		TargetChat: externalChat, Kind: "agent_reply", Text: heldMsg, Status: "held", ApprovalID: &ap,
	})
	if err := h.Store.LinkMeetingApproval(ctx, m.ID, apID, "su", "kesepakatan waktu diajukan ke SU (lokasi menyusul)"); err != nil {
		log.Printf("[VENUE] tautkan meeting #%d ke approval #%d gagal: %v", m.ID, apID, err)
	}
	// Catat waktu yang diajukan agar renegosiasi waktu berikutnya bisa terdeteksi.
	ed.TimePresentedAt = nowPresented
	if det, merr := json.Marshal(ed); merr == nil {
		_ = h.Store.UpdateMeetingDetails(ctx, m.ID, det, "su", "tandai waktu yang diajukan ke SU")
	}
	log.Printf("[VENUE] meeting #%d waktu diajukan ke SU (approval #%d) — venue menyusul", m.ID, apID)
	if h.maybeNotifyGroupApproval(ctx, apID) {
		return
	}
	h.notifySUTimeApproval(ctx, m, ed, apID)
}

// suApprovalTarget mengembalikan nomor SU tujuan notifikasi approval.
func (h *Handler) suApprovalTarget(det meetingDetails) string {
	if p := adminNormalizePhone(det.InitiatedBy); p != "" && phoneMatchesAny(p, h.SUPhone, h.SUPhones) {
		return p
	}
	return h.SUPhone
}

// notifySUTimeApproval mengirim ringkasan WAKTU (siapa + waktu + topik) ke SU untuk
// persetujuan tahap pertama meeting offline. Belum ada lokasi (venue dikoordinasikan
// setelah SU setuju). TIDAK menampilkan biaya/estimasi harga (kebijakan).
func (h *Handler) notifySUTimeApproval(ctx context.Context, m *model.MeetingRequest, ed meetingDetails, apID int64) {
	if h.SUPhone == "" {
		return
	}
	suTarget := h.suApprovalTarget(ed)
	who := firstNonEmptyStr(ed.AttendeeName, m.ExternalName, "Pihak eksternal")
	if c := strings.TrimSpace(m.ExternalCompany); c != "" {
		who += " (" + c + ")"
	}
	title := firstNonEmptyStr(ed.Title, m.Topic, "(tanpa topik)")
	when := "(waktu belum pasti)"
	if m.ProposedDatetime != nil {
		when = formatWIBLong(*m.ProposedDatetime)
	}
	header := "🔔 *Persetujuan waktu meeting OFFLINE diperlukan*"
	coordNote := "Setelah Anda setujui, Bu Nova akan dikoordinasikan untuk memastikan lokasi, lalu " +
		"meeting difinalisasi otomatis (tanpa persetujuan lagi)."
	if rf := strings.TrimSpace(ed.RescheduleFrom); rf != "" {
		header = "🔔 *Persetujuan reschedule (waktu) meeting OFFLINE diperlukan*"
		coordNote = "Setelah Anda setujui, Bu Nova akan dikoordinasikan ulang untuk lokasi (booking " +
			"venue & waktu lama akan dibatalkan), lalu jadwal baru difinalisasi otomatis."
		if oldAt, perr := time.Parse(time.RFC3339, rf); perr == nil {
			when = formatWIBLong(oldAt) + " → " + when
		}
	}
	body := fmt.Sprintf("%s\n"+
		"Dengan: %s\n🗓️ %s\nTopik: %s\n\n"+
		"%s\n\nBalas *SETUJU* untuk menyetujui waktu, atau *TOLAK* untuk batal.",
		header, who, when, title, coordNote)
	ap := apID
	h.sendAndRecord(ctx, func() error { return h.Waha.SendText(suTarget, body) },
		model.OutboundMessage{
			ConversationID: m.ConversationID, ContactID: m.ContactID, TargetChat: suTarget,
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
				"Balas *SETUJU* untuk mengonfirmasi, atau *TOLAK* untuk membatalkan. "+
				"(Saya tidak membuat permintaan baru agar tidak terjadi jadwal ganda.)",
				who, when)
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

	// Meeting grup (Fase 1): tahan notifikasi SU sampai SEMUA peserta setuju.
	if h.maybeNotifyGroupApproval(ctx, id) {
		return
	}
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
	// Sekaligus resolve nomor SU tujuan dari inisiator meeting (SU multi-nomor).
	slotNote := ""
	suTarget := h.SUPhone
	if reply.Meeting != nil {
		if mr, merr := h.Store.MeetingByApproval(ctx, id); merr == nil && mr != nil {
			var md meetingDetails
			_ = json.Unmarshal(mr.Details, &md)
			suTarget = h.suApprovalTarget(md)
			if mr.ProposedDatetime != nil && strings.TrimSpace(md.RescheduleFrom) != "" {
				dur := md.DurationMinutes
				if dur <= 0 {
					dur = 60
				}
				slotNote = h.freeSlotSuggestion(ctx, *mr.ProposedDatetime, dur)
			}
		}
	}

	msg := fmt.Sprintf("%s \n%s%s\nBalas *SETUJU* untuk konfirmasi, atau *TOLAK* untuk batal.",
		header, body, slotNote)
	apID := id
	h.sendAndRecord(ctx, func() error { return h.Waha.SendText(suTarget, msg) },
		model.OutboundMessage{
			ConversationID: convID, ContactID: cidPtr(contact), TargetChat: suTarget,
			Kind: "approval_notify", Text: msg, ApprovalID: &apID,
		})
}

// maybeNotifyGroupApproval menggerbang notifikasi persetujuan untuk MEETING GRUP.
//
// Perilaku grup:
//   - Bila belum semua peserta menyepakati waktu → TAHAN (tidak ada pesan ke SU); peserta
//     yang sudah setuju tetap menerima ack interim di jalur pemanggil. Status tetap pending.
//   - Bila peserta terakhir baru saja menyetujui → kirim SATU notifikasi gabungan ke SU
//     (dijaga anti-dobel via MarkGroupApprovalNotified).
func (h *Handler) maybeNotifyGroupApproval(ctx context.Context, approvalID int64) bool {
	if h.Store == nil {
		return false
	}
	m, err := h.Store.MeetingByApproval(ctx, approvalID)
	if err != nil || m == nil {
		return false
	}
	var det meetingDetails
	_ = json.Unmarshal(m.Details, &det)
	if strings.TrimSpace(det.GroupID) == "" || det.GroupSize < 2 {
		return false // meeting solo → jalur lama
	}

	agreed, err := h.Store.GroupAgreedCount(ctx, det.GroupID)
	if err != nil {
		log.Printf("[GROUP] hitung peserta setuju grup %s gagal: %v — fallback notif per-peserta", det.GroupID, err)
		return false
	}
	if agreed < det.GroupSize {
		log.Printf("[GROUP] %s: %d/%d peserta setuju — tahan notifikasi SU (menunggu peserta lain)", det.GroupID, agreed, det.GroupSize)
		return true // ditangani (ditahan) — jangan notif per-peserta
	}

	// Semua peserta sudah MERESPONS.
	meetings, merr := h.Store.MeetingsByGroup(ctx, det.GroupID)
	if merr != nil || len(meetings) == 0 {
		log.Printf("[GROUP] ambil daftar peserta grup %s gagal: %v — fallback notif per-peserta", det.GroupID, merr)
		return false
	}
	converge := groupTimesConverge(meetings)

	// Jaga agar hanya SATU notifikasi terminal terkirim (konsolidasi ATAU divergensi).
	first, err := h.Store.MarkGroupApprovalNotified(ctx, det.GroupID)
	if err != nil {
		log.Printf("[GROUP] tandai notifikasi grup %s gagal: %v — fallback notif per-peserta", det.GroupID, err)
		return false
	}
	if !first {
		log.Printf("[GROUP] %s: notifikasi gabungan sudah dikirim peserta lain — lewati", det.GroupID)
		return true
	}
	if converge {
		h.notifySUGroupApproval(ctx, det.GroupID, approvalID)
	} else {
		log.Printf("[GROUP] %s: peserta menyepakati waktu BERBEDA — laporkan divergensi ke SU (bukan konsolidasi)", det.GroupID)
		h.notifySUGroupDivergence(ctx, det.GroupID)
	}
	return true
}

// groupTimesConverge melaporkan apakah SELURUH peserta grup berbagi SATU waktu usulan yang sama
// (syarat sah untuk konsolidasi grup). false bila ada peserta tanpa waktu atau waktunya berbeda —
// grup tidak boleh dianggap "sepakat" bila anggotanya menyepakati jam/tanggal yang berlainan.
func groupTimesConverge(meetings []*model.MeetingRequest) bool {
	var anchor *time.Time
	for _, m := range meetings {
		if m.ProposedDatetime == nil {
			return false
		}
		if anchor == nil {
			anchor = m.ProposedDatetime
			continue
		}
		if !m.ProposedDatetime.Equal(*anchor) {
			return false
		}
	}
	return anchor != nil
}

// notifySUGroupApproval mengirim SATU ringkasan gabungan ke SU untuk seluruh peserta grup
// yang sudah menyepakati waktu. SU cukup membalas SETUJU sekali (id mana pun dari peserta
// grup); DecideApproval akan meng-cascade ke seluruh peserta.
func (h *Handler) notifySUGroupApproval(ctx context.Context, groupID string, primaryApprovalID int64) {
	if h.SUPhone == "" {
		return
	}
	meetings, err := h.Store.MeetingsByGroup(ctx, groupID)
	if err != nil || len(meetings) == 0 {
		log.Printf("[GROUP] ambil daftar peserta grup %s gagal: %v", groupID, err)
		return
	}
	offline := false
	topic := ""
	initiatedBy := ""
	var lines []string
	for i, m := range meetings {
		var d meetingDetails
		_ = json.Unmarshal(m.Details, &d)
		if d.VenueCoordination {
			offline = true
		}
		if topic == "" {
			topic = firstNonEmptyStr(d.Title, m.Topic)
		}
		if initiatedBy == "" {
			initiatedBy = strings.TrimSpace(d.InitiatedBy)
		}
		who := firstNonEmptyStr(d.AttendeeName, m.ExternalName, "Peserta")
		if c := strings.TrimSpace(m.ExternalCompany); c != "" {
			who += " (" + c + ")"
		}
		when := "(waktu belum pasti)"
		if m.ProposedDatetime != nil {
			when = formatWIBLong(*m.ProposedDatetime)
		}
		lines = append(lines, fmt.Sprintf("%d. %s — 🗓️ %s", i+1, who, when))
	}
	// Bila menyusut jadi 1 peserta (mis. setelah split), pakai kata-kata solo, bukan "grup".
	solo := len(meetings) == 1
	header := "🔔 *Konfirmasi meeting grup diperlukan*"
	coordNote := ""
	if offline {
		if solo {
			header = "🔔 *Persetujuan waktu meeting (OFFLINE) diperlukan*"
		} else {
			header = "🔔 *Persetujuan waktu meeting grup (OFFLINE) diperlukan*"
		}
		coordNote = "\nSetelah Anda setujui, Bu Nova dikoordinasikan untuk lokasi, lalu meeting difinalisasi otomatis."
		if !solo {
			coordNote = "\nSetelah Anda setujui, Bu Nova dikoordinasikan untuk lokasi (satu kali untuk semua " +
				"peserta), lalu meeting difinalisasi otomatis."
		}
	} else if solo {
		header = "🔔 *Konfirmasi meeting diperlukan*"
	}
	if topic == "" {
		topic = "(tanpa topik)"
	}
	lead := fmt.Sprintf("Seluruh %d peserta telah menyepakati waktu", len(meetings))
	if solo {
		lead = "Peserta telah menyepakati waktu"
	}
	tail := "Balas *SETUJU* untuk menyetujui SELURUH peserta sekaligus, atau *TOLAK* untuk batal."
	if solo {
		tail = "Balas *SETUJU* untuk konfirmasi, atau *TOLAK* untuk batal."
	}
	body := fmt.Sprintf("%s\n%s:\n\n%s\n\nTopik: %s%s\n\n"+tail,
		header, lead, strings.Join(lines, "\n"), topic, coordNote)

	ap := primaryApprovalID
	convID := ""
	if len(meetings) > 0 {
		convID = meetings[0].ConversationID
	}
	suTarget := h.suApprovalTarget(meetingDetails{InitiatedBy: initiatedBy})
	h.sendAndRecord(ctx, func() error { return h.Waha.SendText(suTarget, body) },
		model.OutboundMessage{
			ConversationID: convID, TargetChat: suTarget,
			Kind: "approval_notify", Text: body, ApprovalID: &ap,
		})
	log.Printf("[GROUP] notifikasi gabungan grup %s (%d peserta) terkirim ke SU %s (primary approval #%d)", groupID, len(meetings), suTarget, primaryApprovalID)
}

// notifySUGroupDivergence melaporkan ke SU 
func (h *Handler) notifySUGroupDivergence(ctx context.Context, groupID string) {
	if h.SUPhone == "" {
		return
	}
	meetings, err := h.Store.MeetingsByGroup(ctx, groupID)
	if err != nil || len(meetings) == 0 {
		log.Printf("[GROUP] ambil daftar peserta grup %s (divergensi) gagal: %v", groupID, err)
		return
	}
	topic := ""
	var lines []string
	for i, m := range meetings {
		var d meetingDetails
		_ = json.Unmarshal(m.Details, &d)
		if topic == "" {
			topic = firstNonEmptyStr(d.Title, m.Topic)
		}
		who := firstNonEmptyStr(d.AttendeeName, m.ExternalName, "Peserta")
		if c := strings.TrimSpace(m.ExternalCompany); c != "" {
			who += " (" + c + ")"
		}
		when := "(waktu belum pasti)"
		if m.ProposedDatetime != nil {
			when = formatWIBLong(*m.ProposedDatetime)
		}
		lines = append(lines, fmt.Sprintf("%d. %s — 🗓️ %s", i+1, who, when))
	}
	if topic == "" {
		topic = "(tanpa topik)"
	}
	var b strings.Builder
	b.WriteString("⚠️ *Peserta meeting grup menyepakati waktu BERBEDA*\n")
	b.WriteString("Sebuah meeting grup seharusnya satu waktu untuk semua, namun peserta menyepakati:\n\n")
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n\nTopik: " + topic)
	b.WriteString("\n\nPilihan Anda:\n")
	b.WriteString("(A) *Samakan semua* ke satu waktu — balas: \"ubah meeting grup ke <tanggal & jam>\".\n")
	b.WriteString("(B) *Pisahkan peserta yang berbeda* (peserta lain tetap) — balas: " +
		"\"jadwalkan <nama> terpisah di <tanggal & jam>\" atau \"keluarkan <nama> dari meeting grup\".")
	h.notifySU(b.String())
	log.Printf("[GROUP] divergensi negosiasi awal grup %s (%d peserta) dilaporkan ke SU", groupID, len(meetings))
}

// freeSlotSuggestion merangkai daftar slot kosong kalender Pak Sudianto (mailbox PA)
// pada hari `day`, dalam jam kerja 08.00–18.00 WIB, untuk durasi `durationMin`.
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
	// Meeting grup : bila approval ini tertaut meeting yang bagian dari grup,
	if m, merr := h.Store.MeetingByApproval(ctx, id); merr == nil && m != nil {
		var det meetingDetails
		_ = json.Unmarshal(m.Details, &det)
		if strings.TrimSpace(det.GroupID) != "" && det.GroupSize >= 2 {
			return h.decideGroupApproval(ctx, id, approve, det.GroupID)
		}
	}

	ap, sendFailed, err := h.applyApprovalDecision(ctx, id, approve)
	if errors.Is(err, db.ErrApprovalNotFound) {
		return "Permintaan tidak ditemukan atau sudah diputuskan.", err
	}
	if err != nil && !sendFailed {
		log.Printf("[APPROVAL] decide gagal #%d: %v", id, err)
		return "Maaf, gagal memproses permintaan.", err
	}
	if !approve {
		return "❌ Ditolak. Pesan tidak dikirim.", nil
	}
	if sendFailed {
		return fmt.Sprintf("⚠️ Disetujui, tetapi pesan gagal terkirim: %v", err), err
	}
	_ = ap

	// Alur meeting offline (venue-coordinated): approval ini adalah PERSETUJUAN WAKTU.
	// Jangan finalisasi sekarang — koordinasikan lokasi ke Bu Nova dulu; finalisasi
	// otomatis setelah venue pasti (confirmVenue → finalizeOfflineMeeting), TANPA
	// persetujuan SU kedua. Kasus langka venue sudah pasti lebih dulu → finalisasi kini.
	if m, merr := h.Store.MeetingByApproval(ctx, id); merr == nil && m != nil {
		var det meetingDetails
		_ = json.Unmarshal(m.Details, &det)
		if det.VenueCoordination {
			if !det.VenueConfirmed {
				if strings.TrimSpace(det.RescheduleFrom) != "" || det.ReschedulePending {
					go h.dispatchVenueRecoordination(m.ID, det, m.ProposedDatetime)
				} else {
					go h.dispatchVenueCoordination(m.ID, det, m.ProposedDatetime)
				}
				log.Printf("[APPROVAL] #%d (meeting #%d) waktu disetujui — koordinasi venue ke Bu Nova", id, m.ID)
				return "✅ Disetujui — waktu dikonfirmasi. Bu Nova sedang dikoordinasikan " +
					"untuk lokasi; konfirmasi final (entri kalender + pesan ke pihak eksternal) akan menyusul " +
					"otomatis setelah lokasi pasti." + inviteDirectionNote(m.RequestedVia), nil
			}
			go h.finalizeOfflineMeeting(m.ID)
			return "✅ Disetujui — lokasi sudah pasti, meeting offline sedang difinalisasi.", nil
		}
	}

	// jadwalkan otomatis (Calendar event + RSVP email) bila ada meeting
	// tertaut dengan datetime valid
	schedMsg := h.scheduleApprovedMeeting(ctx, id)

	return "✅ Disetujui. Pesan telah dikirim." + schedMsg, nil
}

// applyApprovalDecision menerapkan keputusan approve/reject untuk SATU approval: menandai
// approval, dan (saat approve) mengirim pesan tertahan ke pihak eksternal + menulis memori
// + menandai meeting tertaut approved.
func (h *Handler) applyApprovalDecision(ctx context.Context, id int64, approve bool) (*model.Approval, bool, error) {
	status := "rejected"
	if approve {
		status = "approved"
	}
	ap, err := h.Store.DecideApproval(ctx, id, status)
	if err != nil {
		if !errors.Is(err, db.ErrApprovalNotFound) {
			log.Printf("[APPROVAL] decide gagal #%d: %v", id, err)
		}
		return nil, false, err
	}

	if !approve {
		log.Printf("[APPROVAL] #%d DITOLAK", id)
		if uerr := h.Store.UpdateMeetingStatusByApproval(ctx, id, "rejected", "su", "ditolak via approval gate"); uerr != nil {
			log.Printf("[MEETING] update rejected approval #%d gagal: %v", id, uerr)
		}
		return ap, false, nil
	}

	// Disetujui: kirim pesan tertahan ke pihak eksternal.
	sendErr := h.Waha.SendToChat(ap.TargetChat, ap.ResponseText)
	apID := id
	h.sendAndRecord(ctx, nil, model.OutboundMessage{
		ConversationID: ap.ConversationID, ContactID: ap.ContactID, AgentID: ap.AgentID,
		TargetChat: ap.TargetChat, Kind: "agent_reply", Text: ap.ResponseText,
		Status: statusOf(sendErr), ApprovalID: &apID, ErrorText: errText(sendErr),
	})
	if sendErr != nil {
		log.Printf("[ERROR] kirim pesan approved #%d gagal: %v", id, sendErr)
		return ap, true, sendErr
	}
	if h.Memory != nil {
		var facts []string
		_ = json.Unmarshal(ap.NewFacts, &facts)
		contact := &model.Contact{}
		if ap.ContactID != nil {
			contact.ID = *ap.ContactID
		}
		if werr := h.Memory.Write(ctx, ap.ConversationID, contact, ap.AgentID, ap.UserText, ap.ResponseText, facts); werr != nil {
			log.Printf("[MEMORY] write approved #%d gagal: %v", id, werr)
		}
	}
	if uerr := h.Store.UpdateMeetingStatusByApproval(ctx, id, "approved", "su", "disetujui via approval gate"); uerr != nil {
		log.Printf("[MEETING] update approved approval #%d gagal: %v", id, uerr)
	}
	log.Printf("[APPROVAL] #%d DISETUJUI, pesan terkirim ke %s", id, ap.TargetChat)
	return ap, false, nil
}

// decideGroupApproval menerapkan SATU keputusan SU ke SELURUH peserta meeting grup (Fase 1).
// Approve: setujui approval tiap peserta (kirim pesan tertahan masing-masing), lalu aksi
// pasca-approve dikonsolidasikan — offline: SATU koordinasi venue ke Bu Nova untuk semua;
// online: jadwalkan tiap peserta (event+undangan masing-masing). Reject: batalkan semua.
func (h *Handler) decideGroupApproval(ctx context.Context, primaryID int64, approve bool, groupID string) (string, error) {
	// (1) Terapkan keputusan ke peserta pertama (yang id-nya dibalas SU).
	_, primFailed, err := h.applyApprovalDecision(ctx, primaryID, approve)
	if errors.Is(err, db.ErrApprovalNotFound) {
		return "Permintaan tidak ditemukan atau sudah diputuskan.", err
	}
	if err != nil && !primFailed {
		return "Maaf, gagal memproses permintaan meeting grup.", err
	}

	// (2) Terapkan keputusan yang SAMA ke peserta grup lain (masing-masing punya approval).
	done := 1
	meetings, gerr := h.Store.MeetingsByGroup(ctx, groupID)
	if gerr != nil {
		log.Printf("[GROUP] ambil peserta grup %s gagal: %v", groupID, gerr)
	}
	for _, sib := range meetings {
		if sib.ApprovalID == nil || *sib.ApprovalID == primaryID {
			continue
		}
		if _, _, cerr := h.applyApprovalDecision(ctx, *sib.ApprovalID, approve); cerr != nil {
			if !errors.Is(cerr, db.ErrApprovalNotFound) {
				log.Printf("[GROUP] terapkan keputusan approval #%d (grup %s) gagal: %v", *sib.ApprovalID, groupID, cerr)
			}
			continue
		}
		done++
	}

	if !approve {
		log.Printf("[GROUP] %s DITOLAK — %d peserta dibatalkan", groupID, done)
		return fmt.Sprintf("❌ Meeting grup dibatalkan. %d peserta telah diberi tahu.", done), nil
	}

	// (3) Aksi pasca-approve dikonsolidasikan. Baca ulang status peserta (kini approved).
	meetings, _ = h.Store.MeetingsByGroup(ctx, groupID)
	if groupIsOffline(meetings) {
		// Satu koordinasi venue untuk SELURUH peserta grup.
		go h.dispatchGroupVenueCoordination(groupID)
		via := ""
		if len(meetings) > 0 {
			via = meetings[0].RequestedVia
		}
		log.Printf("[GROUP] %s disetujui (%d peserta) — koordinasi venue tunggal ke Bu Nova", groupID, done)
		return fmt.Sprintf("✅ Meeting grup disetujui — waktu dikonfirmasi untuk %d peserta. Bu Nova "+
			"dikoordinasikan SEKALI untuk lokasi; finalisasi otomatis setelah lokasi pasti.%s",
			done, inviteDirectionNote(via)), nil
	}
	// Online: jadwalkan tiap peserta (masing-masing perlu event kalender + undangan sendiri).
	var sb strings.Builder
	for _, mm := range meetings {
		if mm.ApprovalID == nil {
			continue
		}
		sb.WriteString(h.scheduleApprovedMeeting(ctx, *mm.ApprovalID))
	}
	log.Printf("[GROUP] %s disetujui & dijadwalkan (%d peserta)", groupID, done)
	return fmt.Sprintf("✅ Meeting grup disetujui & dijadwalkan untuk %d peserta.%s", done, sb.String()), nil
}

// groupIsOffline true bila ada peserta grup yang butuh koordinasi venue (meeting offline).
func groupIsOffline(meetings []*model.MeetingRequest) bool {
	for _, m := range meetings {
		var d meetingDetails
		_ = json.Unmarshal(m.Details, &d)
		if d.VenueCoordination {
			return true
		}
	}
	return false
}

// dispatchGroupVenueCoordination mengirim SATU permintaan koordinasi venue ke Bu Nova yang
// mencakup SELURUH peserta grup offline (satu waktu, daftar peserta, preferensi lokasi
// tergabung). Saat Bu Nova CONFIRM_VENUE, confirmVenue menerapkan lokasi ke semua peserta.
func (h *Handler) dispatchGroupVenueCoordination(groupID string) {
	lctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	meetings, err := h.Store.MeetingsByGroup(lctx, groupID)
	if err != nil || len(meetings) == 0 {
		cancel()
		log.Printf("[VENUE-COORD] grup %s: ambil peserta gagal: %v", groupID, err)
		return
	}
	// Ambil meeting offline sebagai referensi (untuk waktu & preferensi lokasi).
	var ref *model.MeetingRequest
	var refDet meetingDetails
	var whos []string
	prefs := map[string]struct{}{}
	var when *time.Time
	topic := ""
	for _, m := range meetings {
		var d meetingDetails
		_ = json.Unmarshal(m.Details, &d)
		if !d.VenueCoordination {
			continue
		}
		if ref == nil {
			ref, refDet = m, d
			when = m.ProposedDatetime
		}
		whos = append(whos, firstNonEmptyStr(d.AttendeeName, m.ExternalName, "peserta"))
		if p := strings.TrimSpace(m.Venue); p != "" {
			prefs[p] = struct{}{}
		}
		if topic == "" {
			topic = firstNonEmptyStr(d.Title, m.Topic)
		}
	}
	if ref == nil {
		cancel()
		log.Printf("[VENUE-COORD] grup %s: tak ada peserta offline — koordinasi dilewati", groupID)
		return
	}
	recs := h.buildVenueRecommendations(lctx, ref.ExternalName)
	cancel()

	var b strings.Builder
	b.WriteString("Pak Sudianto sudah menyetujui SATU pertemuan tatap muka GRUP dengan " +
		strconv.Itoa(len(whos)) + " peserta: " + strings.Join(whos, ", ") + ".")
	if when != nil {
		b.WriteString(" Waktu: " + formatWIBLong(*when) + ".")
	}
	if topic != "" {
		b.WriteString(" Topik: " + topic + ".")
	}
	if len(prefs) > 0 {
		list := make([]string, 0, len(prefs))
		for p := range prefs {
			list = append(list, p)
		}
		b.WriteString(" Preferensi area/lokasi: " + strings.Join(list, "; ") + ".")
	}
	b.WriteString(" Cari SATU lokasi yang menampung semua peserta.")
	b.WriteString(recs)
	b.WriteString(venueCompletionRule)
	_ = refDet
	h.dispatchNovaVenue(ref.ID, b.String())
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

	// Cabang RESCHEDULE: meeting sudah punya event O365 & menyimpan jadwal lama →
	// PATCH event yang ada + kirim email perubahan jadwal (bukan membuat event baru).
	if det.EventID != "" && strings.TrimSpace(det.RescheduleFrom) != "" {
		return h.finalizeReschedule(ctx, m, det, title, duration)
	}

	return h.createEventAndInvite(ctx, m, det, title, duration)
}

// createEventAndInvite membuat event kalender baru (online → Teams joinUrl otomatis),
// mengirim RSVP email bila email diketahui, menandai meeting 'scheduled', lalu menjadwalkan
// pengingat.
func (h *Handler) createEventAndInvite(ctx context.Context, m *model.MeetingRequest, det meetingDetails, title string, duration int) string {
	// KEBIJAKAN BARU: untuk meeting yang diinisiasi pihak EKSTERNAL, PIHAK EKSTERNAL yang
	// mengirim undangan/link meeting ke pa@hypernet.co.id — PA tidak lagi mengundang mereka.
	externalHosted := m.RequestedVia == "external"
	isOnline := strings.TrimSpace(m.Venue) == ""
	dt := m.ProposedDatetime.Format(time.RFC3339)
	isGroup := strings.TrimSpace(det.GroupID) != "" && det.GroupSize >= 2

	// Meeting GRUP: seluruh peserta berbagi SATU event kalender di kalender SU (bukan N
	// event terpisah).
	if isGroup {
		if shared := h.sharedGroupEvent(ctx, det.GroupID, m.ID); shared != nil {
			return h.attachSharedGroupEvent(ctx, m, det, title, duration, *shared, externalHosted)
		}
	}

	// Fallback email peserta: bila undangan (det.AttendeeEmail) belum terisi tapi email
	// sudah tercatat di kontak (mis. kontak memberi email SEBELUM venue dikonfirmasi),
	// ambil dari contacts agar undangan tetap terkirim. Hanya solo & non-eksternal.
	if !isGroup && !externalHosted && strings.TrimSpace(det.AttendeeEmail) == "" && m.ContactID != nil {
		if c, err := h.Store.ContactByID(ctx, int64(*m.ContactID)); err == nil && c != nil && strings.TrimSpace(c.Email) != "" {
			det.AttendeeEmail = strings.TrimSpace(c.Email)
			log.Printf("[SCHEDULE] meeting #%d email peserta diambil dari kontak: %s", m.ID, det.AttendeeEmail)
		}
	}

	var attendees []string
	if isGroup {
		// Event tunggal grup mengundang SEMUA peserta sekaligus.
		attendees = h.groupAttendeeEmails(ctx, det.GroupID)
	} else if !externalHosted && det.AttendeeEmail != "" {
		attendees = append(attendees, det.AttendeeEmail)
	}
	createOnline := isOnline && !externalHosted // eksternal: link disiapkan pihak eksternal

	// Daftar nama peserta untuk deskripsi event kalender SU (grup → semua nama; solo →
	// satu nama). Membantu SU melihat SIAPA yang ikut langsung dari kalendernya.
	var names []string
	if isGroup {
		names = h.groupParticipantNames(ctx, det.GroupID)
	} else if n := firstNonEmptyStr(det.AttendeeName, m.ExternalName); n != "" {
		names = []string{n}
	}

	// 1) Buat Calendar event (online non-eksternal → dapat Teams joinUrl otomatis).
	ev, err := h.Services.CreateEvent(ctx, services.CreateEventReq{
		Title: title, Datetime: dt, DurationMinutes: duration, Venue: m.Venue,
		Attendees: attendees, IsOnline: createOnline, Body: buildEventBody(names),
	})
	if err != nil {
		log.Printf("[SCHEDULE] buat event meeting #%d gagal: %v", m.ID, err)
		return "\n⚠️ Gagal membuat event kalender — silakan jadwalkan manual."
	}
	det.EventID = ev.EventID
	det.CalendarLink = ev.CalendarLink
	det.TeamsLink = ev.OnlineMeetingURL
	log.Printf("[SCHEDULE] meeting #%d event=%s teams=%s external=%v", m.ID, ev.EventID, ev.OnlineMeetingURL, externalHosted)

	// 2) Undangan email.
	emailNote := ""
	switch {
	case externalHosted:
		// Pihak eksternal yang mengirim undangan/link ke pa@hypernet.co.id (bukan PA).
		emailNote = " (pihak eksternal diminta mengirim undangan/link meeting ke pa@hypernet.co.id)"
	case det.AttendeeEmail != "":
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
	default:
		emailNote = " (email kontak tidak diketahui, undangan tidak dikirim)"
	}

	// 3) Tandai meeting scheduled + simpan link.
	newDetails, _ := json.Marshal(det)
	if err := h.Store.ScheduleMeeting(ctx, m.ID, newDetails, "su", "dijadwalkan otomatis via Fase 9"); err != nil {
		log.Printf("[SCHEDULE] tandai scheduled meeting #%d gagal: %v", m.ID, err)
	}
	// Pengingat otomatis beberapa menit sebelum meeting mulai (SU + pihak eksternal).
	h.scheduleMeetingReminder(ctx, m, det)
	return fmt.Sprintf("\n📅 Meeting dijadwalkan (event kalender dibuat)%s.", emailNote)
}

// sharedGroupEvent mengembalikan detail event kalender yang SUDAH dibuat oleh peserta
// grup lain (EventID + link), atau nil bila belum ada.
func (h *Handler) sharedGroupEvent(ctx context.Context, groupID string, excludeID int64) *meetingDetails {
	meetings, err := h.Store.MeetingsByGroup(ctx, groupID)
	if err != nil {
		return nil
	}
	for _, mm := range meetings {
		if mm == nil || mm.ID == excludeID {
			continue
		}
		var d meetingDetails
		_ = json.Unmarshal(mm.Details, &d)
		if strings.TrimSpace(d.EventID) != "" {
			return &d
		}
	}
	return nil
}

// groupParticipantNames mengumpulkan NAMA semua peserta grup (unik, non-kosong) yang belum
// dibatalkan — dipakai untuk deskripsi event kalender SU agar terlihat siapa saja yang ikut.
func (h *Handler) groupParticipantNames(ctx context.Context, groupID string) []string {
	meetings, err := h.Store.MeetingsByGroup(ctx, groupID)
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, mm := range meetings {
		if mm == nil || mm.Status == "cancelled" || mm.Status == "rejected" {
			continue
		}
		var d meetingDetails
		_ = json.Unmarshal(mm.Details, &d)
		n := firstNonEmptyStr(d.AttendeeName, mm.ExternalName)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

// buildEventBody menyusun deskripsi (HTML) event kalender berisi daftar peserta yang ikut.
// Kosong bila tak ada nama.
func buildEventBody(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "Peserta yang diundang: " + html.EscapeString(joinNamesID(names)) + "."
}

func (h *Handler) groupAttendeeEmails(ctx context.Context, groupID string) []string {
	meetings, err := h.Store.MeetingsByGroup(ctx, groupID)
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, mm := range meetings {
		if mm == nil || mm.RequestedVia == "external" {
			continue
		}
		var d meetingDetails
		_ = json.Unmarshal(mm.Details, &d)
		e := strings.TrimSpace(d.AttendeeEmail)
		if e == "" {
			continue
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}

// attachSharedGroupEvent memakai ULANG event kalender grup yang sudah dibuat peserta lain:
// salin EventID + link ke detail peserta ini, kirim RSVP personalnya, tandai scheduled, lalu
// jadwalkan pengingat (yang sendiri sudah dedup grup).
func (h *Handler) attachSharedGroupEvent(ctx context.Context, m *model.MeetingRequest, det meetingDetails, title string, duration int, shared meetingDetails, externalHosted bool) string {
	det.EventID = shared.EventID
	det.CalendarLink = shared.CalendarLink
	det.TeamsLink = shared.TeamsLink
	dt := m.ProposedDatetime.Format(time.RFC3339)

	emailNote := ""
	switch {
	case externalHosted:
		emailNote = " (pihak eksternal diminta mengirim undangan/link meeting ke pa@hypernet.co.id)"
	case det.AttendeeEmail != "":
		if err := h.Services.SendRSVP(ctx, services.RSVPReq{
			To: det.AttendeeEmail, ToName: firstNonEmptyStr(det.AttendeeName, m.ExternalName, det.AttendeeEmail),
			Title: title, Datetime: dt, DurationMinutes: duration, Venue: m.Venue,
			CalendarLink: shared.CalendarLink, TeamsLink: shared.TeamsLink,
		}); err != nil {
			log.Printf("[SCHEDULE] kirim RSVP (event grup) meeting #%d gagal: %v", m.ID, err)
			emailNote = " (email undangan gagal terkirim)"
		} else {
			log.Printf("[SCHEDULE] RSVP terkirim ke %s meeting #%d (event grup %s dipakai ulang)", det.AttendeeEmail, m.ID, shared.EventID)
		}
	default:
		emailNote = " (email kontak tidak diketahui, undangan tidak dikirim)"
	}

	newDetails, _ := json.Marshal(det)
	if err := h.Store.ScheduleMeeting(ctx, m.ID, newDetails, "su", "dijadwalkan (event kalender grup dipakai ulang)"); err != nil {
		log.Printf("[SCHEDULE] tandai scheduled meeting #%d gagal: %v", m.ID, err)
	}
	h.scheduleMeetingReminder(ctx, m, det)
	return fmt.Sprintf("\n📅 Meeting dijadwalkan (bergabung ke event kalender grup)%s.", emailNote)
}

// finalizeOfflineMeeting menyelesaikan meeting offline setelah lokasi dikonfirmasi —
// tanpa persetujuan SU kedua.
func (h *Handler) finalizeOfflineMeeting(meetingID int64) {
	if h.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	m, err := h.Store.MeetingByID(ctx, meetingID)
	if err != nil || m == nil {
		log.Printf("[VENUE-FINAL] meeting #%d tidak ditemukan: %v", meetingID, err)
		return
	}
	var det meetingDetails
	_ = json.Unmarshal(m.Details, &det)

	// Meeting grup (Fase 1): finalisasi seluruh peserta dengan SATU laporan gabungan ke SU.
	if strings.TrimSpace(det.GroupID) != "" && det.GroupSize >= 2 {
		h.finalizeOfflineGroup(det.GroupID)
		return
	}

	ok, suffix := h.finalizeOfflineMeetingCore(ctx, m)
	if !ok {
		return
	}
	who := firstNonEmptyStr(det.AttendeeName, m.ExternalName, "pihak terkait")
	h.notifySU(fmt.Sprintf("✅ Meeting offline dengan %s telah dikonfirmasi lengkap — \n\n 🗓️ %s \n📍 %s.%s",
		who, formatWIBLong(*m.ProposedDatetime), m.Venue, suffix))
	log.Printf("[VENUE-FINAL] meeting #%d difinalisasi (venue=%q)", m.ID, m.Venue)
}


func (h *Handler) finalizeOfflineGroup(groupID string) {
	if h.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	meetings, err := h.Store.MeetingsByGroup(ctx, groupID)
	if err != nil || len(meetings) == 0 {
		log.Printf("[VENUE-FINAL] grup %s: ambil peserta gagal: %v", groupID, err)
		return
	}
	var lines []string
	var when *time.Time
	venue, suffix := "", ""
	done := 0
	for _, m := range meetings {
		var d meetingDetails
		_ = json.Unmarshal(m.Details, &d)
		if !d.VenueCoordination {
			continue
		}
		ok, sfx := h.finalizeOfflineMeetingCore(ctx, m)
		if !ok {
			continue
		}
		done++
		if suffix == "" {
			suffix = sfx
		}
		if when == nil {
			when = m.ProposedDatetime
		}
		if venue == "" {
			venue = m.Venue
		}
		who := firstNonEmptyStr(d.AttendeeName, m.ExternalName, "peserta")
		lines = append(lines, "• "+who)
	}
	if done == 0 {
		log.Printf("[VENUE-FINAL] grup %s: belum ada peserta yang siap difinalisasi", groupID)
		return
	}
	whenStr := "(waktu)"
	if when != nil {
		whenStr = formatWIBLong(*when)
	}
	h.notifySU(fmt.Sprintf("✅ Meeting offline GRUP telah dikonfirmasi lengkap untuk %d peserta:\n%s\n\n🗓️ %s\n📍 %s.%s",
		done, strings.Join(lines, "\n"), whenStr, venue, suffix))
	log.Printf("[VENUE-FINAL] grup %s difinalisasi (%d peserta, venue=%q)", groupID, done, venue)
}

// inviteDirectionNote mengembalikan catatan arah undangan (SU-facing) sesuai penginisiasi
// meeting.
func inviteDirectionNote(requestedVia string) string {
	if requestedVia == "external" {
		return " Pihak eksternal akan diminta mengirim undangan/link ke pa@hypernet.co.id."
	}
	return " Undangan kalender akan kami kirimkan ke pihak eksternal."
}

// participantInviteLine menyusun kalimat arah undangan yang DIKIRIM ke pihak eksternal saat
// konfirmasi akhir meeting offline. Selaras dengan inviteDirectionNote namun dari sudut
// pandang pihak eksternal (lihat [[su-initiated-invite-direction]]):
//   - external-initiated → minta mereka kirim undangan/link ke pa@hypernet.co.id.
//   - SU-initiated + email diketahui → beri tahu undangan kalender sudah dikirim ke email mereka.
//   - SU-initiated + email belum ada → minta alamat email agar undangan bisa dikirim.
//
// Selalu diakhiri spasi agar aman disambung sebelum "Sampai jumpa di sana".
func participantInviteLine(requestedVia, attendeeEmail string) string {
	switch {
	case requestedVia == "external":
		return "Bila ada undangan/agenda meeting, mohon dikirimkan ke email kami di pa@hypernet.co.id ya. "
	case strings.TrimSpace(attendeeEmail) != "":
		return "Undangan kalender sudah kami kirimkan ke email Anda. "
	default:
		return "Bila berkenan, mohon informasikan alamat email Anda agar undangan kalender dapat kami kirimkan. "
	}
}

func (h *Handler) finalizeOfflineMeetingCore(ctx context.Context, m *model.MeetingRequest) (bool, string) {
	var det meetingDetails
	_ = json.Unmarshal(m.Details, &det)
	if !det.TimeAgreed || !det.VenueConfirmed || m.ProposedDatetime == nil {
		log.Printf("[VENUE-FINAL] meeting #%d belum siap (timeAgreed=%v venueConfirmed=%v waktu=%v) — ditahan",
			m.ID, det.TimeAgreed, det.VenueConfirmed, m.ProposedDatetime != nil)
		return false, ""
	}
	if m.Status == "scheduled" {
		log.Printf("[VENUE-FINAL] meeting #%d sudah scheduled — dilewati", m.ID)
		return false, ""
	}
	if m.Status != "approved" {
		log.Printf("[VENUE-FINAL] meeting #%d status=%s (SU belum menyetujui waktu) — finalisasi ditahan", m.ID, m.Status)
		return false, ""
	}

	title := firstNonEmptyStr(det.Title, m.Topic, "Meeting")
	duration := det.DurationMinutes
	if duration <= 0 {
		duration = 60
	}

	// 1) Konfirmasi MENYELURUH ke pihak eksternal (waktu + lokasi pasti, tanpa biaya).
	if externalChat := h.externalChatIDForMeeting(ctx, m); externalChat != "" {
		who := firstNonEmptyStr(det.AttendeeName, m.ExternalName, "")
		greet := "Halo"
		if who != "" {
			greet = "Halo " + who
		}
		extMsg := fmt.Sprintf("%s, pertemuan dengan Pak Sudianto sudah dikonfirmasi sepenuhnya:\n"+
			"🗓️ %s\n📍 %s\nTopik: %s.\n\n%sSampai jumpa di sana, terima kasih. 🙏",
			greet, formatWIBLong(*m.ProposedDatetime), m.Venue, title,
			participantInviteLine(m.RequestedVia, det.AttendeeEmail))
		agentID := firstNonEmptyStr(m.AgentID, "pa_communicator")
		h.sendAndRecord(ctx, func() error { return h.Waha.SendToChat(externalChat, extMsg) },
			model.OutboundMessage{
				ConversationID: m.ConversationID, ContactID: m.ContactID, AgentID: agentID,
				TargetChat: externalChat, Kind: "agent_reply", Text: extMsg,
			})
	}

	// 2) Event kalender + undangan email (reschedule → PATCH event yang ada).
	suffix := ""
	if h.Services != nil && h.Services.Enabled() {
		if det.EventID != "" && strings.TrimSpace(det.RescheduleFrom) != "" {
			suffix = h.finalizeReschedule(ctx, m, det, title, duration)
		} else {
			suffix = h.createEventAndInvite(ctx, m, det, title, duration)
		}
	} else {
		// Layanan kalender/email nonaktif → tetap tandai scheduled agar status konsisten.
		if serr := h.Store.ScheduleMeeting(ctx, m.ID, m.Details, "su", "dijadwalkan (layanan kalender nonaktif)"); serr != nil {
			log.Printf("[VENUE-FINAL] tandai scheduled #%d gagal: %v", m.ID, serr)
		}
		h.scheduleMeetingReminder(ctx, m, det)
		suffix = " (layanan kalender/email nonaktif)"
	}
	return true, suffix
}

// ResendMeetingRSVP mengirim ulang undangan RSVP + .ics untuk meeting yang sudah dijadwalkan
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
	// Fallback: bila undangan (det.AttendeeEmail) belum terisi tapi email sudah tercatat di
	// kontak (mis. email diberi SETELAH finalisasi, sebelum fix backfill diterapkan), ambil
	// dari contacts agar resend tetap bisa jalan. Hanya solo & non-eksternal.
	backfilledEmail := false
	if strings.TrimSpace(det.AttendeeEmail) == "" && m.RequestedVia != "external" && m.ContactID != nil {
		if c, cerr := h.Store.ContactByID(ctx, int64(*m.ContactID)); cerr == nil && c != nil && strings.TrimSpace(c.Email) != "" {
			det.AttendeeEmail = strings.TrimSpace(c.Email)
			backfilledEmail = true
			log.Printf("[RSVP-RESEND] meeting #%d email peserta diambil dari kontak: %s", meetingID, det.AttendeeEmail)
		}
	}
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
	// Simpan balik email yang di-backfill agar tercatat di details (undangan berikutnya konsisten).
	if backfilledEmail {
		newDetails, _ := json.Marshal(det)
		if serr := h.Store.ScheduleMeeting(ctx, meetingID, newDetails, "su", "email peserta di-backfill dari kontak saat resend undangan"); serr != nil {
			log.Printf("[RSVP-RESEND] simpan email meeting #%d gagal: %v", meetingID, serr)
		}
	}
	log.Printf("[RSVP-RESEND] undangan meeting #%d terkirim ulang ke %s", meetingID, det.AttendeeEmail)
	return nil
}

// finalizeReschedule menyelesaikan reschedule yang sudah disetujui SU: PATCH event O365
// yang ADA ke jadwal baru (link Teams tetap), kirim email "jadwal lama → baru"
func (h *Handler) finalizeReschedule(ctx context.Context, m *model.MeetingRequest, det meetingDetails, title string, duration int) string {
	dtNew := m.ProposedDatetime.Format(time.RFC3339)
	venue := m.Venue

	// Meeting GRUP berbagi SATU event kalender → PATCH cukup SEKALI (pada baris kanonik =
	// ID terkecil) agar MS Graph tidak mengirim beruntun notifikasi pembaruan ke semua
	// peserta.
	patchEvent := true
	if strings.TrimSpace(det.GroupID) != "" && det.GroupSize >= 2 {
		if canonical, _ := h.groupReminderCanonical(ctx, det.GroupID); canonical != 0 && m.ID != canonical {
			patchEvent = false
		}
	}
	if patchEvent {
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
	} else {
		log.Printf("[RESCHEDULE] meeting #%d bagian grup — event bersama sudah di-PATCH baris kanonik, hanya kirim email personal", m.ID)
	}

	// KEBIJAKAN BARU: meeting yang diinisiasi pihak eksternal → pihak eksternal yang
	// memperbarui undangan/link ke pa@hypernet.co.id; PA tidak mengirim email jadwal baru.
	emailNote := ""
	switch {
	case m.RequestedVia == "external":
		emailNote = " (pihak eksternal diminta memperbarui undangan/link ke pa@hypernet.co.id)"
	case det.AttendeeEmail != "":
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
	default:
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
