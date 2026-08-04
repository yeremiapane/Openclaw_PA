// Admin endpoints untuk kelola whitelist & memantau kontak external + audit log.
// Semua route ini dilindungi middleware.AdminAuth (header X-Admin-Key).
package routes

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	"pa-ai/api-gateway/src/db"
)

// AdminHandler menampung dependency untuk endpoint admin.
// Gateway dipakai untuk aksi yang berbagi logika dengan webhook handler
// (mis. memutuskan approval → kirim pesan tertahan).
type AdminHandler struct {
	Store   *db.Store
	Gateway *Handler
}

func qInt(c *gin.Context, key string, def int) int {
	if v := c.Query(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func qBool(c *gin.Context, key string) bool {
	v := c.Query(key)
	return v == "true" || v == "1"
}

// ─── Whitelist contacts ───────────────────────────────────────────

// ListContacts: GET /admin/contacts?include_deleted=true
func (h *AdminHandler) ListContacts(c *gin.Context) {
	includeDeleted := qBool(c, "include_deleted")
	rows, err := h.Store.ListContacts(c.Request.Context(), includeDeleted)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"contacts": rows, "count": len(rows)})
}

// AddContact: POST /admin/contacts  — hanya admin yang boleh menambah kontak.
func (h *AdminHandler) AddContact(c *gin.Context) {
	var in db.ContactInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payload tidak valid"})
		return
	}
	if in.Phone == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "field 'phone' wajib diisi"})
		return
	}
	contact, err := h.Store.AddContact(c.Request.Context(), in)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			c.JSON(http.StatusConflict, gin.H{"error": "phone/lid sudah terdaftar"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"contact": contact})
}

// UpdateContact: PATCH /admin/contacts/:phone
func (h *AdminHandler) UpdateContact(c *gin.Context) {
	var in db.ContactInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payload tidak valid"})
		return
	}
	contact, err := h.Store.UpdateContact(c.Request.Context(), c.Param("phone"), in)
	if errors.Is(err, db.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "kontak tidak ditemukan"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"contact": contact})
}

// DeleteContact: DELETE /admin/contacts/:phone  — soft-delete (data tetap ada untuk audit).
func (h *AdminHandler) DeleteContact(c *gin.Context) {
	err := h.Store.DeleteContact(c.Request.Context(), c.Param("phone"))
	if errors.Is(err, db.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "kontak tidak ditemukan"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "soft_deleted", "phone": c.Param("phone")})
}

// ─── External contacts ────────────────────────────────────────────

// ListExternal: GET /admin/external?status=pending&limit=100
func (h *AdminHandler) ListExternal(c *gin.Context) {
	rows, err := h.Store.ListExternalContacts(c.Request.Context(), c.Query("status"), qInt(c, "limit", 100))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"external_contacts": rows, "count": len(rows)})
}

// GetExternal: GET /admin/external/:identifier  — lihat profil + riwayat akses.
func (h *AdminHandler) GetExternal(c *gin.Context) {
	identifier := c.Param("identifier")
	ec, err := h.Store.GetExternalContact(c.Request.Context(), identifier)
	if errors.Is(err, db.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "external contact tidak ditemukan"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Gabungkan dengan riwayat akses terakhir untuk identifier ini.
	logs, err := h.Store.ListAccessLogs(c.Request.Context(), "", identifier, 50)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"contact": ec, "access_logs": logs})
}

// UpdateExternal: PATCH /admin/external/:identifier  — enrich profiling (email/company/address/tags/profile/risk_score).
func (h *AdminHandler) UpdateExternal(c *gin.Context) {
	var in db.ExternalInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payload tidak valid"})
		return
	}
	ec, err := h.Store.UpdateExternalContact(c.Request.Context(), c.Param("identifier"), in)
	if errors.Is(err, db.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "external contact tidak ditemukan"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"contact": ec})
}

// DeleteExternal: DELETE /admin/external/:identifier  — soft-delete (data tetap ada untuk audit).
func (h *AdminHandler) DeleteExternal(c *gin.Context) {
	err := h.Store.SoftDeleteExternalContact(c.Request.Context(), c.Param("identifier"))
	if errors.Is(err, db.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "external contact tidak ditemukan"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "soft_deleted", "identifier": c.Param("identifier")})
}

// BlockExternal: POST /admin/external/:identifier/block
func (h *AdminHandler) BlockExternal(c *gin.Context) {
	var body struct {
		Notes string `json:"notes"`
	}
	_ = c.ShouldBindJSON(&body)
	err := h.Store.SetExternalStatus(c.Request.Context(), c.Param("identifier"), "blocked", body.Notes)
	if errors.Is(err, db.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "external contact tidak ditemukan"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "blocked", "identifier": c.Param("identifier")})
}

// UnblockExternal: POST /admin/external/:identifier/unblock  — batalkan blokir (kembali 'pending').
func (h *AdminHandler) UnblockExternal(c *gin.Context) {
	var body struct {
		Notes string `json:"notes"`
	}
	_ = c.ShouldBindJSON(&body)
	err := h.Store.SetExternalStatus(c.Request.Context(), c.Param("identifier"), "pending", body.Notes)
	if errors.Is(err, db.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "external contact tidak ditemukan"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "unblocked", "identifier": c.Param("identifier")})
}

// ─── Block/unblock cepat by phone (mode open) ─────────────────────

