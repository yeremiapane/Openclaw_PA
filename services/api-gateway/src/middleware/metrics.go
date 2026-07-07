// Instrumentasi Prometheus (Fase M1 — monitoring).
// Menyediakan gin middleware yang mencatat jumlah & durasi tiap HTTP request, serta
// handler /metrics untuk di-scrape Prometheus. Metrik proses (memori/CPU, runtime Go)
// otomatis diekspos oleh client_golang lewat registry default.
package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// httpReqTotal = jumlah request per rute/metode/status. Label "path" memakai POLA
	// rute (mis. "/webhook/waha"), BUKAN URL mentah, agar kardinalitas tetap rendah.
	httpReqTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_http_requests_total",
			Help: "Total HTTP request yang ditangani gateway, per rute/metode/status.",
		},
		[]string{"method", "path", "status"},
	)
	// httpReqDuration = histogram durasi penanganan request (detik) per rute/metode.
	// Bucket default (5ms..10s) memadai untuk endpoint webhook & admin.
	httpReqDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gateway_http_request_duration_seconds",
			Help:    "Durasi penanganan HTTP request gateway dalam detik.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)
	// securityEvents = jumlah keputusan security layer yang MEMBLOKIR/menolak, per jenis
	// (Fase M3). Label "kind" bernilai tetap & kardinalitas rendah (whitelist_blocked,
	// external_blocked, rate_limited, injection_blocked, admin_auth_failed) agar aman untuk
	// alerting. Detail kaya (nomor, potongan pesan, pola) tetap di tabel access_logs.
	securityEvents = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_security_events_total",
			Help: "Total peristiwa keamanan (blokir/tolak) gateway, per jenis.",
		},
		[]string{"kind"},
	)

	// ─── Fase M4 — Metrik operasional/kesehatan (gauge, di-refresh kolektor) ───
	// Berbeda dari counter di atas: ini gauge yang MENCERMINKAN KEADAAN SAAT INI,
	// diperbarui berkala oleh StartHealthCollector (poll WAHA + hitung Postgres).

	// wahaSessionUp = 1 bila sesi WhatsApp (WAHA) berstatus WORKING, selain itu 0.
	// Jalur WhatsApp adalah tulang punggung sistem — bila 0, bot tak bisa kirim/terima.
	wahaSessionUp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_waha_session_up",
		Help: "Status sesi WAHA: 1 = WORKING, 0 = selain itu.",
	})
	// wahaReachable = 1 bila endpoint WAHA bisa dihubungi (HTTP OK) apa pun statusnya,
	// 0 bila tak terjangkau. Memisahkan 'WAHA mati' dari 'sesi belum WORKING'.
	wahaReachable = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_waha_reachable",
		Help: "Keterjangkauan endpoint WAHA: 1 = terhubung, 0 = tak terjangkau.",
	})
	// scheduledTasks = jumlah tugas terjadwal PER STATUS (pending/fired/error/cancelled).
	// 'error' > 0 menandakan pengingat/digest gagal terkirim dan perlu perhatian.
	scheduledTasks = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_scheduled_tasks",
		Help: "Jumlah tugas terjadwal (pengingat/digest) per status saat ini.",
	}, []string{"status"})
	// pendingApprovals = jumlah pesan keluar yang MENUNGGU persetujuan SU (approval gate).
	// Menumpuk = SU perlu meninjau; alat bantu SLA persetujuan.
	pendingApprovals = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_pending_approvals",
		Help: "Jumlah approval (pesan keluar) yang masih menunggu keputusan SU.",
	})

	// ─── Fase M5 — Agent/LLM, pengiriman keluar, worker, pool DB ───────────────

	// agentCalls = jumlah giliran agent per agent & HASIL (outcome: ok/no_reply/
	// parse_error/error/…). Label kardinalitas rendah (himpunan agent & outcome tetap).
	// Inilah sinyal utama untuk mendeteksi kontaminasi sesi OpenClaw (parse_error).
	agentCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_agent_calls_total",
		Help: "Total giliran agent, per agent & hasil (outcome).",
	}, []string{"agent", "outcome"})
	// agentDuration = histogram durasi satu giliran agent (detik). Bucket lebih lebar
	// dari HTTP karena panggilan LLM berskala detik–menit.
	agentDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_agent_duration_seconds",
		Help:    "Durasi satu giliran agent (detik).",
		Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 45, 60, 90, 120},
	}, []string{"agent"})
	// agentTokens = total token per JENIS (input/output/cache_read/cache_write). Basis
	// pemantauan pemakaian & anggaran (API usage).
	agentTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_agent_tokens_total",
		Help: "Total token agent, per jenis (input/output/cache_read/cache_write).",
	}, []string{"type"})
	// agentFallbacks / agentRefusals = flag ortogonal terhadap outcome; dipisah agar
	// bisa dialert langsung (lonjakan fallback = model utama bermasalah; refusal = agent menolak).
	agentFallbacks = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_agent_fallbacks_total",
		Help: "Total giliran agent yang memakai model fallback.",
	})
	agentRefusals = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_agent_refusals_total",
		Help: "Total giliran agent yang berujung penolakan (refusal).",
	})

	// outboundMessages = jumlah pesan keluar per JENIS & STATUS (sent/failed/held).
	// 'failed' naik = pengiriman WhatsApp bermasalah walau sesi WAHA sehat.
	outboundMessages = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_outbound_messages_total",
		Help: "Total pesan keluar gateway, per jenis & status (sent/failed/held).",
	}, []string{"kind", "status"})

	// workerLastRun = timestamp Unix (detik) terakhir kali sebuah worker latar
	// MENYELESAIKAN satu siklus. Alert 'basi' (now - nilai > ambang) menangkap
	// kematian senyap goroutine (scheduler/email-watcher/health-collector).
	workerLastRun = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_worker_last_run_timestamp_seconds",
		Help: "Unix timestamp siklus terakhir tiap worker latar.",
	}, []string{"worker"})

	// dbPoolConnections = statistik pool koneksi Postgres (pgxpool) per keadaan
	// (acquired/idle/total/max). 'acquired' mendekati 'max' = pool nyaris habis.
	dbPoolConnections = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_db_pool_connections",
		Help: "Statistik pool koneksi Postgres per keadaan (acquired/idle/total/max).",
	}, []string{"state"})
)

