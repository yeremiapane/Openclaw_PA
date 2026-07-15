//go:build integration

// Integration test untuk WHITELIST_MODE (strict/open) + endpoint admin block/unblock cepat.
// Menjalankan CHAIN Auth yang sama seperti gateway terdeploy terhadap Postgres ASLI
// (infra live) TANPA menyentuh WAHA/OpenClaw: handler terminal hanya melaporkan keputusan.
// Semua baris memakai nomor BOGUS ber-namespace & dibersihkan di awal + akhir. Jalankan:
//
//	go test -tags integration ./src/routes/ -run TestWhitelistMode -v
package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"pa-ai/api-gateway/src/config"
	"pa-ai/api-gateway/src/db"
	"pa-ai/api-gateway/src/middleware"
	"pa-ai/api-gateway/src/model"
)

func TestWhitelistModeAndBlockUnblock(t *testing.T) {
	ctx := context.Background()
	_ = godotenv.Load("../../../../.env")
	cfg := config.Load()

	store, err := db.NewStore(ctx, cfg)
	if err != nil {
		t.Fatalf("koneksi Postgres gagal: %v", err)
	}
	defer store.Close()

	pool, err := pgxpool.New(ctx, cfg.DBConnString())
	if err != nil {
		t.Fatalf("pool mentah gagal: %v", err)
	}
	defer pool.Close()

	// Nomor & LID BOGUS khusus test (bukan nomor asli SU/Nova/tester).
	const phone = "6288000000199"
	const cusID = phone + "@c.us"
	const bogusLid = "9990001112223"
	const lidID = bogusLid + "@lid"

	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM access_logs WHERE identifier = ANY($1)`,
			[]string{cusID, lidID})
		_, _ = pool.Exec(ctx, `DELETE FROM external_contacts WHERE identifier = ANY($1)`,
			[]string{cusID, lidID})
		_, _ = pool.Exec(ctx, `DELETE FROM contacts WHERE phone = $1`, phone)
	}
	cleanup()
	defer cleanup()

	gin.SetMode(gin.TestMode)

	// terminal: handler paling ujung — hanya jalan bila Auth MENGIZINKAN (c.Next()).
	// Melaporkan trust kontak agar test bisa membedakan served vs blocked.
	terminal := func(c *gin.Context) {
		v, ok := c.Get(middleware.CtxContact)
		if !ok {
			c.JSON(http.StatusOK, gin.H{"status": "served"})
			return
		}
		ct := v.(*model.Contact)
		c.JSON(http.StatusOK, gin.H{"status": "served", "trust": ct.TrustLevel, "phone": ct.Phone, "id": ct.ID})
	}

	newWebhook := func(openMode bool) *gin.Engine {
		r := gin.New()
		r.POST("/webhook/waha", middleware.Auth(store, nil, openMode), terminal)
		return r
	}

	adminGrp := gin.New()
	admin := &AdminHandler{Store: store}
	ag := adminGrp.Group("/admin", middleware.AdminAuth(cfg.AdminAPIKey))
	ag.POST("/block", admin.BlockPhone)
	ag.POST("/unblock", admin.UnblockPhone)

	// ── helper request ──
	postWebhook := func(r *gin.Engine, from, body string) map[string]any {
		ev := model.WahaEvent{Event: "message", Session: "default"}
		ev.Payload.From = from
		ev.Payload.Body = body
		raw, _ := json.Marshal(ev)
		req := httptest.NewRequest(http.MethodPost, "/webhook/waha", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	postAdmin := func(path, key string, payload map[string]string) (int, map[string]any) {
		raw, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("X-Admin-Key", key)
		}
		w := httptest.NewRecorder()
		adminGrp.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	contactActive := func() bool {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM contacts WHERE phone=$1 AND deleted_at IS NULL`, phone).Scan(&n)
		return n > 0
	}
	extStatus := func(id string) string {
		var s string
		_ = pool.QueryRow(ctx, `SELECT COALESCE(status,'') FROM external_contacts WHERE identifier=$1`, id).Scan(&s)
		return s
	}

	strict := newWebhook(false)
	open := newWebhook(true)

	// ── 1) STRICT: nomor tak dikenal → DIBLOKIR, tak ada kontak dibuat ──
	if got := postWebhook(strict, cusID, "halo strict"); got["status"] != "blocked" {
		t.Fatalf("#1 strict unknown: status=%v, ingin blocked", got["status"])
	}
	if contactActive() {
		t.Fatalf("#1 strict tak boleh membuat kontak whitelist")
	}

	// ── 2) OPEN: nomor tak dikenal → AUTO-WHITELIST (served, trust external) ──
	got := postWebhook(open, cusID, "halo open")
	if got["status"] != "served" {
		t.Fatalf("#2 open unknown: status=%v, ingin served", got["status"])
	}
	if got["trust"] != "external" {
		t.Fatalf("#2 open trust=%v, ingin external", got["trust"])
	}
	if !contactActive() {
		t.Fatalf("#2 open harus membuat kontak whitelist aktif")
	}
	firstID := got["id"]

	// ── 3) OPEN idempoten: nomor sama → served, kontak sama (bukan duplikat) ──
	got3 := postWebhook(open, cusID, "halo lagi")
	if got3["status"] != "served" || got3["id"] != firstID {
		t.Fatalf("#3 open idempoten gagal: %v (ingin served id=%v)", got3, firstID)
	}
	var activeCount int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM contacts WHERE phone=$1 AND deleted_at IS NULL`, phone).Scan(&activeCount)
	if activeCount != 1 {
		t.Fatalf("#3 kontak aktif=%d, ingin tepat 1 (tanpa duplikat)", activeCount)
	}

	// ── 4) OPEN + @lid tak teresolusi (lids=nil, tanpa altPhone) → DIBLOKIR ──
	if got := postWebhook(open, lidID, "halo lid"); got["status"] != "blocked" {
		t.Fatalf("#4 open @lid tak teresolusi: status=%v, ingin blocked (phone NOT NULL)", got["status"])
	}

	// ── 5) ADMIN /block: kunci salah → 401 ──
	if code, _ := postAdmin("/admin/block", "kunci-salah", map[string]string{"phone": phone}); code != http.StatusUnauthorized {
		t.Fatalf("#5 block kunci salah: code=%d, ingin 401", code)
	}
	// phone kosong → 400
	if code, _ := postAdmin("/admin/block", cfg.AdminAPIKey, map[string]string{"phone": ""}); code != http.StatusBadRequest {
		t.Fatalf("#5b block phone kosong: code=%d, ingin 400", code)
	}

	// ── 6) ADMIN /block sah: cabut whitelist + tandai blocked @c.us ──
	code, resp := postAdmin("/admin/block", cfg.AdminAPIKey, map[string]string{"phone": phone, "notes": "itest spam"})
	if code != http.StatusOK || resp["status"] != "blocked" {
		t.Fatalf("#6 block: code=%d resp=%v", code, resp)
	}
	if contactActive() {
		t.Fatalf("#6 block harus soft-delete kontak whitelist")
	}
	if s := extStatus(cusID); s != "blocked" {
		t.Fatalf("#6 external %s status=%q, ingin blocked", cusID, s)
	}

	// ── 7) OPEN setelah block: nomor sama → tetap DIBLOKIR (admin menang) ──
	if got := postWebhook(open, cusID, "halo lagi setelah block"); got["status"] != "blocked" {
		t.Fatalf("#7 open setelah block: status=%v, ingin blocked", got["status"])
	}
	if contactActive() {
		t.Fatalf("#7 nomor terblokir tak boleh ter-auto-whitelist ulang")
	}

	// ── 8) ADMIN /unblock: kembalikan ke pending ──
	code, resp = postAdmin("/admin/unblock", cfg.AdminAPIKey, map[string]string{"phone": phone})
	if code != http.StatusOK || resp["status"] != "unblocked" {
		t.Fatalf("#8 unblock: code=%d resp=%v", code, resp)
	}
	if s := extStatus(cusID); s != "pending" {
		t.Fatalf("#8 external %s status=%q, ingin pending", cusID, s)
	}

	// ── 9) OPEN setelah unblock: nomor auto-whitelist ULANG → served ──
	if got := postWebhook(open, cusID, "halo setelah unblock"); got["status"] != "served" {
		t.Fatalf("#9 open setelah unblock: status=%v, ingin served", got["status"])
	}
	if !contactActive() {
		t.Fatalf("#9 setelah unblock harus auto-whitelist ulang")
	}

	fmt.Println("OK: strict blokir · open auto-whitelist (idempoten) · @lid-no-phone blokir · /block menang atas open · /unblock pulih")
}
