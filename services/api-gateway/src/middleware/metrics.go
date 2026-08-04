// Instrumentasi Prometheus (monitoring).
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
	securityEvents = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_security_events_total",
			Help: "Total peristiwa keamanan (blokir/tolak) gateway, per jenis.",
		},
		[]string{"kind"},
	)

	wahaSessionUp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_waha_session_up",
		Help: "Status sesi WAHA: 1 = WORKING, 0 = selain itu.",
	})
	wahaReachable = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_waha_reachable",
		Help: "Keterjangkauan endpoint WAHA: 1 = terhubung, 0 = tak terjangkau.",
	})
	scheduledTasks = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_scheduled_tasks",
		Help: "Jumlah tugas terjadwal (pengingat/digest) per status saat ini.",
	}, []string{"status"})
	pendingApprovals = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_pending_approvals",
		Help: "Jumlah approval (pesan keluar) yang masih menunggu keputusan SU.",
	})
	agentCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_agent_calls_total",
		Help: "Total giliran agent, per agent & hasil (outcome).",
	}, []string{"agent", "outcome"})
	agentDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_agent_duration_seconds",
		Help:    "Durasi satu giliran agent (detik).",
		Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 45, 60, 90, 120},
	}, []string{"agent"})
	agentTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_agent_tokens_total",
		Help: "Total token agent, per jenis (input/output/cache_read/cache_write).",
	}, []string{"type"})
	agentFallbacks = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_agent_fallbacks_total",
		Help: "Total giliran agent yang memakai model fallback.",
	})
	agentRefusals = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_agent_refusals_total",
		Help: "Total giliran agent yang berujung penolakan (refusal).",
	})

	outboundMessages = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_outbound_messages_total",
		Help: "Total pesan keluar gateway, per jenis & status (sent/failed/held).",
	}, []string{"kind", "status"})

	workerLastRun = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_worker_last_run_timestamp_seconds",
		Help: "Unix timestamp siklus terakhir tiap worker latar.",
	}, []string{"worker"})

	dbPoolConnections = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_db_pool_connections",
		Help: "Statistik pool koneksi Postgres per keadaan (acquired/idle/total/max).",
	}, []string{"state"})
)

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

func RecordOutbound(kind, status string) {
	if kind == "" {
		kind = "unknown"
	}
	if status == "" {
		status = "unknown"
	}
	outboundMessages.WithLabelValues(kind, status).Inc()
}

func WorkerHeartbeat(name string) {
	workerLastRun.WithLabelValues(name).Set(float64(time.Now().Unix()))
}

// SetDBPoolStats memperbarui gauge statistik pool koneksi Postgres.
func SetDBPoolStats(acquired, idle, total, max int32) {
	dbPoolConnections.WithLabelValues("acquired").Set(float64(acquired))
	dbPoolConnections.WithLabelValues("idle").Set(float64(idle))
	dbPoolConnections.WithLabelValues("total").Set(float64(total))
	dbPoolConnections.WithLabelValues("max").Set(float64(max))
}

var canonicalTaskStatuses = []string{"pending", "fired", "error", "cancelled"}

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

// SetPendingApprovals memperbarui gauge jumlah approval yang menunggu SU.
func SetPendingApprovals(n int) { pendingApprovals.Set(float64(n)) }

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// SecurityEvent menaikkan counter peristiwa keamanan berjenis kind.
func SecurityEvent(kind string) {
	securityEvents.WithLabelValues(kind).Inc()
}

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

func MetricsHandler() gin.HandlerFunc {
	h := promhttp.Handler()
	return func(c *gin.Context) {
		h.ServeHTTP(c.Writer, c.Request)
	}
}
