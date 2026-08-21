// API Gateway — single entry/exit point untuk PA AI System.
// webhook receiver (WAHA), /health, helper kirim ke WAHA, bootstrap session.
// security layer (whitelist -> rate limit -> sanitizer) di depan handler.
package main

import (
	"context"
	"log"

	"github.com/gin-gonic/gin"

	"pa-ai/api-gateway/src/config"
	"pa-ai/api-gateway/src/db"
	"pa-ai/api-gateway/src/google"
	"pa-ai/api-gateway/src/memory"
	"pa-ai/api-gateway/src/middleware"
	"pa-ai/api-gateway/src/openclaw"
	"pa-ai/api-gateway/src/routes"
	"pa-ai/api-gateway/src/services"
	"pa-ai/api-gateway/src/waha"
)

func main() {
	cfg := config.Load()

	ctx := context.Background()

	// --- PostgreSQL (whitelist) ---
	store, err := db.NewStore(ctx, cfg)
	if err != nil {
		log.Fatalf("gagal koneksi PostgreSQL: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		log.Fatalf("gagal migrate skema: %v", err)
	}
	if err := store.SeedTrustedContacts(ctx, cfg); err != nil {
		log.Fatalf("gagal seed kontak trusted: %v", err)
	}

	// --- Redis (rate limit) ---
	limiter, err := db.NewRateLimiter(ctx, cfg)
	if err != nil {
		log.Fatalf("gagal koneksi Redis: %v", err)
	}
	defer limiter.Close()

	// --- Redis (memory cache) ---
	cache, err := db.NewCache(ctx, cfg)
	if err != nil {
		log.Fatalf("gagal koneksi Redis (cache): %v", err)
	}
	defer cache.Close()
	mem := memory.New(store, cache)

	// --- WAHA client + bootstrap session ---
	wahaClient := waha.New(cfg.WahaURL, cfg.WahaAPIKey, cfg.WahaSession)
	go wahaClient.EnsureSessionStarted()

	// --- OpenClaw client (shell-out ke CLI `openclaw agent`) ---
	openClawClient := openclaw.New(cfg.OpenClawBin, cfg.OpenClawNode, cfg.OpenClawScript, cfg.OpenClawAgent, cfg.OpenClawTimeout)

	// --- Service (Calendar + Email via MS Graph, in-process) ---
	// Dipanggil hanya oleh approval gate setelah SU approve. Tak ada port polos.
	svcClient := services.New(
		cfg.MSGraphTenantID, cfg.MSGraphClientID, cfg.MSGraphClientSecret,
		cfg.MSGraphUserUPN, cfg.MSGraphRefreshToken, cfg.MailFromName, cfg.SignaturePath,
	)

	// --- Google People API client (opsional; nonaktif bila kredensial kosong) ---
	var googleClient *google.Client
	if cfg.GoogleContactsEnabled {
		googleClient = google.New(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRefreshToken)
	}

	h := &routes.Handler{Waha: wahaClient, Store: store, OpenClaw: openClawClient, Memory: mem, Services: svcClient, SUPhone: cfg.SUPhone, NovaPhone: cfg.NovaPhone, AdminPhone: cfg.AdminPhone, SUPhones: cfg.SUPhones, AdminPhones: cfg.AdminPhones, ReminderLeadMinutes: cfg.ReminderLeadMinutes, ReadDelayMin: cfg.ReadDelayMin, ReadDelayMax: cfg.ReadDelayMax, PresenceDelayMin: cfg.PresenceDelayMin, PresenceDelayMax: cfg.PresenceDelayMax, LongReplyDelayMin: cfg.LongReplyDelayMin, LongReplyDelayMax: cfg.LongReplyDelayMax, LongReplyThreshold: cfg.LongReplyThreshold, BurstWindow: cfg.BurstWindow, SpawnStaggerInterval: cfg.SpawnStaggerInterval, PreflightCheckNumber: cfg.PreflightCheckNumber, Google: googleClient, GoogleContactSyncDelay: cfg.GoogleContactSyncDelay, AlertEmailTo: cfg.AlertEmailTo, AlertWebhookToken: cfg.AlertWebhookToken, DocWorkDir: cfg.DocWorkDir}
	// Gerbang perhatian global: satu slot (kapasitas 1) sebagai mutex FIFO lintas chat.
	// Aktif hanya bila ATTENTION_QUEUE=true; nil = fitur nonaktif (paralel seperti biasa).
	if cfg.AttentionQueue {
		h.AttentionGate = make(chan struct{}, 1)
		h.AttentionMaxWait = cfg.AttentionMaxWait
	}
	admin := &routes.AdminHandler{Store: store, Gateway: h}

	// --- Worker pengingat: kirim tugas terjadwal ke SU saat jatuh tempo ---
	h.StartScheduler(ctx)

	// --- Worker pantauan email: periksa inbox berkala, lapor email yang cocok ---
	h.StartEmailWatcher(ctx)

	// --- Auto sanitizer ---
	h.StartDocJanitor(ctx)

	// --- Kolektor metrik operasional
	h.StartHealthCollector(ctx)

	r := gin.Default()

	// Instrumentasi Prometheus: catat jumlah & durasi tiap request, lalu
	// ekspos di /metrics untuk di-scrape.
	r.Use(middleware.Metrics())
	r.GET("/metrics", middleware.MetricsHandler())

	r.GET("/health", func(c *gin.Context) {
		status, err := wahaClient.SessionStatus()
		waStatus := status
		if err != nil {
			waStatus = "unreachable"
		}
		c.JSON(200, gin.H{
			"status":       "ok",
			"service":      "api-gateway",
			"waha_session": waStatus,
		})
	})

	// Security chain : auth -> rate limit -> sanitize -> handler.
	webhook := r.Group("/webhook")
	{
		webhook.POST("/waha",
			middleware.Auth(store, wahaClient, cfg.WhitelistMode == "open"),
			middleware.RateLimit(limiter, store),
			middleware.Sanitize(cfg.MaxMsgLen, store),
			h.WahaInbound,
		)
		// openclaw-output adalah jalur internal (bukan dari WhatsApp), tanpa chain WA.
		webhook.POST("/openclaw-output", h.OpenClawOutput)
	}

	// Webhook Alertmanager: relay alert -> email. Auth Bearer token
	// sendiri (ALERT_WEBHOOK_TOKEN), bukan X-Admin-Key. Nonaktif bila token kosong.
	r.POST("/internal/alerts", h.AlertWebhook)

	// Admin API (kelola whitelist, pantau external & audit log) — wajib X-Admin-Key.
	adminGrp := r.Group("/admin", middleware.AdminAuth(cfg.AdminAPIKey))
	{
		adminGrp.GET("/contacts", admin.ListContacts)
		adminGrp.POST("/contacts", admin.AddContact)
		adminGrp.PATCH("/contacts/:phone", admin.UpdateContact)
		adminGrp.DELETE("/contacts/:phone", admin.DeleteContact)

		adminGrp.GET("/external", admin.ListExternal)
		adminGrp.GET("/external/:identifier", admin.GetExternal)
		adminGrp.PATCH("/external/:identifier", admin.UpdateExternal)
		adminGrp.DELETE("/external/:identifier", admin.DeleteExternal)
		adminGrp.POST("/external/:identifier/block", admin.BlockExternal)
		adminGrp.POST("/external/:identifier/unblock", admin.UnblockExternal)
		adminGrp.POST("/external/:identifier/promote", admin.PromoteExternal)

		// Block/unblock cepat by phone (mode open) — satu panggilan menutup semua jalur.
		adminGrp.POST("/block", admin.BlockPhone)
		adminGrp.POST("/unblock", admin.UnblockPhone)

		adminGrp.GET("/logs", admin.ListLogs)

		// Approval gate
		adminGrp.GET("/approvals", admin.ListApprovals)
		adminGrp.POST("/approvals/:id/approve", admin.ApproveApproval)
		adminGrp.POST("/approvals/:id/reject", admin.RejectApproval)

		// Observability & evaluasi 
		adminGrp.GET("/executions", admin.ListExecutions)
		adminGrp.GET("/usage", admin.Usage)
		adminGrp.GET("/outbound", admin.ListOutbound)
		adminGrp.GET("/meetings", admin.ListMeetings)
		adminGrp.GET("/meetings/:id/history", admin.MeetingHistory)
		adminGrp.POST("/meetings/:id/resend-rsvp", admin.ResendRSVP)
	}

	addr := ":" + cfg.Port
	log.Printf("[api-gateway] listening on %s (WAHA=%s session=%s)", addr, cfg.WahaURL, cfg.WahaSession)
	if err := r.Run(addr); err != nil {
		log.Fatalf("server gagal jalan: %v", err)
	}
}