// BlockPhone: POST /admin/block  body {"phone":"628xxx","notes":"..."}
// Blokir menyeluruh untuk WHITELIST_MODE=open: cabut dari whitelist + tandai
// 'blocked' di external_contacts (bentuk @c.us dan @lid). Satu panggilan.
func (h *AdminHandler) BlockPhone(c *gin.Context) {
	var body struct {
		Phone string `json:"phone"`
		Notes string `json:"notes"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Phone == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "field 'phone' wajib diisi"})
		return
	}
	blocked, err := h.Store.BlockContactByPhone(c.Request.Context(), body.Phone, body.Notes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "blocked", "phone": body.Phone, "identifiers": blocked})
}

// UnblockPhone: POST /admin/unblock  body {"phone":"628xxx"}
// Batalkan blokir by phone (kembalikan 'pending'). Di mode open, pesan berikutnya
// akan auto-whitelist ulang.
func (h *AdminHandler) UnblockPhone(c *gin.Context) {
	var body struct {
		Phone string `json:"phone"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Phone == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "field 'phone' wajib diisi"})
		return
	}
	unblocked, err := h.Store.UnblockContactByPhone(c.Request.Context(), body.Phone)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "unblocked", "phone": body.Phone, "identifiers": unblocked})
}

// PromoteExternal: POST /admin/external/:identifier/promote  — pindahkan ke whitelist.
func (h *AdminHandler) PromoteExternal(c *gin.Context) {
	var in db.ContactInput
	_ = c.ShouldBindJSON(&in)
	contact, err := h.Store.PromoteExternal(c.Request.Context(), c.Param("identifier"), in)
	if errors.Is(err, db.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "external contact tidak ditemukan"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "promoted", "contact": contact})
}

// ─── Audit logs ───────────────────────────────────────────────────

// ListLogs: GET /admin/logs?decision=blocked&identifier=...&limit=100
func (h *AdminHandler) ListLogs(c *gin.Context) {
	rows, err := h.Store.ListAccessLogs(c.Request.Context(), c.Query("decision"), c.Query("identifier"), qInt(c, "limit", 100))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": rows, "count": len(rows)})
}

// ─── Approval gate ───────────────────────────────────────

// ListApprovals: GET /admin/approvals?status=pending&limit=50
func (h *AdminHandler) ListApprovals(c *gin.Context) {
	rows, err := h.Store.ListApprovals(c.Request.Context(), c.Query("status"), qInt(c, "limit", 50))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"approvals": rows, "count": len(rows)})
}

// ApproveApproval: POST /admin/approvals/:id/approve
func (h *AdminHandler) ApproveApproval(c *gin.Context) { h.decideApproval(c, true) }

// RejectApproval: POST /admin/approvals/:id/reject
func (h *AdminHandler) RejectApproval(c *gin.Context) { h.decideApproval(c, false) }

func (h *AdminHandler) decideApproval(c *gin.Context, approve bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id tidak valid"})
		return
	}
	msg, err := h.Gateway.DecideApproval(c.Request.Context(), id, approve)
	if errors.Is(err, db.ErrApprovalNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": msg})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": msg})
		return
	}
	status := "rejected"
	if approve {
		status = "approved"
	}
	c.JSON(http.StatusOK, gin.H{"status": status, "message": msg})
}

// ─── Observability & evaluasi (Fase 8.5) ──────────────────────────

// ListExecutions: GET /admin/executions?conversation=<id>&limit=50
// Trace per giliran agent: token, model, durasi, actions, outcome.
func (h *AdminHandler) ListExecutions(c *gin.Context) {
	rows, err := h.Store.ListExecutions(c.Request.Context(), c.Query("conversation"), qInt(c, "limit", 50))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"executions": rows, "count": len(rows)})
}

// Usage: GET /admin/usage?conversation=<id>&limit=100
// Agregat token per conversation (untuk evaluasi biaya/efisiensi model).
func (h *AdminHandler) Usage(c *gin.Context) {
	rows, err := h.Store.UsageByConversation(c.Request.Context(), c.Query("conversation"), qInt(c, "limit", 100))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"usage": rows, "count": len(rows)})
}

// ListOutbound: GET /admin/outbound?conversation=<id>&limit=50
// Jejak pesan yang dikirim/ditahan bot.
func (h *AdminHandler) ListOutbound(c *gin.Context) {
	rows, err := h.Store.ListOutbound(c.Request.Context(), c.Query("conversation"), qInt(c, "limit", 50))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"outbound": rows, "count": len(rows)})
}

// ListMeetings: GET /admin/meetings?status=pending&limit=50
func (h *AdminHandler) ListMeetings(c *gin.Context) {
	rows, err := h.Store.ListMeetings(c.Request.Context(), c.Query("status"), qInt(c, "limit", 50))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"meetings": rows, "count": len(rows)})
}

// MeetingHistory: GET /admin/meetings/:id/history
func (h *AdminHandler) MeetingHistory(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id tidak valid"})
		return
	}
	rows, err := h.Store.MeetingHistory(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"history": rows, "count": len(rows)})
}

// ResendRSVP: POST /admin/meetings/:id/resend-rsvp — kirim ulang email undangan (RSVP + .ics)
// untuk meeting yang sudah dijadwalkan, tanpa membuat event kalender baru. Untuk memulihkan
// kegagalan pengiriman email.
func (h *AdminHandler) ResendRSVP(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id tidak valid"})
		return
	}
	if err := h.Gateway.ResendMeetingRSVP(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "meeting_id": id})
}