// RecordAgentCall mencatat satu giliran agent ke metrik Prometheus (Fase M5): naikkan
// counter per outcome, amati durasi, tambah token per jenis, dan flag fallback/refusal.
// Dipanggil dari satu titik tunggal (logExecution) agar konsisten dengan tabel audit.
func RecordAgentCall(agent, outcome string, durationMs int, inTok, outTok, cacheReadTok, cacheWriteTok int, fallback, refusal bool) {
	if agent == "" {
		agent = "unknown"
	}
	if outcome == "" {
		outcome = "unknown"
	}
	agentCalls.WithLabelValues(agent, outcome).Inc()
	if durationMs > 0 {
		agentDuration.WithLabelValues(agent).Observe(float64(durationMs) / 1000.0)
	}
	if inTok > 0 {
		agentTokens.WithLabelValues("input").Add(float64(inTok))
	}
	if outTok > 0 {
		agentTokens.WithLabelValues("output").Add(float64(outTok))
	}
	if cacheReadTok > 0 {
		agentTokens.WithLabelValues("cache_read").Add(float64(cacheReadTok))
	}
	if cacheWriteTok > 0 {
		agentTokens.WithLabelValues("cache_write").Add(float64(cacheWriteTok))
	}
	if fallback {
		agentFallbacks.Inc()
	}
	if refusal {
		agentRefusals.Inc()
	}
}

// RecordOutbound menaikkan counter pesan keluar per jenis & status (Fase M5).
// Dipanggil dari sendAndRecord (titik tunggal penentu status kirim).
func RecordOutbound(kind, status string) {
	if kind == "" {
		kind = "unknown"
	}
	if status == "" {
		status = "unknown"
	}
	outboundMessages.WithLabelValues(kind, status).Inc()
}

