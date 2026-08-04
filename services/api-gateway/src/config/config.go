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
	OpenClawAgent   string        // agent target ("pa_communicator")
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

	// Alerting : Alertmanager -> webhook gateway -> email via MS Graph.
	AlertEmailTo      string // tujuan notifikasi alert keamanan/kesehatan
	AlertWebhookToken string // Bearer token untuk POST /internal/alerts (kosong = endpoint nonaktif)

	// Kontak trusted untuk seed whitelist
	SUPhone   string
	SULid     string
	NovaPhone string
	NovaLid   string
	AdminPhone string
	AdminLid   string

	// Pengingat : menit sebelum meeting mulai untuk pengingat otomatis.
	ReminderLeadMinutes int

	// Jeda "baca" (read receipt / centang biru): sebelum menandai pesan masuk
	// sudah dibaca, bot menunggu durasi ACAK di rentang [min, max] agar terlihat
	// manusiawi. Berlaku untuk semua trust (SU, Nova, admin, eksternal).
	// Set keduanya 0 untuk menonaktifkan jeda (tandai baca seketika).
	ReadDelayMin time.Duration
	ReadDelayMax time.Duration

	// Jeda antar-kontak saat SU menyuruh pa_communicator menghubungi BEBERAPA orang
	// (>=2 SPAWN_AGENT dalam satu instruksi): orang ke-N dihubungi setelah
	// (N-1) × interval ini, agar tidak mengirim serempak (lebih manusiawi & aman
	// anti-spam). Kontak pertama tetap seketika. 0 = nonaktif (kirim serempak).
	SpawnStaggerInterval time.Duration

	// MS Graph (Calendar + Email) in-process, dipanggil saat approve.
	MSGraphTenantID     string
	MSGraphClientID     string
	MSGraphClientSecret string
	MSGraphUserUPN      string // pengirim email & pemilik calendar
	MSGraphRefreshToken string
	MailFromName        string // nama tampilan pengirim
	SignaturePath       string // path signature.html untuk email

	DocWorkDir string
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

		SUPhone:    getenv("SU_PHONE", ""),
		SULid:      getenv("SU_LID", ""),
		NovaPhone:  getenv("NOVA_PHONE", ""),
		NovaLid:    getenv("NOVA_LID", ""),
		AdminPhone: getenv("ADMIN_PHONE", ""),
		AdminLid:   getenv("ADMIN_LID", ""),

		ReminderLeadMinutes: getenvInt("MEETING_REMINDER_LEAD_MIN", 15),

		ReadDelayMin: time.Duration(getenvInt("READ_DELAY_MIN_SEC", 1)) * time.Second,
		ReadDelayMax: time.Duration(getenvInt("READ_DELAY_MAX_SEC", 30)) * time.Second,

		SpawnStaggerInterval: time.Duration(getenvInt("SPAWN_STAGGER_SEC", 60)) * time.Second,

		MSGraphTenantID:     getenv("MS_GRAPH_TENANT_ID", ""),
		MSGraphClientID:     getenv("MS_GRAPH_CLIENT_ID", ""),
		MSGraphClientSecret: getenv("MS_GRAPH_CLIENT_SECRET", ""),
		MSGraphUserUPN:      getenv("MS_GRAPH_USER_UPN", ""),
		MSGraphRefreshToken: getenv("MS_GRAPH_REFRESH_TOKEN", ""),
		MailFromName:        getenv("FROM_NAME", "PA Asisten"),
		SignaturePath:       getenv("SIGNATURE_PATH", "assets/signature.html"),

		DocWorkDir: getenv("DOC_WORK_DIR", filepath.Join(os.TempDir(), "pa_ai_outbox")),
	}
	if abs, err := filepath.Abs(cfg.DocWorkDir); err == nil {
		cfg.DocWorkDir = abs
	}
	if err := os.MkdirAll(cfg.DocWorkDir, 0o755); err != nil {
		log.Printf("[config] PERINGATAN: gagal menyiapkan DOC_WORK_DIR %s: %v — SEND_DOCUMENT jalur docPath nonaktif", cfg.DocWorkDir, err)
		cfg.DocWorkDir = ""
	} else {
		log.Printf("[config] DOC_WORK_DIR = %s", cfg.DocWorkDir)
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
	if cfg.AdminPhone == "" {
		log.Println("[config] ADMIN_PHONE kosong — agent admin nonaktif (tak ada nomor yang dipetakan ke trust 'admin').")
	} else {
		log.Printf("[config] ADMIN_PHONE di-set — agent admin aktif untuk nomor %s (trust=admin).", cfg.AdminPhone)
	}
	// Normalisasi jeda baca: negatif → 0; bila min > max, tukar agar rentang valid.
	if cfg.ReadDelayMin < 0 {
		cfg.ReadDelayMin = 0
	}
	if cfg.ReadDelayMax < 0 {
		cfg.ReadDelayMax = 0
	}
	if cfg.ReadDelayMin > cfg.ReadDelayMax {
		cfg.ReadDelayMin, cfg.ReadDelayMax = cfg.ReadDelayMax, cfg.ReadDelayMin
	}
	if cfg.ReadDelayMax == 0 {
		log.Println("[config] READ_DELAY: nonaktif — pesan masuk ditandai dibaca seketika.")
	} else {
		log.Printf("[config] READ_DELAY: pesan masuk ditandai dibaca setelah jeda acak %v–%v.",
			cfg.ReadDelayMin, cfg.ReadDelayMax)
	}
	if cfg.SpawnStaggerInterval < 0 {
		cfg.SpawnStaggerInterval = 0
	}
	if cfg.SpawnStaggerInterval == 0 {
		log.Println("[config] SPAWN_STAGGER: nonaktif — kontak keluar massal dikirim serempak.")
	} else {
		log.Printf("[config] SPAWN_STAGGER: kontak keluar ke-2 dst. ditunda kelipatan %v.", cfg.SpawnStaggerInterval)
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
