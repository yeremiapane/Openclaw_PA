package memory

import (
	"strings"
	"testing"

	"pa-ai/api-gateway/src/model"
)

// Kontak baru tanpa riwayat & tanpa fakta → pesan dikirim apa adanya.
func TestBuildInjectNewContact(t *testing.T) {
	c := &Context{IsReturning: false}
	got := c.BuildInjectMessage("Halo")
	if got != "Halo" {
		t.Fatalf("kontak baru harus apa adanya, dapat %q", got)
	}
}

// Kontak kembali → preamble konteks + pesan terbaru.
func TestBuildInjectReturning(t *testing.T) {
	c := &Context{
		Contact:     &model.Contact{Name: "Rina", Company: "PT Uji Coba", TrustLevel: "trusted"},
		History:     []model.Message{{Role: "user", Text: "Halo"}, {Role: "assistant", Text: "Selamat siang"}},
		Facts:       []string{"Dari PT Uji Coba"},
		State:       "NEW_CONTACT",
		IsReturning: true,
	}
	got := c.BuildInjectMessage("Apakah nama saya tercatat?")

	for _, want := range []string{
		"[KONTEKS PERCAKAPAN", "Rina", "PT Uji Coba", "trust: trusted",
		"Fakta yang sudah diketahui", "Dari PT Uji Coba",
		"Riwayat percakapan terakhir", "[kontak] Halo", "[kamu] Selamat siang",
		"[PESAN MASUK TERBARU", "Apakah nama saya tercatat?",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("preamble tidak memuat %q\n--- hasil ---\n%s", want, got)
		}
	}
}

// Ada fakta walau belum ada riwayat → tetap pakai preamble (grounding profil).
func TestBuildInjectFactsOnly(t *testing.T) {
	c := &Context{
		Facts:       []string{"Email: rina@uji.co"},
		State:       "NEW_CONTACT",
		IsReturning: false,
	}
	got := c.BuildInjectMessage("Halo lagi")
	if !strings.Contains(got, "Email: rina@uji.co") || !strings.Contains(got, "Halo lagi") {
		t.Fatalf("preamble fakta tidak terbentuk: %q", got)
	}
}

// History melebihi historyInPrompt → hanya N terakhir yang dipakai.
func TestBuildInjectTrimsHistory(t *testing.T) {
	var hist []model.Message
	for i := 0; i < historyInPrompt+5; i++ {
		hist = append(hist, model.Message{Role: "user", Text: "pesan-lama"})
	}
	hist = append(hist, model.Message{Role: "assistant", Text: "PESAN-PALING-BARU"})
	c := &Context{History: hist, State: "NEW_CONTACT", IsReturning: true}

	got := c.BuildInjectMessage("lanjut")
	if !strings.Contains(got, "PESAN-PALING-BARU") {
		t.Fatalf("pesan terbaru harus ada di preamble")
	}
	if strings.Count(got, "pesan-lama") > historyInPrompt {
		t.Fatalf("riwayat tidak dipangkas ke %d, dapat %d", historyInPrompt, strings.Count(got, "pesan-lama"))
	}
}
