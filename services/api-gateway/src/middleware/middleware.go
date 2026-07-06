// Package middleware berisi security layer :
// auth/whitelist -> rate limit -> input sanitizer, plus audit logging,
// tracking kontak external, dan proteksi endpoint admin.
package middleware

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"pa-ai/api-gateway/src/db"
	"pa-ai/api-gateway/src/model"
)

// Kunci context yang dibagikan antar middleware & handler.
const (
	CtxEvent   = "waha_event" // *model.WahaEvent
	CtxContact = "contact"    // *model.Contact
	CtxText    = "clean_text" // string
)

// bodyPreview memotong teks untuk disimpan di audit log.
func bodyPreview(s string) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 200 {
		return string([]rune(s)[:200])
	}
	return s
}

// phoneOf mengembalikan nomor kanonik dari identifier (kosong bila bukan @c.us).
func phoneOf(id model.Identifier) string {
	if id.Kind == "phone" {
		return id.Value
	}
	return ""
}

// LidResolver memetakan LID WhatsApp (privacy ID) ke nomor asli (MSISDN digit-only).
// Diimplementasikan oleh *waha.Client (engine GOWS menyimpan peta LID↔nomor).
// Dipakai sebagai fallback whitelist saat payload `@lid` tak membawa nomor asli.
type LidResolver interface {
	ResolvePhoneByLid(lid string) (string, error)
}

// Auth adalah middleware pertama: parse payload, filter event message,
// cek whitelist, dan catat kontak eksternal. `lids` boleh nil (fallback resolusi
// LID→nomor lewat WAHA dilewati).
func Auth(store *db.Store, lids LidResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, _ := c.GetRawData()
		var ev model.WahaEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			log.Printf("[WEBHOOK] payload tidak bisa di-parse: %v", err)
			c.AbortWithStatusJSON(http.StatusOK, gin.H{"status": "ignored", "reason": "bad payload"})
			return
		}
		if ev.Event != "message" {
			c.AbortWithStatusJSON(http.StatusOK, gin.H{"status": "ignored", "event": ev.Event})
			return
		}
		if ev.Payload.FromMe {
			c.AbortWithStatusJSON(http.StatusOK, gin.H{"status": "ignored", "reason": "fromMe"})
			return
		}

		ctx := c.Request.Context()
		id := model.ParseFrom(ev.Payload.From)

		// Pemetaan @lid → phone: cari via nomor asli (remoteJidAlt) agar tetap
		// dikenali, simpan lid ke kontak untuk lookup berikutnya (best-effort).
		lookup := id
		if id.Kind == "lid" {
			pn := ev.AltPhone()
			// Fallback GOWS: payload @lid kerap TIDAK membawa remoteJidAlt/participantAlt,
			// sehingga AltPhone kosong dan kontak yang di-whitelist via NOMOR (mis. kontak
			// yang baru di-spawn SU, lid-nya belum tercatat) tidak dikenali → ter-BLOCK.
			// Tanyakan nomor asli ke WAHA (engine GOWS menyimpan peta LID↔nomor).
			if pn == "" && lids != nil {
				if rp, rerr := lids.ResolvePhoneByLid(id.Value); rerr != nil {
					log.Printf("[WARN] resolve LID %s ke nomor gagal: %v", id.Value, rerr)
				} else if rp != "" {
					pn = rp
					log.Printf("[LID] %s diresolusikan ke nomor %s via WAHA", id.Value, pn)
				}
			}
			if pn != "" {
				lookup = model.Identifier{Kind: "phone", Value: pn, Raw: ev.Payload.From}
			}
		}
		contact, err := store.FindContact(ctx, lookup)
		if errors.Is(err, db.ErrNotWhitelisted) && lookup.Kind != id.Kind {
			// fallback: coba lagi dengan identifier asli (mis. lid sudah terdaftar
			// di kontak walau nomor belum).
			if c2, e2 := store.FindContact(ctx, id); e2 == nil {
				contact, err = c2, nil
			}
		}
		// Backfill lid ke kontak agar audit & lookup berikutnya konsisten.
		if err == nil && id.Kind == "lid" && contact.Lid != id.Value {
			if uerr := store.SetContactLid(ctx, contact.ID, id.Value); uerr != nil {
				log.Printf("[WARN] gagal simpan lid %s ke kontak #%d: %v", id.Value, contact.ID, uerr)
			}
		}

		if errors.Is(err, db.ErrNotWhitelisted) {
			// Catat / perbarui jejak kontak external untuk mitigasi.
			reason := "not_whitelisted"
			if ec, uerr := store.UpsertExternalContact(ctx, id); uerr != nil {
				log.Printf("[ERROR] upsert external (%s): %v", ev.Payload.From, uerr)
			} else if ec.Status == "blocked" {
				reason = "external_blocked"
			}
			logAccess(store, ctx, model.AccessLog{
				Identifier: ev.Payload.From, Kind: id.Kind, Phone: phoneOf(id),
				Decision: "blocked", Reason: reason, BodyPreview: bodyPreview(ev.Payload.Body),
			})
			if reason == "external_blocked" {
				SecurityEvent("external_blocked")
			} else {
				SecurityEvent("whitelist_blocked")
			}
			log.Printf("[BLOCKED] from=%s (%s:%s) reason=%s", ev.Payload.From, id.Kind, id.Value, reason)
			c.AbortWithStatusJSON(http.StatusOK, gin.H{"status": "blocked"})
			return
		}
		if err != nil {
			log.Printf("[ERROR] cek whitelist %s: %v", ev.Payload.From, err)
			c.AbortWithStatusJSON(http.StatusOK, gin.H{"status": "error", "reason": "whitelist lookup"})
			return
		}

		c.Set(CtxEvent, &ev)
		c.Set(CtxContact, contact)
		c.Next()
	}
}

