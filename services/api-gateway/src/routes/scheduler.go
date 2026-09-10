package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"pa-ai/api-gateway/src/db"
	"pa-ai/api-gateway/src/middleware"
	"pa-ai/api-gateway/src/model"
	"pa-ai/api-gateway/src/openclaw"
	"pa-ai/api-gateway/src/services"
)

// schedulerPoll = jeda antar pemeriksaan tugas terjadwal.
const schedulerPoll = 30 * time.Second

const schedulerPrepLead = 90 * time.Second

// StartScheduler menjalankan worker latar belakang yang memeriksa tugas terjadwal
// (scheduled_tasks) tiap menit.
func (h *Handler) StartScheduler(ctx context.Context) {
	if h.SUPhone == "" {
		log.Printf("[SCHEDULER] SU phone kosong — worker pengingat TIDAK dijalankan")
		return
	}
	log.Printf("[SCHEDULER] worker pengingat aktif (poll tiap %s, susun-awal %s, lead meeting %d menit)",
		schedulerPoll, schedulerPrepLead, h.reminderLead())

	go h.reconcileUnfinalizedOfflineMeetings(context.Background())

	middleware.WorkerHeartbeat("scheduler")
	t := time.NewTicker(schedulerPoll)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Printf("[SCHEDULER] worker pengingat berhenti")
				return
			case <-t.C:
				h.runDueTasks(context.Background())
				middleware.WorkerHeartbeat("scheduler") // Fase M5: bukti loop hidup
			}
		}
	}()
}

// runDueTasks mengambil-alih tugas yang jatuh tempo dalam jendela susun-awal
// (now()+prepLead) lalu memprosesnya MASING-MASING di goroutine terpisah. Pemrosesan
// terpisah penting karena fireTask menahan pesan sampai fire_at — bila dijalankan
// serial, penahanan akan memblokir loop poll.
func (h *Handler) runDueTasks(ctx context.Context) {
	cutoff := time.Now().Add(schedulerPrepLead)
	tasks, err := h.Store.ClaimDueTasks(ctx, 20, cutoff)
	if err != nil {
		log.Printf("[SCHEDULER] ambil tugas due gagal: %v", err)
		return
	}
	for _, task := range tasks {
		go h.fireTask(ctx, task)
	}
}

// fireTask menyusun pengingat lebih awal lalu MENAHANNYA sampai fire_at sebelum
// mengirim ke Pak Sudianto. Tugas sudah ditandai 'fired' oleh ClaimDueTasks; bila
// pengiriman gagal, status dikoreksi menjadi 'error'.
func (h *Handler) fireTask(ctx context.Context, task model.ScheduledTask) {
	// Timeout = jendela susun-awal + anggaran penyusunan LLM. Penahanan memakai
	// sebagian jendela ini (bukan menambah di atas penyusunan), jadi 5 menit lega.
	// KERJA BERAT (deep_work) beda urusan: itu memang dimaksudkan berjalan lama.
	budget := 5 * time.Minute
	if task.Kind == "deep_work" {
		budget = deepWorkBudget + 5*time.Minute
	}
	fctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// Instruksi default = pengingat statis. Untuk DIGEST KALENDER, tarik agenda hari
	// ini langsung dari kalender PA lalu minta orchestrator menyusun ringkasan + rencana.
	instruction := buildReminderInstruction(task)
	opts := pushOpts{releaseAt: task.FireAt}
	switch task.Kind {
	case "calendar_digest":
		instruction = h.buildCalendarDigestInstruction(fctx, task)
	case "deep_work":
		instruction = buildDeepWorkInstruction(task)
		// Kirim BEGITU selesai, jangan ditahan sampai fire_at: fire_at di sini
		// menandai kapan pekerjaan MULAI, bukan kapan laporannya jatuh tempo.
		opts.releaseAt = time.Now()
		// Anggaran waktu besar — inilah seluruh alasan DEFER_TASK ada.
		opts.timeout = deepWorkBudget
		// Kerja berat sering berujung pada berkas (SEND_DOCUMENT). Tanpa ini,
		// action-nya dibuang diam-diam dan Pak Sudianto hanya menerima teks yang
		// menjanjikan lampiran yang tak pernah datang.
		opts.applyActions = true
	}
	err := h.pushToOrchestrator(fctx, instruction, opts)

	// Pengingat MEETING juga dikirim ke pihak EKSTERNAL via WhatsApp (bila ada chat
	// eksternal). SU sudah dapat push di atas; ini melengkapi agar kedua pihak diingatkan.
	if task.Kind == "meeting_reminder" && task.MeetingID != nil {
		h.remindExternalForMeeting(fctx, *task.MeetingID, task.FireAt)
	}

	// Reschedule-on-fire: untuk pengingat BERULANG, jadwalkan kejadian berikutnya
	// sekarang (idempoten — ClaimDueTasks memfire tiap baris tepat sekali). INILAH
	// jaminan keandalan seri (Go, bukan LLM). Dilakukan baik kirim sukses maupun gagal:
	// kegagalan kirim sesaat tak boleh mematikan seri harian/mingguan.
	h.rearmRecurring(context.Background(), task)

	if err != nil {
		log.Printf("[SCHEDULER] tugas #%d (%s) gagal kirim: %v", task.ID, task.Kind, err)
		if e := h.Store.MarkTaskError(context.Background(), task.ID, err.Error()); e != nil {
			log.Printf("[SCHEDULER] tandai error tugas #%d gagal: %v", task.ID, e)
		}
		return
	}
	log.Printf("[SCHEDULER] tugas #%d (%s) tersampaikan ke SU", task.ID, task.Kind)
}

