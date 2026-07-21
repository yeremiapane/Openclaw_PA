package routes

import (
	"strings"
	"testing"
	"time"

	"pa-ai/api-gateway/src/model"
)

// TestDeferDelay menjaga batas bawah jeda tugas berat. Ini bukan sekadar validasi
// input: jeda di bawah jendela susun-awal scheduler (schedulerPrepLead) membuat worker
// menyambar tugas pada saat yang sama dengan giliran percakapan yang membuatnya —
// dua giliran orchestrator pada satu sesi sekaligus, dan laporan hasil bisa tiba
// mendahului balasan "baik, saya kerjakan" yang memicunya.
func TestDeferDelay(t *testing.T) {
	kasus := []struct {
		nama  string
		menit int
		harap time.Duration
	}{
		{"nol dinaikkan ke lantai", 0, minDeferDelay},
		{"negatif dinaikkan ke lantai", -30, minDeferDelay},
		{"di bawah lantai dinaikkan", 1, minDeferDelay},
		{"nilai wajar dipakai apa adanya", 10, 10 * time.Minute},
		{"kelewat besar dipangkas ke 24 jam", 10080, 24 * time.Hour},
	}
	for _, k := range kasus {
		t.Run(k.nama, func(t *testing.T) {
			if got := deferDelay(k.menit); got != k.harap {
				t.Fatalf("deferDelay(%d) = %s, mau %s", k.menit, got, k.harap)
			}
		})
	}

	// Pengikat eksplisit ke penyebabnya: apa pun nilainya, jeda tak boleh lebih
	// pendek dari jendela klaim scheduler.
	for _, m := range []int{-5, 0, 1} {
		if d := deferDelay(m); d < schedulerPrepLead {
			t.Fatalf("deferDelay(%d) = %s, lebih pendek dari jendela klaim %s — tugas akan disambar seketika",
				m, d, schedulerPrepLead)
		}
	}
}

// TestBuildDeepWorkInstruction memastikan giliran lanjutan membawa brief-nya UTUH.
// Giliran ini berjalan tanpa pesan pemicu dari Pak Sudianto; bila brief-nya hilang,
// orchestrator hanya punya ingatan sesi untuk menebak apa yang harus dikerjakan —
// dan yang keluar adalah laporan karangan, bukan error.
func TestBuildDeepWorkInstruction(t *testing.T) {
	const brief = "Telusuri harga pasar sewa coworking space di Jakarta Selatan"
	got := buildDeepWorkInstruction(model.ScheduledTask{Kind: "deep_work", Note: brief})

	if !strings.Contains(got, brief) {
		t.Fatalf("instruksi tidak memuat brief:\n%s", got)
	}
	// Tanpa penanda ini, orchestrator memperlakukannya sebagai pesan dari SU dan
	// membalas seolah sedang mengobrol, bukan mengerjakan tugas.
	if !strings.Contains(got, "giliran sistem") {
		t.Fatalf("instruksi tidak menandai dirinya giliran sistem:\n%s", got)
	}
	// Kegagalan yang paling mahal di jalur riset adalah laporan yang dikarang.
	if !strings.Contains(strings.ToUpper(got), "APA ADANYA") {
		t.Fatalf("instruksi tidak meminta kejujuran saat gagal:\n%s", got)
	}
}
