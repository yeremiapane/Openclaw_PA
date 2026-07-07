// Package config memuat konfigurasi API Gateway dari environment variables.
package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config menampung seluruh setting yang dibaca dari .env / environment.
type Config struct {
	Port        string // port HTTP API Gateway (default 4000)
	WahaURL     string // base URL WAHA (dari host: http://localhost:13000)
	WahaAPIKey  string // X-Api-Key untuk WAHA
	WahaSession string // nama session WAHA (WAHA Core wajib "default")
	OpenClawURL string // base URL OpenClaw (legacy; tak dipakai — OpenClaw via CLI)

	// OpenClaw via CLI shell-out
	OpenClawBin     string        // binary openclaw (default "openclaw" di PATH)
	OpenClawNode    string        // path node (mode node+script, default "node")
	OpenClawScript  string        // path openclaw.mjs; kosong → auto-deteksi di Windows
	OpenClawAgent   string        // agent target (Fase 6: "pa_communicator")
	OpenClawTimeout time.Duration // batas waktu satu turn agent

	// Database / cache
	DBHost    string
	DBPort    string
	DBUser    string
	DBPass    string
	DBName    string
	RedisAddr string

	// Security
	RateLimitMax    int           // maks pesan per window per kontak
	RateLimitWindow time.Duration // panjang window rate limit
	MaxMsgLen       int           // batas panjang body pesan
	AdminAPIKey     string        // melindungi endpoint /admin/*
	// WhitelistMode mengatur perlakuan nomor TAK DIKENAL:
	//   "strict" (default) — hanya kontak whitelist yang dilayani; sisanya diblokir.
	//   "open"             — semua penelepon otomatis di-whitelist sbg 'external' &
	//                        dilayani pa_communicator (kecuali yang diblokir admin).
	// SU & Nova tetap bersumber dari .env (di-seed) apa pun modenya.
	WhitelistMode string

	// Alerting (Fase M3b): Alertmanager -> webhook gateway -> email via MS Graph.
	AlertEmailTo      string // tujuan notifikasi alert keamanan/kesehatan
	AlertWebhookToken string // Bearer token untuk POST /internal/alerts (kosong = endpoint nonaktif)

	// Kontak trusted untuk seed whitelist
	SUPhone   string
	SULid     string
	NovaPhone string
	NovaLid   string

	// Pengingat (Fase A): menit sebelum meeting mulai untuk pengingat otomatis.
	ReminderLeadMinutes int

	// MS Graph (Calendar + Email) in-process, dipanggil saat approve.
	MSGraphTenantID     string
	MSGraphClientID     string
	MSGraphClientSecret string
	MSGraphUserUPN      string // pengirim email & pemilik calendar
	// MSGraphRefreshToken = refresh token DELEGATED (opsional) untuk membaca email
	// (Email Watch). Dipakai sebagai FALLBACK bila izin aplikasi (Mail.Read app) belum
	// ada / ditolak. Kosong = hanya andalkan izin aplikasi.
	MSGraphRefreshToken string
	MailFromName        string // nama tampilan pengirim
	SignaturePath       string // path signature.html untuk email
}

// DBConnString merakit DSN PostgreSQL dari komponen Config.
func (c Config) DBConnString() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		c.DBUser, c.DBPass, c.DBHost, c.DBPort, c.DBName)
}

