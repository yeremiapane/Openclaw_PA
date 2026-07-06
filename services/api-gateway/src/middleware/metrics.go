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
)

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
