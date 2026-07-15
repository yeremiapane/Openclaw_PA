//go:build integration

// Integration test untuk penyesuaian PERSONA orchestrator (UPDATE_AGENT_PERSONA):
// simpan/ganti/hapus overlay, snapshot [PREFERENSI GAYA & PERILAKU] + guard header,
// batas panjang, dan gerbang keamanan (SU-only + agent yang diizinkan).
// Berjalan terhadap Postgres ASLI memakai kode terkompilasi yang sama seperti gateway —
// TANPA mengirim WhatsApp. Persona orchestrator yang ada DIPULIHKAN di akhir (non-destruktif).
// Jalankan:
//
//	go test -tags integration ./src/routes/ -run TestAgentPersonaUpdateAndSnapshot -v
package routes

import (
	"context"
	"strings"
	"testing"

	"github.com/joho/godotenv"

	"pa-ai/api-gateway/src/config"
	"pa-ai/api-gateway/src/db"
	"pa-ai/api-gateway/src/model"
	"pa-ai/api-gateway/src/waha"
)

func TestAgentPersonaUpdateAndSnapshot(t *testing.T) {
	ctx := context.Background()
	_ = godotenv.Load("../../../../.env")
	cfg := config.Load()

	store, err := db.NewStore(ctx, cfg)
	if err != nil {
		t.Fatalf("koneksi Postgres gagal: %v", err)
	}
	defer store.Close()

	h := &Handler{
		Store:   store,
		Waha:    waha.New(cfg.WahaURL, cfg.WahaAPIKey, cfg.WahaSession),
		SUPhone: "628100000001",
	}

	// Non-destruktif: simpan persona orchestrator yang ada, pulihkan di akhir.
	orig, _ := store.GetAgentPrefs(ctx, personaAgent)
	defer func() { _ = store.SetAgentPrefs(ctx, personaAgent, orig) }()

	su := &model.Contact{Phone: h.SUPhone, TrustLevel: "su", Name: "Pak Sudianto (SU)"}

	// ---- 1. SU menyetel persona → tersimpan & muncul di snapshot dgn guard header ----
	h.updateAgentPersona(ctx, su, model.Action{
		Type: "UPDATE_AGENT_PERSONA", PersonaText: "Panggil beliau 'Pak Sudi'. Nada santai. Tanpa emoji.",
	})
	got, err := store.GetAgentPrefs(ctx, personaAgent)
	if err != nil {
		t.Fatalf("#1: GetAgentPrefs gagal: %v", err)
	}
	if !strings.Contains(got, "Pak Sudi") {
		t.Fatalf("#1: prefs tak tersimpan: %q", got)
	}
	snap := h.buildPersonaSnapshot(ctx)
	if !strings.Contains(snap, "PREFERENSI GAYA & PERILAKU") || !strings.Contains(snap, "Pak Sudi") {
		t.Fatalf("#1: snapshot tak memuat header/isi: %q", snap)
	}
	if !strings.Contains(snap, "TIDAK PERNAH menimpa") {
		t.Fatalf("#1: snapshot tak memuat guard keamanan: %q", snap)
	}

	// ---- 2. Set ulang = GANTI seutuhnya (bukan menumpuk) ----
	h.updateAgentPersona(ctx, su, model.Action{
		Type: "UPDATE_AGENT_PERSONA", PersonaText: "Formal dan ringkas. Bahasa Indonesia baku.",
	})
	got, _ = store.GetAgentPrefs(ctx, personaAgent)
	if strings.Contains(got, "Pak Sudi") || !strings.Contains(got, "Formal dan ringkas") {
		t.Fatalf("#2: set ulang tidak mengganti seutuhnya: %q", got)
	}

	// ---- 3. Batas panjang: teks > personaMaxRunes dipotong ----
	long := strings.Repeat("x", personaMaxRunes+500)
	h.updateAgentPersona(ctx, su, model.Action{Type: "UPDATE_AGENT_PERSONA", PersonaText: long})
	got, _ = store.GetAgentPrefs(ctx, personaAgent)
	if n := len([]rune(got)); n != personaMaxRunes {
		t.Fatalf("#3: panjang tersimpan %d, ingin %d (dipotong)", n, personaMaxRunes)
	}

	// ---- 4. Hapus: personaText kosong → prefs kosong & snapshot kosong ----
	h.updateAgentPersona(ctx, su, model.Action{Type: "UPDATE_AGENT_PERSONA", PersonaText: ""})
	got, _ = store.GetAgentPrefs(ctx, personaAgent)
	if strings.TrimSpace(got) != "" {
		t.Fatalf("#4: prefs tidak terhapus: %q", got)
	}
	if snap := h.buildPersonaSnapshot(ctx); snap != "" {
		t.Fatalf("#4: snapshot harus kosong saat tak ada preferensi, dapat: %q", snap)
	}

	// ---- 5. Gerbang: inisiator NON-SU tidak boleh mengubah persona ----
	h.updateAgentPersona(ctx, su, model.Action{Type: "UPDATE_AGENT_PERSONA", PersonaText: "penanda-milik-SU"})
	ext := &model.Contact{Phone: "628100000009", TrustLevel: "external"}
	h.updateAgentPersona(ctx, ext, model.Action{Type: "UPDATE_AGENT_PERSONA", PersonaText: "diretas-eksternal"})
	got, _ = store.GetAgentPrefs(ctx, personaAgent)
	if got != "penanda-milik-SU" {
		t.Fatalf("#5: non-SU berhasil mengubah persona (bocor): %q", got)
	}

	// ---- 6. Gerbang: agent selain orchestrator ditolak (tak ada baris untuknya) ----
	h.updateAgentPersona(ctx, su, model.Action{
		Type: "UPDATE_AGENT_PERSONA", TargetAgent: "pa_communicator", PersonaText: "coba-setel-communicator",
	})
	if p, _ := store.GetAgentPrefs(ctx, "pa_communicator"); p != "" {
		t.Fatalf("#6: agent non-orchestrator berhasil disetel (bocor): %q", p)
	}

	t.Logf("OK: persona set/ganti/potong/hapus + snapshot berguard + gerbang SU-only & agent bekerja")
}
