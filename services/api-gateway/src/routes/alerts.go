// Webhook penerima notifikasi Alertmanager (Fase M3b).
// Alertmanager mem-POST alert yang menyala ke sini; gateway merangkumnya menjadi
// email lalu mengirimkannya via MS Graph ke AlertEmailTo. Endpoint dilindungi
// Bearer token (ALERT_WEBHOOK_TOKEN); bila token kosong, endpoint dinonaktifkan.
package routes

import (
	"context"
	"crypto/subtle"
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// amAlert = satu alert dalam payload webhook Alertmanager.
type amAlert struct {
	Status      string            `json:"status"` // firing | resolved
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"`
}

// amPayload = badan webhook Alertmanager (skema v4).
type amPayload struct {
	Status string    `json:"status"`
	Alerts []amAlert `json:"alerts"`
}

// AlertWebhook menangani POST /internal/alerts dari Alertmanager.
func (h *Handler) AlertWebhook(c *gin.Context) {
	// Gerbang: token wajib. Kosong = fitur nonaktif (hindari relay email tanpa auth).
	if h.AlertWebhookToken == "" {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "alert webhook nonaktif (ALERT_WEBHOOK_TOKEN kosong)"})
		return
	}
	got := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(h.AlertWebhookToken)) != 1 {
		log.Printf("[ALERT] webhook ditolak dari %s (token salah/absen)", c.ClientIP())
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var p amPayload
	if err := c.ShouldBindJSON(&p); err != nil {
		log.Printf("[ALERT] payload tidak bisa di-parse: %v", err)
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "bad payload"})
		return
	}
	if len(p.Alerts) == 0 {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "alerts": 0})
		return
	}

	if h.Services == nil || !h.Services.Enabled() {
		log.Printf("[ALERT] MS Graph nonaktif — %d alert TIDAK dikirim via email", len(p.Alerts))
		c.JSON(http.StatusOK, gin.H{"status": "email_disabled", "alerts": len(p.Alerts)})
		return
	}

	subject := alertSubject(p)
	body := alertHTML(p)

	// Kirim email di background agar Alertmanager tidak menunggu latensi Graph
	// (menghindari retry/timeout). Kegagalan cukup dicatat di log.
	go func(to, subj, htmlBody string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.Services.SendAlert(ctx, to, "Admin PA AI", subj, htmlBody); err != nil {
			log.Printf("[ALERT] gagal kirim email ke %s: %v", to, err)
			return
		}
		log.Printf("[ALERT] email terkirim ke %s (%d alert): %s", to, len(p.Alerts), subj)
	}(h.AlertEmailTo, subject, body)

	c.JSON(http.StatusOK, gin.H{"status": "accepted", "alerts": len(p.Alerts)})
}

// alertSubject merangkum status + jenis alert untuk baris subjek email.
func alertSubject(p amPayload) string {
	status := strings.ToUpper(p.Status)
	if len(p.Alerts) == 1 {
		name := p.Alerts[0].Labels["alertname"]
		if name == "" {
			name = "Alert"
		}
		return fmt.Sprintf("[PA AI][%s] %s", status, name)
	}
	return fmt.Sprintf("[PA AI][%s] %d alert menyala", status, len(p.Alerts))
}

// alertHTML merender daftar alert menjadi email HTML sederhana. Seluruh nilai
// dari payload di-escape agar konten alert tak bisa menyuntik HTML ke email.
func alertHTML(p amPayload) string {
	var b strings.Builder
	b.WriteString(`<div style="font-family:Segoe UI,Arial,sans-serif;font-size:14px;color:#222">`)
	b.WriteString(fmt.Sprintf(`<p><strong>Status keseluruhan:</strong> %s</p>`, html.EscapeString(strings.ToUpper(p.Status))))
	for _, a := range p.Alerts {
		color := "#c62828" // firing = merah
		if a.Status == "resolved" {
			color = "#2e7d32" // resolved = hijau
		}
		name := esc(a.Labels["alertname"], "Alert")
		sev := esc(a.Labels["severity"], "-")
		pilar := esc(a.Labels["pilar"], "-")
		summary := esc(a.Annotations["summary"], "")
		desc := esc(a.Annotations["description"], "")
		when := formatAlertTime(a.StartsAt)

		b.WriteString(fmt.Sprintf(
			`<div style="border-left:4px solid %s;padding:8px 12px;margin:10px 0;background:#fafafa">`+
				`<div style="font-weight:600;color:%s">%s — %s</div>`+
				`<div>%s</div>`+
				`<div style="color:#555;margin-top:4px">%s</div>`+
				`<div style="color:#888;font-size:12px;margin-top:6px">severity: %s · pilar: %s · sejak: %s</div>`+
				`</div>`,
			color, color, name, strings.ToUpper(a.Status), summary, desc, sev, pilar, when))
	}
	b.WriteString(`<p style="color:#888;font-size:12px">Dikirim otomatis oleh PA AI Monitoring (Alertmanager).</p>`)
	b.WriteString(`</div>`)
	return b.String()
}

// esc meng-escape HTML dan memberi nilai default bila kosong.
func esc(s, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	return html.EscapeString(s)
}

// formatAlertTime mengubah timestamp RFC3339 Alertmanager ke waktu lokal Jakarta.
func formatAlertTime(iso string) string {
	if iso == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return html.EscapeString(iso)
	}
	loc, lerr := time.LoadLocation("Asia/Jakarta")
	if lerr == nil {
		t = t.In(loc)
	}
	return t.Format("02 Jan 2006 15:04 MST")
}