// rearmRecurring menyisipkan baris pending baru untuk kejadian berikutnya sebuah
// pengingat berulang. No-op untuk pengingat sekali-tembak. Kejadian berikutnya dihitung
// RELATIF terhadap now (bukan FireAt) sehingga bila scheduler sempat mati, seri langsung
// melompat ke slot masa depan berikutnya alih-alih meledak mengejar yang terlewat.
func (h *Handler) rearmRecurring(ctx context.Context, task model.ScheduledTask) {
	if task.RecurKind == "" || task.RecurKind == "none" {
		return
	}
	next, ok := nextOccurrence(task, time.Now())
	if !ok {
		log.Printf("[SCHEDULER] rekurensi #%d tak bisa dihitung (kind=%q time=%q dow=%v) — seri berhenti",
			task.ID, task.RecurKind, task.RecurTime, task.RecurDow)
		return
	}
	id, err := h.Store.CreateScheduledTask(ctx, model.ScheduledTask{
		FireAt:    next,
		Kind:      task.Kind,
		Note:      task.Note,
		CreatedBy: task.CreatedBy,
		RecurKind: task.RecurKind,
		RecurTime: task.RecurTime,
		RecurDow:  task.RecurDow,
		Label:     task.Label,
	})
	if err != nil {
		log.Printf("[SCHEDULER] jadwalkan ulang pengingat berulang #%d gagal: %v", task.ID, err)
		return
	}
	log.Printf("[SCHEDULER] pengingat berulang #%d → kejadian berikut #%d pada %s",
		task.ID, id, next.In(wibZone).Format("2006-01-02 15:04 WIB"))
}

// nextOccurrence menghitung kejadian berulang berikutnya, STRICTLY setelah `from`.
// RecurTime "HH:MM" ditafsirkan di WIB. daily → slot HH:MM berikutnya; weekly → hari
// RecurDow (0=Minggu..6=Sabtu) berikutnya pada HH:MM. ok=false bila parameter tak valid.
func nextOccurrence(task model.ScheduledTask, from time.Time) (time.Time, bool) {
	hh, mm, ok := parseHHMM(task.RecurTime)
	if !ok {
		return time.Time{}, false
	}
	base := from.In(wibZone)
	slot := func(d time.Time) time.Time {
		return time.Date(d.Year(), d.Month(), d.Day(), hh, mm, 0, 0, wibZone)
	}
	switch task.RecurKind {
	case "daily":
		cand := slot(base)
		if !cand.After(from) {
			cand = cand.AddDate(0, 0, 1)
		}
		return cand, true
	case "weekly":
		if task.RecurDow == nil || *task.RecurDow < 0 || *task.RecurDow > 6 {
			return time.Time{}, false
		}
		want := time.Weekday(*task.RecurDow)
		cand := slot(base)
		for i := 0; i < 8; i++ { // maks 8 hari cukup menemukan dow berikutnya
			if cand.Weekday() == want && cand.After(from) {
				return cand, true
			}
			cand = cand.AddDate(0, 0, 1)
		}
		return time.Time{}, false
	default:
		return time.Time{}, false
	}
}