// Load membaca .env lalu merakit Config dari environment.
func Load() Config {
	candidates := []string{
		".env",
		filepath.Join("..", "..", ".env"),
		filepath.Join("..", "..", "..", ".env"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			if err := godotenv.Load(p); err == nil {
				log.Printf("[config] .env dimuat dari %s", p)
				break
			}
		}
	}

	cfg := Config{
		Port:        getenv("GATEWAY_PORT", "4000"),
		WahaURL:     getenv("WAHA_URL", "http://localhost:13000"),
		WahaAPIKey:  getenv("WAHA_API_KEY", ""),
		WahaSession: getenv("WAHA_SESSION", "default"),
		OpenClawURL: getenv("OPENCLAW_URL", "http://localhost:5173"),

		OpenClawBin:     getenv("OPENCLAW_BIN", "openclaw"),
		OpenClawNode:    getenv("OPENCLAW_NODE", "node"),
		OpenClawScript:  getenv("OPENCLAW_SCRIPT", ""),
		OpenClawAgent:   getenv("OPENCLAW_AGENT", "pa_communicator"),
		OpenClawTimeout: time.Duration(getenvInt("OPENCLAW_TIMEOUT_SEC", 180)) * time.Second,

		DBHost:    getenv("DB_HOST", "localhost"),
		DBPort:    getenv("DB_PORT", "5432"),
		DBUser:    getenv("DB_USER", "pa_ai"),
		DBPass:    getenv("DB_PASS", ""),
		DBName:    getenv("DB_NAME", "pa_ai"),
		RedisAddr: getenv("REDIS_ADDR", "localhost:6379"),

		RateLimitMax:    getenvInt("RATE_LIMIT_MAX", 20),
		RateLimitWindow: time.Duration(getenvInt("RATE_LIMIT_WINDOW_SEC", 60)) * time.Second,
		MaxMsgLen:       getenvInt("MAX_MSG_LEN", 2000),
		AdminAPIKey:     getenv("ADMIN_API_KEY", ""),
		WhitelistMode:   normalizeWhitelistMode(getenv("WHITELIST_MODE", "strict")),

		AlertEmailTo:      getenv("ALERT_EMAIL_TO", "yeremia.yosefan@hypernet.co.id"),
		AlertWebhookToken: getenv("ALERT_WEBHOOK_TOKEN", ""),

		SUPhone:   getenv("SU_PHONE", ""),
		SULid:     getenv("SU_LID", ""),
		NovaPhone: getenv("NOVA_PHONE", ""),
		NovaLid:   getenv("NOVA_LID", ""),

		ReminderLeadMinutes: getenvInt("MEETING_REMINDER_LEAD_MIN", 15),

		MSGraphTenantID:     getenv("MS_GRAPH_TENANT_ID", ""),
		MSGraphClientID:     getenv("MS_GRAPH_CLIENT_ID", ""),
		MSGraphClientSecret: getenv("MS_GRAPH_CLIENT_SECRET", ""),
		MSGraphUserUPN:      getenv("MS_GRAPH_USER_UPN", ""),
		MSGraphRefreshToken: getenv("MS_GRAPH_REFRESH_TOKEN", ""),
		MailFromName:        getenv("FROM_NAME", "PA Asisten"),
		SignaturePath:       getenv("SIGNATURE_PATH", "assets/signature.html"),
	}

	if cfg.WahaAPIKey == "" {
		log.Println("[config] PERINGATAN: WAHA_API_KEY kosong — panggilan ke WAHA akan 401")
	}
	if cfg.AdminAPIKey == "" {
		log.Println("[config] PERINGATAN: ADMIN_API_KEY kosong — endpoint /admin/* akan ditolak total")
	}
	if cfg.MSGraphTenantID == "" || cfg.MSGraphClientID == "" || cfg.MSGraphClientSecret == "" || cfg.MSGraphUserUPN == "" {
		log.Println("[config] PERINGATAN: kredensial MS Graph belum lengkap — penjadwalan meeting (Calendar/Email) nonaktif")
	}
	if cfg.WhitelistMode == "open" {
		log.Println("[config] WHITELIST_MODE=open — SEMUA nomor tak dikenal akan otomatis di-whitelist & dilayani pa_communicator (kecuali yang diblokir admin).")
	} else {
		log.Println("[config] WHITELIST_MODE=strict — hanya kontak whitelist yang dilayani.")
	}
	return cfg
}

// normalizeWhitelistMode memvalidasi nilai WHITELIST_MODE. Nilai tak dikenal
// jatuh ke "strict" (default aman) disertai peringatan.
func normalizeWhitelistMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "open":
		return "open"
	case "strict", "":
		return "strict"
	default:
		log.Printf("[config] WHITELIST_MODE=%q tak dikenal — pakai \"strict\"", v)
		return "strict"
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("[config] %s bukan integer valid (%q), pakai default %d", key, v, fallback)
	}
	return fallback
}
