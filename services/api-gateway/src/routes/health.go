package routes

import (
	"context"
	"log"
	"time"

	"pa-ai/api-gateway/src/middleware"
)

// healthPoll = jeda antar penyegaran metrik operasional (Fase M4). 30 dtk cukup halus
// untuk mendeteksi sesi WAHA putus/tugas error tanpa membebani WAHA maupun Postgres.
const healthPoll = 30 * time.Second

// StartHealthCollector menjalankan worker latar yang menyegarkan metrik OPERASIONAL/
// KESEHATAN Prometheus (Fase M4): status sesi WAHA, jumlah tugas terjadwal per status,
// dan approval yang menunggu SU. Berbeda dari counter HTTP/keamanan (yang naik saat
// kejadian), gauge ini mencerminkan KEADAAN SAAT INI sehingga perlu di-poll berkala.
// State disimpan hanya di Prometheus; berhenti saat ctx dibatalkan.
func (h *Handler) StartHealthCollector(ctx context.Context) {
	log.Printf("[HEALTH] kolektor metrik operasional aktif (poll tiap %s)", healthPoll)
	// Sekali di awal agar metrik terisi tanpa menunggu tick pertama.
	h.collectHealth(ctx)

	t := time.NewTicker(healthPoll)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Printf("[HEALTH] kolektor metrik operasional berhenti")
				return
			case <-t.C:
				h.collectHealth(ctx)
			}
		}
	}()
}

// collectHealth mengambil sekali potret kesehatan dan menuliskannya ke gauge Prometheus.
// Tiap sumber dipantau independen: kegagalan satu (mis. WAHA tak terjangkau) tidak
// menghentikan pembacaan yang lain, dan justru TERCERMIN sebagai nilai gauge (0/absen).
func (h *Handler) collectHealth(ctx context.Context) {
	// WAHA: status sesi WhatsApp (tulang punggung kirim/terima).
	if h.Waha != nil {
		status, err := h.Waha.SessionStatus()
		if err != nil {
			middleware.SetWahaStatus(false, "")
		} else {
			middleware.SetWahaStatus(true, status)
		}
	}

	if h.Store == nil {
		return
	}

	// Tugas terjadwal per status (pending/fired/error/cancelled).
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if counts, err := h.Store.CountScheduledTasksByStatus(cctx); err != nil {
		log.Printf("[HEALTH] hitung tugas terjadwal gagal: %v", err)
	} else {
		middleware.SetScheduledTaskCounts(counts)
	}

	// Approval yang menunggu keputusan SU.
	if n, err := h.Store.CountPendingApprovals(cctx); err != nil {
		log.Printf("[HEALTH] hitung approval pending gagal: %v", err)
	} else {
		middleware.SetPendingApprovals(n)
	}

	// Statistik pool koneksi Postgres (Fase M5) — deteksi pool nyaris habis.
	acquired, idle, total, max := h.Store.PoolStats()
	middleware.SetDBPoolStats(acquired, idle, total, max)

	// Heartbeat: buktikan kolektor ini masih hidup (Fase M5).
	middleware.WorkerHeartbeat("health_collector")
}
