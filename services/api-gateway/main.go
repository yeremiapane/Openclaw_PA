// API Gateway — single entry/exit point untuk PA AI System.
// Fase 3: webhook receiver (WAHA), /health, helper kirim ke WAHA, bootstrap session.
// Fase 4: security layer (whitelist -> rate limit -> sanitizer) di depan handler.
package main

import (
	"context"
	"log"

	"github.com/gin-gonic/gin"

	"pa-ai/api-gateway/src/config"
	"pa-ai/api-gateway/src/db"
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

	// --- Redis (memory cache, Fase 7) ---
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

	// --- Service Fase 9 (Calendar + Email via MS Graph, in-process) ---
	// Dipanggil hanya oleh approval gate setelah SU approve. Tak ada port polos.
	svcClient := services.New(
		cfg.MSGraphTenantID, cfg.MSGraphClientID, cfg.MSGraphClientSecret,
		cfg.MSGraphUserUPN, cfg.MSGraphRefreshToken, cfg.MailFromName, cfg.SignaturePath,
	)

	h := &routes.Handler{Waha: wahaClient, Store: store, OpenClaw: openClawClient, Memory: mem, Services: svcClient, SUPhone: cfg.SUPhone, NovaPhone: cfg.NovaPhone, ReminderLeadMinutes: cfg.ReminderLeadMinutes}
	admin := &routes.AdminHandler{Store: store, Gateway: h}

	// --- Worker pengingat (Fase A): kirim tugas terjadwal ke SU saat jatuh tempo ---
	h.StartScheduler(ctx)

	// --- Worker pantauan email (Fitur E): periksa inbox berkala, lapor email yang cocok ---
	h.StartEmailWatcher(ctx)

	r := gin.Default()

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

	// Security chain Fase 4: auth -> rate limit -> sanitize -> handler.
	webhook := r.Group("/webhook")
	{
		webhook.POST("/waha",
			middleware.Auth(store, wahaClient),
			middleware.RateLimit(limiter, store),
			middleware.Sanitize(cfg.MaxMsgLen, store),
			h.WahaInbound,
		)
		// openclaw-output adalah jalur internal (bukan dari WhatsApp), tanpa chain WA.
		webhook.POST("/openclaw-output", h.OpenClawOutput)
	}

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
		adminGrp.POST("/external/:identifier/promote", admin.PromoteExternal)

		adminGrp.GET("/logs", admin.ListLogs)

		// Approval gate (Fase 8)
		adminGrp.GET("/approvals", admin.ListApprovals)
		adminGrp.POST("/approvals/:id/approve", admin.ApproveApproval)
		adminGrp.POST("/approvals/:id/reject", admin.RejectApproval)

		// Observability & evaluasi (Fase 8.5)
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