// RateLimit adalah middleware kedua: batasi pesan per kontak via Redis.
// Memakai phone kanonik kontak agar @lid & @c.us berbagi counter yang sama.
func RateLimit(limiter *db.RateLimiter, store *db.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		ev := c.MustGet(CtxEvent).(*model.WahaEvent)
		contact := c.MustGet(CtxContact).(*model.Contact)
		ok, count, err := limiter.Allow(c.Request.Context(), contact.Phone)
		if err != nil {
			log.Printf("[ERROR] rate limiter (%s): %v", contact.Phone, err)
			c.Next() // fail-open agar pesan sah tidak hilang saat Redis bermasalah.
			return
		}
		if !ok {
			cid := contact.ID
			logAccess(store, c.Request.Context(), model.AccessLog{
				Identifier: ev.Payload.From, Kind: model.ParseFrom(ev.Payload.From).Kind,
				Phone: contact.Phone, ContactID: &cid, Decision: "rate_limited",
				BodyPreview: bodyPreview(ev.Payload.Body),
			})
			SecurityEvent("rate_limited")
			log.Printf("[RATE LIMITED] phone=%s count=%d", contact.Phone, count)
			c.AbortWithStatusJSON(http.StatusOK, gin.H{"status": "rate_limited"})
			return
		}
		c.Next()
	}
}

// Pola prompt injection yang diblok (case-insensitive).
var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore\s+(all\s+)?previous\s+instructions`),
	regexp.MustCompile(`(?i)disregard\s+(your\s+)?previous`),
	regexp.MustCompile(`(?i)forget\s+(all\s+)?(your\s+)?(previous\s+)?instructions`),
	regexp.MustCompile(`(?i)you\s+are\s+now\b`),
	regexp.MustCompile(`(?i)pretend\s+to\s+be\b`),
	regexp.MustCompile(`(?i)act\s+as\s+(if|a|an)\b`),
	regexp.MustCompile(`(?i)system\s+prompt`),
	regexp.MustCompile(`(?i)reveal\s+(your\s+)?(system\s+)?prompt`),
	regexp.MustCompile(`(?i)\[INST\]`),
	regexp.MustCompile(`(?i)<\|?system\|?>`),
	regexp.MustCompile(`(?i)<\|im_start\|>`),
}

// Sanitize adalah middleware ketiga: deteksi prompt injection lalu batasi panjang.
func Sanitize(maxLen int, store *db.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		ev := c.MustGet(CtxEvent).(*model.WahaEvent)
		contact := c.MustGet(CtxContact).(*model.Contact)
		text := ev.Payload.Body

		// Kartu kontak (vCard) datang dengan body kosong; ubah jadi teks ringkas
		// (nama + nomor) agar agent bisa membacanya. Disintesis SEBELUM scan injeksi
		// & batas panjang sehingga nama dari kartu tetap melewati filter keamanan.
		if strings.TrimSpace(text) == "" {
			if ct := ev.ContactText(); ct != "" {
				text = ct
			}
		}

		for _, re := range injectionPatterns {
			if re.MatchString(text) {
				cid := contact.ID
				logAccess(store, c.Request.Context(), model.AccessLog{
					Identifier: ev.Payload.From, Kind: model.ParseFrom(ev.Payload.From).Kind,
					Phone: contact.Phone, ContactID: &cid, Decision: "injection_blocked",
					Reason: re.String(), BodyPreview: bodyPreview(text),
				})
				SecurityEvent("injection_blocked")
				log.Printf("[INJECTION BLOCKED] phone=%s pola=%q", contact.Phone, re.String())
				c.AbortWithStatusJSON(http.StatusOK, gin.H{"status": "blocked_injection"})
				return
			}
		}

		clean := strings.TrimSpace(text)
		if len([]rune(clean)) > maxLen {
			clean = string([]rune(clean)[:maxLen])
			log.Printf("[SANITIZE] pesan dipotong ke %d karakter", maxLen)
		}
		c.Set(CtxText, clean)
		c.Next()
	}
}

// AdminAuth melindungi route /admin/* dengan header X-Admin-Key (perbandingan
// constant-time). Jika ADMIN_API_KEY kosong, semua akses ditolak.
func AdminAuth(adminKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if adminKey == "" {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "admin API dinonaktifkan (ADMIN_API_KEY kosong)"})
			return
		}
		got := c.GetHeader("X-Admin-Key")
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(adminKey)) != 1 {
			SecurityEvent("admin_auth_failed")
			log.Printf("[ADMIN] akses ditolak dari %s ke %s", c.ClientIP(), c.Request.URL.Path)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}

// logAccess menulis audit log dan mencatat error tanpa mengganggu request.
func logAccess(store *db.Store, ctx context.Context, e model.AccessLog) {
	if err := store.RecordAccess(ctx, e); err != nil {
		log.Printf("[ERROR] gagal tulis access_log: %v", err)
	}
}