// parseHHMM memparse "HH:MM" (24 jam). Mengembalikan ok=false bila format salah atau
// di luar rentang.
func parseHHMM(s string) (hh, mm int, ok bool) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// holdUntil menahan goroutine sampai releaseAt (menghormati pembatalan ctx). Bila
// releaseAt sudah lewat, langsung kembali. Inilah mekanisme "tahan sampai tepat
// waktu": pesan sudah disusun lebih awal, lalu ditahan di sini.
func holdUntil(ctx context.Context, releaseAt time.Time) error {
	d := time.Until(releaseAt)
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// deepWorkBudget = batas waktu SATU giliran kerja berat (DEFER_TASK).
const deepWorkBudget = 15 * time.Minute

// maxPendingDeepWork = batas tugas berat yang boleh mengantre sekaligus.
const maxPendingDeepWork = 5

// buildDeepWorkInstruction merangkai giliran lanjutan untuk pekerjaan yang ditunda.
func buildDeepWorkInstruction(task model.ScheduledTask) string {
	var b strings.Builder
	b.WriteString("[TUGAS TERTUNDA — giliran sistem, BUKAN pesan dari Pak Sudianto]\n")
	b.WriteString("Sebelumnya kamu menunda pekerjaan di bawah ini karena butuh waktu. ")
	b.WriteString("Sekarang KERJAKAN sampai tuntas. Kamu punya waktu jauh lebih lega ")
	b.WriteString("daripada giliran percakapan biasa, jadi telusuri sumbernya dengan benar.\n\n")
	b.WriteString("Pekerjaan: " + strings.TrimSpace(task.Note) + "\n\n")
	b.WriteString("Setelah selesai, isi `response` dengan LAPORAN HASILNYA untuk Pak Sudianto ")
	b.WriteString("dalam Bahasa Indonesia — langsung ke isinya, jangan menyebut bahwa ini ")
	b.WriteString("proses otomatis atau giliran sistem. Bila hasilnya lebih enak dibaca sebagai ")
	b.WriteString("berkas (tabel, laporan, presentasi, grafik), lampirkan lewat SEND_DOCUMENT.\n")
	b.WriteString("Bila ternyata masih perlu satu tahap lagi, panggil DEFER_TASK sekali lagi ")
	b.WriteString("dengan sisa pekerjaannya — tetapi laporkan dulu apa yang SUDAH kamu dapat, ")
	b.WriteString("jangan biarkan Pak Sudianto menunggu tanpa kabar.\n")
	b.WriteString("Bila pekerjaannya gagal atau datanya tidak ketemu, KATAKAN APA ADANYA. ")
	b.WriteString("Laporan karangan jauh lebih merugikan daripada laporan kosong.")
	return b.String()
}

// minDeferDelay = jeda terpendek yang boleh dipakai tugas berat.
const minDeferDelay = schedulerPrepLead + time.Minute

// deferDelay menentukan jeda sebelum tugas berat mulai dikerjakan. Batas ATAS 24 jam
// menjaga agar salah ketik (mis. 10080 = seminggu)
func deferDelay(minutes int) time.Duration {
	d := time.Duration(minutes) * time.Minute
	if d < minDeferDelay {
		return minDeferDelay
	}
	if d > 24*time.Hour {
		return 24 * time.Hour
	}
	return d
}

// deferTask menjalankan action DEFER_TASK: orchestrator menyerahkan pekerjaan berat
// (riset, penelusuran, penyusunan laporan) ke giliran latar belakang, lalu membalas
// Pak Sudianto sekarang juga tanpa menunggu pekerjaan itu selesai.
func (h *Handler) deferTask(ctx context.Context, initiator *model.Contact, a model.Action) {
	if initiator == nil || initiator.TrustLevel != "su" {
		trust := "(nil)"
		if initiator != nil {
			trust = initiator.TrustLevel
		}
		log.Printf("[DEFER] DITOLAK: inisiator non-SU (trust=%s)", trust)
		return
	}
	brief := firstNonEmptyStr(strings.TrimSpace(a.Task), strings.TrimSpace(a.ReminderNote))
	if brief == "" {
		log.Printf("[DEFER] brief kosong — diabaikan")
		return
	}

	// Rem anti-penundaan-beruntun (lihat maxPendingDeepWork).
	if tasks, err := h.Store.ListActiveTasks(ctx, "su", 50); err == nil {
		pending := 0
		for _, t := range tasks {
			if t.Kind == "deep_work" {
				pending++
			}
		}
		if pending >= maxPendingDeepWork {
			log.Printf("[DEFER] DITOLAK: sudah ada %d tugas berat mengantre (batas %d) — brief: %q",
				pending, maxPendingDeepWork, truncateRunes(brief, 80))
			return
		}
	}

	delay := deferDelay(a.DeferMinutes)

	label := strings.TrimSpace(a.ReminderLabel)
	if label == "" {
		label = truncateRunes(brief, 60)
	}
	if r := []rune(label); len(r) > 80 {
		label = string(r[:80])
	}

	id, err := h.Store.CreateScheduledTask(ctx, model.ScheduledTask{
		FireAt: time.Now().Add(delay), Kind: "deep_work", Note: brief, CreatedBy: "su", Label: label,
	})
	if err != nil {
		log.Printf("[DEFER] simpan tugas berat gagal: %v", err)
		return
	}
	log.Printf("[DEFER] tugas berat #%d dijadwalkan %s: %q", id,
		time.Now().Add(delay).In(wibZone).Format("15:04 WIB"), truncateRunes(brief, 80))
}

// buildReminderInstruction merangkai instruksi (giliran sistem) untuk orchestrator:
// ia menyusun sendiri pesan WhatsApp yang natural berdasarkan isi pengingat.
func buildReminderInstruction(task model.ScheduledTask) string {
	note := strings.TrimSpace(task.Note)
	var b strings.Builder
	b.WriteString("[PENGINGAT TERJADWAL — giliran sistem, BUKAN pesan dari Pak Sudianto]\n")
	b.WriteString("Sekarang waktunya menyampaikan pengingat berikut kepada Pak Sudianto. ")
	b.WriteString("Susun satu pesan WhatsApp yang ramah, ringkas, dan jelas dalam Bahasa Indonesia. ")
	b.WriteString("Sampaikan langsung isi pengingatnya; JANGAN menyebut bahwa ini proses otomatis atau giliran sistem.\n\n")
	b.WriteString("Isi pengingat: " + note)
	return b.String()
}

// pushToOrchestrator: primitive PUSH PROAKTIF ke Pak Sudianto
// - Inject giliran sistem ke percakapan orchestrator
// - Rakit konteks: tanggal + meeting snapshot + memori
// - Susun balasan orchestrator dan tahan sampai releaseAt
// - Kirim ke chat SU dan simpan ke memori percakapan
// - Penyusunan lebih awal, pengiriman tepat pada releaseAt
// - Tanpa actions (murni notifikasi, bebas efek samping)
// - Reusable untuk notifikasi proaktif lain
// pushOpts mengatur perilaku satu push terjadwal. Nilai nol = perilaku lama
// (tahan sampai releaseAt, timeout client default, action DIBUANG) sehingga jalur
// pengingat & digest yang sudah berjalan tidak berubah sedikit pun.
type pushOpts struct {
	releaseAt    time.Time     // tahan pesan sampai waktu ini
	timeout      time.Duration // 0 = pakai timeout client default
	applyActions bool
}

func (h *Handler) pushToOrchestrator(ctx context.Context, task string, opts pushOpts) error {
	if strings.TrimSpace(h.SUPhone) == "" {
		return errors.New("SU phone kosong")
	}
	const agentID = "orchestrator"
	convID := "agent:" + agentID + ":" + h.SUPhone

	// Kontak SU: pakai data whitelist bila ada (agar contact_id audit benar),
	// fallback ke kontak sintetis.
	contact := &model.Contact{Phone: h.SUPhone, TrustLevel: "su", Name: "Pak Sudianto (SU)"}
	if h.Store != nil {
		if c, err := h.Store.FindContact(ctx, model.Identifier{Kind: "phone", Value: h.SUPhone}); err == nil && c.ID > 0 {
			contact = c
		}
	}

	injectMsg := task
	if h.Memory != nil {
		if mc, err := h.Memory.Assemble(ctx, convID, contact); err != nil {
			log.Printf("[PUSH-SU] assemble konteks gagal conv=%s: %v (lanjut tanpa konteks)", convID, err)
		} else {
			mc.LiveStatus = buildDateAnchor()
			if snap := h.buildMeetingSnapshot(ctx); snap != "" {
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
			injectMsg = mc.BuildInjectMessage(task)
		}
	}

	cl := h.OpenClaw
	if opts.timeout > 0 {
		cl = cl.WithTimeout(opts.timeout)
	}
	reply, meta, err := h.injectWithRecoveryVia(ctx, cl, agentID, convID, injectMsg)
	if errors.Is(err, openclaw.ErrNoReply) {
		h.logExecution(ctx, convID, agentID, contact, injectMsg, nil, meta, "no_reply", "")
		return errors.New("orchestrator memilih diam")
	}
	if err != nil {
		h.logExecution(ctx, convID, agentID, contact, injectMsg, nil, meta, outcomeFromErr(err), err.Error())
		return err
	}
	execID := h.logExecution(ctx, convID, agentID, contact, injectMsg, reply, meta, "ok", "")

	resp := strings.TrimSpace(reply.Response)
	if resp == "" {
		return errors.New("orchestrator membalas kosong")
	}

	// TAHAN sampai tepat waktu: pesan sudah disusun di atas (lambat), sekarang
	// tunggu sisa waktu sampai fire_at lalu kirim — agar pengingat tidak telat.
	if wait := time.Until(opts.releaseAt); wait > 0 {
		log.Printf("[SCHEDULER] pesan siap, ditahan %s sampai jatuh tempo", wait.Round(time.Second))
	}
	if err := holdUntil(ctx, opts.releaseAt); err != nil {
		return fmt.Errorf("penahanan dibatalkan: %w", err)
	}

	// Action dijalankan SEBELUM teks dikirim agar lampiran (SEND_DOCUMENT) sampai
	// bersama laporannya, bukan menyusul setelahnya.
	if opts.applyActions && len(reply.Actions) > 0 {
		log.Printf("[PUSH-SU] menjalankan %d action dari balasan terjadwal", len(reply.Actions))
		h.applyActions(ctx, convID, contact, reply.Actions, execID, task)
	}

	// Simpan memori SEBELUM kirim agar konteks tetap konsisten bila kirim gagal.
	if h.Memory != nil {
		if werr := h.Memory.Write(ctx, convID, contact, agentID, "[Pengingat terjadwal aktif]", resp, reply.NewFacts); werr != nil {
			log.Printf("[PUSH-SU] memory write gagal conv=%s: %v", convID, werr)
		}
	}
	h.sendAndRecord(ctx, func() error { return h.Waha.SendText(h.SUPhone, resp) },
		model.OutboundMessage{
			ExecutionID: execPtr(execID), ConversationID: convID, ContactID: cidPtr(contact),
			AgentID: agentID, TargetChat: h.SUPhone, Kind: "proactive_reply", Text: resp,
		})
	return nil
}

// pushToSupport = padanan pushToOrchestrator untuk Bu Nova (agent "support"): menyusun
// balasan support DI PERCAKAPAN ASLINYA (agent:support:<NovaPhone>, sama seperti yang
// dipakai dispatchNovaVenue untuk koordinasi venue) lalu benar-benar MENGIRIMKANNYA ke
// WhatsApp Nova dan mencatatnya ke memori percakapan — bukan sesi admin-ctl terisolasi
// yang balasannya hanya kembali ke Admin tanpa efek nyata (bug lama, lihat ADMIN_SPAWN).
func (h *Handler) pushToSupport(ctx context.Context, task string, opts pushOpts) error {
	if strings.TrimSpace(h.NovaPhone) == "" {
		return errors.New("nomor Bu Nova (support) kosong")
	}
	const agentID = "support"
	convID := "agent:" + agentID + ":" + h.NovaPhone

	contact := &model.Contact{Phone: h.NovaPhone, TrustLevel: "semi_trusted", Name: "Bu Nova"}
	if h.Store != nil {
		if c, err := h.Store.FindContact(ctx, model.Identifier{Kind: "phone", Value: h.NovaPhone}); err == nil && c.ID > 0 {
			contact = c
		}
	}

	injectMsg := task
	if h.Memory != nil {
		if mc, err := h.Memory.Assemble(ctx, convID, contact); err != nil {
			log.Printf("[PUSH-SUPPORT] assemble konteks gagal conv=%s: %v (lanjut tanpa konteks)", convID, err)
		} else {
			mc.LiveStatus = buildDateAnchor()
			if snap := h.buildMeetingSnapshot(ctx); snap != "" {
				mc.LiveStatus += "\n\n" + snap
			}
			injectMsg = mc.BuildInjectMessage(task)
		}
	}

	cl := h.OpenClaw
	if opts.timeout > 0 {
		cl = cl.WithTimeout(opts.timeout)
	}
	reply, meta, err := h.injectWithRecoveryVia(ctx, cl, agentID, convID, injectMsg)
	if errors.Is(err, openclaw.ErrNoReply) {
		h.logExecution(ctx, convID, agentID, contact, injectMsg, nil, meta, "no_reply", "")
		return errors.New("support memilih diam")
	}
	if err != nil {
		h.logExecution(ctx, convID, agentID, contact, injectMsg, nil, meta, outcomeFromErr(err), err.Error())
		return err
	}
	execID := h.logExecution(ctx, convID, agentID, contact, injectMsg, reply, meta, "ok", "")

	resp := strings.TrimSpace(reply.Response)
	if resp == "" {
		return errors.New("support membalas kosong")
	}

	if wait := time.Until(opts.releaseAt); wait > 0 {
		log.Printf("[SCHEDULER] pesan ke Bu Nova siap, ditahan %s sampai jatuh tempo", wait.Round(time.Second))
	}
	if err := holdUntil(ctx, opts.releaseAt); err != nil {
		return fmt.Errorf("penahanan dibatalkan: %w", err)
	}

	if opts.applyActions && len(reply.Actions) > 0 {
		log.Printf("[PUSH-SUPPORT] menjalankan %d action dari balasan terjadwal", len(reply.Actions))
		h.applyActions(ctx, convID, contact, reply.Actions, execID, task)
	}

	if h.Memory != nil {
		if werr := h.Memory.Write(ctx, convID, contact, agentID, "[Direktif admin diteruskan]", resp, reply.NewFacts); werr != nil {
			log.Printf("[PUSH-SUPPORT] memory write gagal conv=%s: %v", convID, werr)
		}
	}
	h.sendAndRecord(ctx, func() error { return h.Waha.SendText(h.NovaPhone, resp) },
		model.OutboundMessage{
			ExecutionID: execPtr(execID), ConversationID: convID, ContactID: cidPtr(contact),
			AgentID: agentID, TargetChat: h.NovaPhone, Kind: "proactive_reply", Text: resp,
		})
	return nil
}

// reconcileUnfinalizedOfflineMeetings: tuntaskan meeting offline yang disetujui tapi
// belum difinalisasi. Dipanggil sekali saat start-up; idempoten.
func (h *Handler) reconcileUnfinalizedOfflineMeetings(ctx context.Context) {
	if h.Store == nil {
		return
	}
	stuck, err := h.Store.FindUnfinalizedOfflineMeetings(ctx)
	if err != nil {
		log.Printf("[RECONCILE] ambil meeting offline tertahan gagal: %v", err)
		return
	}
	if len(stuck) == 0 {
		return
	}
	log.Printf("[RECONCILE] %d meeting offline siap difinalisasi — menuntaskan", len(stuck))
	for i := range stuck {
		m := stuck[i]
		log.Printf("[RECONCILE] finalisasi meeting offline #%d", m.ID)
		h.finalizeOfflineMeeting(m.ID)
	}
}

// reminderLead mengembalikan lead time pengingat meeting (menit sebelum mulai).
func (h *Handler) reminderLead() int {
	if h.ReminderLeadMinutes > 0 {
		return h.ReminderLeadMinutes
	}
	return 15
}

// scheduleMeetingReminder membuat pengingat otomatis meeting (lead menit sebelum).
// Idempoten terhadap reschedule: batalkan yang lama dan buat yang baru.
// Aman dipanggil walau Services/Calendar nonaktif.
func (h *Handler) scheduleMeetingReminder(ctx context.Context, m *model.MeetingRequest, det meetingDetails) {
	if m == nil || m.ProposedDatetime == nil {
		return
	}
	// Bersihkan pengingat lama (kasus reschedule) sebelum membuat yang baru.
	if err := h.Store.CancelTasksForMeeting(ctx, m.ID); err != nil {
		log.Printf("[REMINDER] batalkan pengingat lama meeting #%d gagal: %v", m.ID, err)
	}

	// Meeting GRUP: satu perintah SU melahirkan SATU baris meeting per peserta, dan
	// finalisasi memanggil fungsi ini untuk TIAP baris. Tanpa dedup, SU menerima
	// pengingat sebanyak jumlah peserta.
	who := firstNonEmptyStr(m.ExternalName, det.AttendeeName)
	if strings.TrimSpace(det.GroupID) != "" && det.GroupSize >= 2 {
		canonical, names := h.groupReminderCanonical(ctx, det.GroupID)
		if canonical != 0 && m.ID != canonical {
			log.Printf("[REMINDER] meeting #%d bagian grup %s — pengingat dibuat pada baris kanonik #%d, dilewati",
				m.ID, det.GroupID, canonical)
			return
		}
		if names != "" {
			who = names
		}
	}

	lead := h.reminderLead()
	fireAt := m.ProposedDatetime.Add(-time.Duration(lead) * time.Minute)
	if fireAt.Before(time.Now()) {
		log.Printf("[REMINDER] meeting #%d terlalu dekat/sudah lewat — pengingat tidak dibuat", m.ID)
		return
	}

	title := firstNonEmptyStr(det.Title, m.Topic, "Meeting")
	note := buildMeetingReminderNote(title, who, m, det, lead)
	mid := m.ID
	id, err := h.Store.CreateScheduledTask(ctx, model.ScheduledTask{
		FireAt: fireAt, Kind: "meeting_reminder", Note: note, MeetingID: &mid, CreatedBy: "system",
	})
	if err != nil {
		log.Printf("[REMINDER] simpan pengingat meeting #%d gagal: %v", m.ID, err)
		return
	}
	log.Printf("[REMINDER] meeting #%d → pengingat #%d pada %s", m.ID, id,
		fireAt.In(wibZone).Format("2006-01-02 15:04 WIB"))
}

// buildMeetingReminderNote menyusun isi pengingat meeting (akan diolah orchestrator
// jadi pesan natural).
func buildMeetingReminderNote(title, who string, m *model.MeetingRequest, det meetingDetails, lead int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Meeting %q", title)
	if who != "" {
		b.WriteString(" dengan " + who)
	}
	if m.ProposedDatetime != nil {
		b.WriteString(" akan dimulai " + formatWIBLong(*m.ProposedDatetime))
	}
	fmt.Fprintf(&b, " (sekitar %d menit lagi).", lead)
	if v := strings.TrimSpace(m.Venue); v != "" {
		b.WriteString(" Lokasi: " + v + ".")
	} else if det.TeamsLink != "" {
		b.WriteString(" Tautan Teams: " + det.TeamsLink + ".")
	}
	return b.String()
}

// groupReminderCanonical menentukan baris meeting KANONIK untuk pengingat grup: ID
// meeting TERKECIL di antara peserta yang masih punya waktu terjadwal dan belum
// dibatalkan/ditolak.
func (h *Handler) groupReminderCanonical(ctx context.Context, groupID string) (int64, string) {
	meetings, err := h.Store.MeetingsByGroup(ctx, groupID)
	if err != nil || len(meetings) == 0 {
		return 0, ""
	}
	var canonical int64
	var names []string
	for _, m := range meetings {
		if m == nil || m.ProposedDatetime == nil {
			continue
		}
		if m.Status == "cancelled" || m.Status == "rejected" {
			continue
		}
		if canonical == 0 || m.ID < canonical {
			canonical = m.ID
		}
		var d meetingDetails
		_ = json.Unmarshal(m.Details, &d)
		if n := firstNonEmptyStr(m.ExternalName, d.AttendeeName); n != "" {
			names = append(names, n)
		}
	}
	return canonical, joinNamesID(names)
}

// joinNamesID menggabungkan nama gaya Indonesia: "A", "A dan B", "A, B dan C".
func joinNamesID(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " dan " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " dan " + names[len(names)-1]
	}
}

// remindExternalForMeeting mengirim pengingat meeting ke eksternal via WhatsApp setelah releaseAt.
func (h *Handler) remindExternalForMeeting(ctx context.Context, meetingID int64, releaseAt time.Time) {
	if h.Store == nil {
		return
	}
	m, err := h.Store.MeetingByID(ctx, meetingID)
	if err != nil || m == nil || m.ProposedDatetime == nil {
		return
	}
	// Jangan ingatkan meeting yang sudah dibatalkan.
	if m.Status == "cancelled" || m.Status == "rejected" {
		return
	}
	chat := h.externalChatIDForMeeting(ctx, m)
	if chat == "" {
		return
	}
	var det meetingDetails
	_ = json.Unmarshal(m.Details, &det)
	title := firstNonEmptyStr(det.Title, m.Topic, "meeting")
	who := firstNonEmptyStr(det.AttendeeName, m.ExternalName, "")
	msg := buildExternalReminderText(who, title, m, det, h.reminderLead())

	// Tahan sampai tepat waktu (push SU di atas sudah menahan; ini pengaman bila push SU
	// gagal lebih awal sebelum sempat menahan).
	if err := holdUntil(ctx, releaseAt); err != nil {
		return
	}
	agentID := firstNonEmptyStr(m.AgentID, "pa_communicator")
	h.sendAndRecord(ctx, func() error { return h.Waha.SendToChat(chat, msg) },
		model.OutboundMessage{
			ConversationID: m.ConversationID, ContactID: m.ContactID, AgentID: agentID,
			TargetChat: chat, Kind: "proactive_reply", Text: msg,
		})
	log.Printf("[REMINDER] pengingat meeting #%d dikirim ke pihak eksternal (%s)", m.ID, chat)
}

// buildExternalReminderText menyusun pesan pengingat WhatsApp untuk pihak eksternal.
func buildExternalReminderText(who, title string, m *model.MeetingRequest, det meetingDetails, lead int) string {
	greet := "Halo"
	if who != "" {
		greet = "Halo " + who
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s 🙏\n", greet)
	b.WriteString("Pengingat: pertemuan dengan Pak Sudianto akan segera berlangsung.\n")
	fmt.Fprintf(&b, "• Agenda: %s\n", title)
	if m.ProposedDatetime != nil {
		fmt.Fprintf(&b, "• Waktu: %s (sekitar %d menit lagi)\n", formatWIBLong(*m.ProposedDatetime), lead)
	}
	if v := strings.TrimSpace(m.Venue); v != "" {
		fmt.Fprintf(&b, "• Lokasi: %s\n", v)
	} else if det.TeamsLink != "" {
		fmt.Fprintf(&b, "• Tautan: %s\n", det.TeamsLink)
	} else {
		b.WriteString("• Silakan bergabung melalui tautan meeting yang telah disiapkan.\n")
	}
	b.WriteString("Terima kasih.")
	return b.String()
}

// setReminder mengeksekusi SET_REMINDER dengan validasi trust level 'su'.
func (h *Handler) setReminder(ctx context.Context, initiator *model.Contact, a model.Action) {
	if initiator == nil || initiator.TrustLevel != "su" {
		trust := "(nil)"
		if initiator != nil {
			trust = initiator.TrustLevel
		}
		log.Printf("[REMINDER] DITOLAK: inisiator non-SU (trust=%s)", trust)
		return
	}
	// Jangan gagalkan lebih awal bila reminderTime kosong/invalid: pengingat BERULANG
	// boleh mengandalkan recurTime saja. Validasi waktu ditangani per-cabang di bawah.
	when, err := parseReminderTime(a.ReminderTime)
	note := firstNonEmptyStr(strings.TrimSpace(a.ReminderNote), strings.TrimSpace(a.Task))
	if note == "" {
		log.Printf("[REMINDER] catatan kosong — diabaikan")
		return
	}

	// Rekurensi (opsional). "" / "none" = sekali tembak.
	recurKind := strings.ToLower(strings.TrimSpace(a.RecurKind))
	if recurKind == "none" {
		recurKind = ""
	}
	label := strings.TrimSpace(a.ReminderLabel)
	if r := []rune(label); len(r) > 80 {
		label = string(r[:80])
	}

	if recurKind != "" {
		if recurKind != "daily" && recurKind != "weekly" {
			log.Printf("[REMINDER] recurKind tak dikenal %q — diabaikan", a.RecurKind)
			return
		}
		if _, _, ok := parseHHMM(a.RecurTime); !ok {
			log.Printf("[REMINDER] recurTime tak valid %q (butuh HH:MM) — diabaikan", a.RecurTime)
			return
		}
		if recurKind == "weekly" && (a.RecurDow == nil || *a.RecurDow < 0 || *a.RecurDow > 6) {
			log.Printf("[REMINDER] weekly butuh recurDow 0-6 — diabaikan (dapat %v)", a.RecurDow)
			return
		}
		first := when
		if err != nil { // ReminderTime tak diberikan/valid → turunkan
			t, ok := nextOccurrence(model.ScheduledTask{RecurKind: recurKind, RecurTime: a.RecurTime, RecurDow: a.RecurDow}, time.Now())
			if !ok {
				log.Printf("[REMINDER] tak bisa menghitung kejadian pertama berulang — diabaikan")
				return
			}
			first = t
		} else if first.Before(time.Now().Add(-1 * time.Minute)) {
			log.Printf("[REMINDER] waktu pertama sudah lewat (%s) — diabaikan", first.In(wibZone).Format("2006-01-02 15:04 WIB"))
			return
		}
		id, cerr := h.Store.CreateScheduledTask(ctx, model.ScheduledTask{
			FireAt: first, Kind: "reminder", Note: note, CreatedBy: "su",
			RecurKind: recurKind, RecurTime: strings.TrimSpace(a.RecurTime), RecurDow: a.RecurDow, Label: label,
		})
		if cerr != nil {
			log.Printf("[REMINDER] simpan pengingat berulang gagal: %v", cerr)
			return
		}
		log.Printf("[REMINDER] pengingat berulang #%d (%s %s) mulai %s: %q", id, recurKind,
			strings.TrimSpace(a.RecurTime), first.In(wibZone).Format("2006-01-02 15:04 WIB"), note)
		return
	}

	// Sekali tembak: ReminderTime wajib valid.
	if err != nil {
		log.Printf("[REMINDER] waktu tidak valid %q: %v (diabaikan)", a.ReminderTime, err)
		return
	}
	if when.Before(time.Now().Add(-1 * time.Minute)) {
		log.Printf("[REMINDER] waktu sudah lewat (%s) — diabaikan", when.In(wibZone).Format("2006-01-02 15:04 WIB"))
		return
	}
	id, err := h.Store.CreateScheduledTask(ctx, model.ScheduledTask{
		FireAt: when, Kind: "reminder", Note: note, CreatedBy: "su", Label: label,
	})
	if err != nil {
		log.Printf("[REMINDER] simpan gagal: %v", err)
		return
	}
	log.Printf("[REMINDER] pengingat #%d dijadwalkan %s: %q", id,
		when.In(wibZone).Format("2006-01-02 15:04 WIB"), note)
}

// cancelReminder menjalankan action CANCEL_REMINDER: menghentikan pengingat aktif (berlaku untuk seri berulang).
// Hanya SU yang dapat membatalkan.
func (h *Handler) cancelReminder(ctx context.Context, initiator *model.Contact, a model.Action) {
	if initiator == nil || initiator.TrustLevel != "su" {
		trust := "(nil)"
		if initiator != nil {
			trust = initiator.TrustLevel
		}
		log.Printf("[REMINDER] CANCEL DITOLAK: inisiator non-SU (trust=%s)", trust)
		return
	}
	if a.ReminderID <= 0 {
		log.Printf("[REMINDER] CANCEL tanpa reminderId — diabaikan")
		return
	}
	err := h.Store.CancelScheduledTask(ctx, a.ReminderID, "su")
	if errors.Is(err, db.ErrScheduledTaskNotFound) {
		log.Printf("[REMINDER] CANCEL #%d: tidak ditemukan / bukan pengingat aktif SU", a.ReminderID)
		return
	}
	if err != nil {
		log.Printf("[REMINDER] CANCEL #%d gagal: %v", a.ReminderID, err)
		return
	}
	log.Printf("[REMINDER] pengingat #%d dibatalkan (seri berulang, bila ada, berhenti)", a.ReminderID)
}

// scheduleCalendarDigest menjalankan SCHEDULE_CALENDAR_DIGEST untuk cek kalender
// terjadwal. Hanya inisiator trust 'su' yang boleh, dan tugas disimpan sebagai
// scheduled_task kind="calendar_digest".
func (h *Handler) scheduleCalendarDigest(ctx context.Context, initiator *model.Contact, a model.Action) {
	if initiator == nil || initiator.TrustLevel != "su" {
		trust := "(nil)"
		if initiator != nil {
			trust = initiator.TrustLevel
		}
		log.Printf("[DIGEST] DITOLAK: inisiator non-SU (trust=%s)", trust)
		return
	}
	// Fokus opsional dari SU (mis. "susun rencana kerja") disimpan di Note
	focus := firstNonEmptyStr(strings.TrimSpace(a.ReminderNote), strings.TrimSpace(a.Task))
	label := strings.TrimSpace(a.ReminderLabel)
	if label == "" {
		label = "Digest kalender"
	}
	if r := []rune(label); len(r) > 80 {
		label = string(r[:80])
	}

	when, terr := parseReminderTime(a.ReminderTime)

	recurKind := strings.ToLower(strings.TrimSpace(a.RecurKind))
	if recurKind == "none" {
		recurKind = ""
	}

	if recurKind != "" {
		if recurKind != "daily" && recurKind != "weekly" {
			log.Printf("[DIGEST] recurKind tak dikenal %q — diabaikan", a.RecurKind)
			return
		}
		if _, _, ok := parseHHMM(a.RecurTime); !ok {
			log.Printf("[DIGEST] recurTime tak valid %q (butuh HH:MM) — diabaikan", a.RecurTime)
			return
		}
		if recurKind == "weekly" && (a.RecurDow == nil || *a.RecurDow < 0 || *a.RecurDow > 6) {
			log.Printf("[DIGEST] weekly butuh recurDow 0-6 — diabaikan (dapat %v)", a.RecurDow)
			return
		}
		first := when
		if terr != nil {
			t, ok := nextOccurrence(model.ScheduledTask{RecurKind: recurKind, RecurTime: a.RecurTime, RecurDow: a.RecurDow}, time.Now())
			if !ok {
				log.Printf("[DIGEST] tak bisa menghitung kejadian pertama berulang — diabaikan")
				return
			}
			first = t
		} else if first.Before(time.Now().Add(-1 * time.Minute)) {
			log.Printf("[DIGEST] waktu pertama sudah lewat (%s) — diabaikan", first.In(wibZone).Format("2006-01-02 15:04 WIB"))
			return
		}
		id, cerr := h.Store.CreateScheduledTask(ctx, model.ScheduledTask{
			FireAt: first, Kind: "calendar_digest", Note: focus, CreatedBy: "su",
			RecurKind: recurKind, RecurTime: strings.TrimSpace(a.RecurTime), RecurDow: a.RecurDow, Label: label,
		})
		if cerr != nil {
			log.Printf("[DIGEST] simpan digest berulang gagal: %v", cerr)
			return
		}
		log.Printf("[DIGEST] digest kalender berulang #%d (%s %s) mulai %s", id, recurKind,
			strings.TrimSpace(a.RecurTime), first.In(wibZone).Format("2006-01-02 15:04 WIB"))
		return
	}

	// Sekali tembak: ReminderTime wajib valid.
	if terr != nil {
		log.Printf("[DIGEST] waktu tidak valid %q: %v (diabaikan)", a.ReminderTime, terr)
		return
	}
	if when.Before(time.Now().Add(-1 * time.Minute)) {
		log.Printf("[DIGEST] waktu sudah lewat (%s) — diabaikan", when.In(wibZone).Format("2006-01-02 15:04 WIB"))
		return
	}
	id, cerr := h.Store.CreateScheduledTask(ctx, model.ScheduledTask{
		FireAt: when, Kind: "calendar_digest", Note: focus, CreatedBy: "su", Label: label,
	})
	if cerr != nil {
		log.Printf("[DIGEST] simpan digest gagal: %v", cerr)
		return
	}
	log.Printf("[DIGEST] digest kalender #%d dijadwalkan %s", id,
		when.In(wibZone).Format("2006-01-02 15:04 WIB"))
}

// buildCalendarDigestInstruction menyusun instruksi orchestrator untuk digest kalender:
// mengambil agenda hari ini dari kalender PA dan meminta orchestrator meringkasnya.
func (h *Handler) buildCalendarDigestInstruction(ctx context.Context, task model.ScheduledTask) string {
	now := time.Now().In(wibZone)
	date := now.Format("2006-01-02")
	var agenda string
	switch {
	case !h.Services.Enabled():
		agenda = "(Integrasi kalender sedang tidak aktif — agenda tidak dapat diambil otomatis.)"
	default:
		evs, err := h.Services.Availability(ctx, date)
		if err != nil {
			log.Printf("[DIGEST] ambil kalender %s gagal: %v", date, err)
			agenda = "(Gagal mengambil agenda dari kalender saat ini.)"
		} else {
			agenda = formatCalendarAgenda(evs)
		}
	}

	var b strings.Builder
	b.WriteString("[DIGEST KALENDER TERJADWAL — giliran sistem, BUKAN pesan dari Pak Sudianto]\n")
	fmt.Fprintf(&b, "Ini pengecekan kalender terjadwal untuk %s.\n", formatWIBDate(now))
	b.WriteString("Susun SATU pesan WhatsApp Bahasa Indonesia yang ramah & ringkas untuk Pak Sudianto: ")
	b.WriteString("sampaikan ringkasan agenda hari ini, lalu usulkan rencana kerja singkat (prioritas/urutan). ")
	b.WriteString("JANGAN menyebut bahwa ini proses otomatis atau giliran sistem.\n")
	if focus := strings.TrimSpace(task.Note); focus != "" {
		b.WriteString("Fokus/permintaan khusus dari Pak Sudianto: " + focus + "\n")
	}
	fmt.Fprintf(&b, "\nAgenda kalender (%s):\n%s", formatWIBDate(now), agenda)
	return b.String()
}

// formatCalendarAgenda merangkai daftar event kalender jadi teks ringkas untuk digest.
func formatCalendarAgenda(evs []services.AvailabilityEvent) string {
	if len(evs) == 0 {
		return "- (Tidak ada agenda/meeting terjadwal hari ini.)"
	}
	var b strings.Builder
	for _, e := range evs {
		s := parseGraphLocal(e.Start)
		en := parseGraphLocal(e.End)
		slot := "?"
		switch {
		case !s.IsZero() && !en.IsZero():
			slot = s.Format("15.04") + "–" + en.Format("15.04")
		case !s.IsZero():
			slot = s.Format("15.04")
		}
		subj := strings.TrimSpace(e.Subject)
		if subj == "" {
			subj = "(tanpa judul)"
		}
		fmt.Fprintf(&b, "- %s WIB — %s", slot, subj)
		if loc := strings.TrimSpace(e.Location); loc != "" {
			b.WriteString(" @ " + loc)
		} else if e.IsOnline {
			b.WriteString(" (online)")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Ringkasan pengingat aktif SU dari PostgreSQL untuk konteks orchestrator.
func (h *Handler) buildReminderSnapshot(ctx context.Context) string {
	if h.Store == nil {
		return ""
	}
	tasks, err := h.Store.ListActiveTasks(ctx, "su", 50)
	if err != nil {
		log.Printf("[SNAPSHOT] ambil pengingat gagal: %v", err)
		return ""
	}
	if len(tasks) == 0 {
		return ""
	}
	var rows []string
	for _, t := range tasks {
		when := t.FireAt.In(wibZone).Format("Mon 02 Jan 2006 15:04") + " WIB"
		recur := ""
		switch t.RecurKind {
		case "daily":
			recur = " | BERULANG tiap hari " + t.RecurTime
		case "weekly":
			recur = " | BERULANG tiap " + weekdayIndo(t.RecurDow) + " " + t.RecurTime
		}
		label := t.Label
		if label == "" {
			label = truncateRunes(t.Note, 40)
		}
		kindTag := ""
		switch t.Kind {
		case "calendar_digest":
			kindTag = " [digest kalender]"
		case "deep_work":
			kindTag = " [tugas tertunda — sedang/akan dikerjakan]"
		}
		rows = append(rows, fmt.Sprintf("- reminderId=%d | %s%s | mulai %s%s", t.ID, label, kindTag, when, recur))
	}
	return "[PENGINGAT & DIGEST AKTIF — data LANGSUNG & OTORITATIF dari sistem. Termasuk " +
		"pengingat (SET_REMINDER) dan digest kalender terjadwal (SCHEDULE_CALENDAR_DIGEST). " +
		"Untuk MEMBATALKAN/menghentikan salah satunya, pakai CANCEL_REMINDER dengan reminderId " +
		"di bawah. Untuk MENGUBAH jadwalnya, batalkan lalu buat baru.]\n" +
		strings.Join(rows, "\n")
}

// weekdayIndo memetakan 0-6 (Minggu..Sabtu) ke nama hari Indonesia.
func weekdayIndo(dow *int) string {
	names := []string{"Minggu", "Senin", "Selasa", "Rabu", "Kamis", "Jumat", "Sabtu"}
	if dow == nil || *dow < 0 || *dow > 6 {
		return "?"
	}
	return names[*dow]
}

// truncateRunes memotong string ke maksimal n rune (menambahkan "…" bila terpotong).
func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// parseReminderTime memparse waktu pengingat. Utamakan RFC3339 (berzona); bila
// orchestrator mengirim waktu tanpa zona, asumsikan WIB.
func parseReminderTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("kosong")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05", "2006-01-02T15:04",
		"2006-01-02 15:04:05", "2006-01-02 15:04",
	} {
		if t, err := time.ParseInLocation(layout, s, wibZone); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("format tak dikenal: %q", s)
}