// WorkerHeartbeat menandai bahwa worker latar `name` baru menyelesaikan satu siklus
// (Fase M5). Dipanggil di tiap iterasi loop worker.
func WorkerHeartbeat(name string) {
	workerLastRun.WithLabelValues(name).Set(float64(time.Now().Unix()))
}

// SetDBPoolStats memperbarui gauge statistik pool koneksi Postgres (Fase M5).
func SetDBPoolStats(acquired, idle, total, max int32) {
	dbPoolConnections.WithLabelValues("acquired").Set(float64(acquired))
	dbPoolConnections.WithLabelValues("idle").Set(float64(idle))
	dbPoolConnections.WithLabelValues("total").Set(float64(total))
	dbPoolConnections.WithLabelValues("max").Set(float64(max))
}

// canonicalTaskStatuses = daftar status tugas terjadwal yang selalu di-set (walau 0)
// agar seri tak "stale"/menghilang saat count turun ke nol — penting untuk grafik & alert.
var canonicalTaskStatuses = []string{"pending", "fired", "error", "cancelled"}

// SetWahaStatus memperbarui gauge kesehatan WAHA (Fase M4). reachable=false berarti
// endpoint tak terjangkau; status="WORKING" menandakan sesi siap kirim/terima.
func SetWahaStatus(reachable bool, status string) {
	if reachable {
		wahaReachable.Set(1)
	} else {
		wahaReachable.Set(0)
	}
	if reachable && status == "WORKING" {
		wahaSessionUp.Set(1)
	} else {
		wahaSessionUp.Set(0)
	}
}

// SetScheduledTaskCounts memperbarui gauge jumlah tugas terjadwal per status (Fase M4).
// Status kanonis selalu di-set (0 bila tak ada) agar seri tetap muncul; status di luar
// daftar kanonis (bila ada) ikut disetel apa adanya.
func SetScheduledTaskCounts(counts map[string]int) {
	for _, st := range canonicalTaskStatuses {
		scheduledTasks.WithLabelValues(st).Set(float64(counts[st]))
	}
	for st, n := range counts {
		if !containsStr(canonicalTaskStatuses, st) {
			scheduledTasks.WithLabelValues(st).Set(float64(n))
		}
	}
}

// SetPendingApprovals memperbarui gauge jumlah approval yang menunggu SU (Fase M4).
func SetPendingApprovals(n int) { pendingApprovals.Set(float64(n)) }

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// SecurityEvent menaikkan counter peristiwa keamanan berjenis kind (Fase M3).
// Dipanggil dari titik penolakan security layer (whitelist, rate limit, injeksi,
// auth admin). Dieskpor agar paket lain (mis. gerbang SU-only) bisa ikut memakainya.
func SecurityEvent(kind string) {
	securityEvents.WithLabelValues(kind).Inc()
}

// Metrics = gin middleware yang mencatat jumlah & durasi tiap request untuk Prometheus.
// Rute /metrics sendiri dilewati agar scrape tak mengotori statistik. Request yang tak
// cocok rute apa pun (404) dilabeli "unmatched" untuk mencegah ledakan kardinalitas dari
// pemindaian URL acak.
func Metrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Path == "/metrics" {
			c.Next()
			return
		}
		start := time.Now()
		c.Next()

		path := c.FullPath()
		if path == "" {
			path = "unmatched"
		}
		httpReqTotal.WithLabelValues(c.Request.Method, path, strconv.Itoa(c.Writer.Status())).Inc()
		httpReqDuration.WithLabelValues(c.Request.Method, path).Observe(time.Since(start).Seconds())
	}
}

// MetricsHandler = handler untuk GET /metrics (format eksposisi Prometheus). Membungkus
// promhttp.Handler() agar cocok dengan signature gin. Endpoint ini TIDAK memuat rahasia,
// tetapi JANGAN diekspos ke internet — cukup dijangkau Prometheus di jaringan lokal.
func MetricsHandler() gin.HandlerFunc {
	h := promhttp.Handler()
	return func(c *gin.Context) {
		h.ServeHTTP(c.Writer, c.Request)
	}
}
