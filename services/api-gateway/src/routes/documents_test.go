package routes

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestReadDocFromWorkDir_Sandbox menguji pagar keamanan jalur `docPath`. Isi field ini
// berasal dari model bahasa yang bisa terpapar teks web tak tepercaya, jadi yang diuji
// di sini bukan "apakah berkas terbaca" melainkan "apakah yang DI LUAR ditolak".
func TestReadDocFromWorkDir_Sandbox(t *testing.T) {
	root := t.TempDir()
	// Berkas rahasia SATU TINGKAT DI ATAS direktori kerja — meniru .env di root repo.
	secret := filepath.Join(filepath.Dir(root), "secret.env")
	if err := os.WriteFile(secret, []byte("MS_GRAPH_CLIENT_SECRET=rahasia"), 0o600); err != nil {
		t.Fatalf("siapkan berkas rahasia: %v", err)
	}
	defer os.Remove(secret)

	want := []byte("PK\x03\x04 pura-pura xlsx")
	if err := os.WriteFile(filepath.Join(root, "laporan.xlsx"), want, 0o600); err != nil {
		t.Fatalf("siapkan berkas sah: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatalf("siapkan subdir: %v", err)
	}

	h := &Handler{DocWorkDir: root}

	t.Run("berkas sah terbaca via path absolut", func(t *testing.T) {
		got, err := h.readDocFromWorkDir(filepath.Join(root, "laporan.xlsx"))
		if err != nil {
			t.Fatalf("tak terduga: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("isi berbeda: %q", got)
		}
	})

	t.Run("path relatif diartikan relatif ke direktori kerja", func(t *testing.T) {
		// Bukan cwd gateway — kalau salah diartikan, berkas tak akan ketemu.
		got, err := h.readDocFromWorkDir("laporan.xlsx")
		if err != nil {
			t.Fatalf("tak terduga: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("isi berbeda: %q", got)
		}
	})

	// Inti pengujian: setiap cara keluar dari sandbox harus gagal.
	for _, tc := range []struct {
		nama string
		path string
	}{
		{"traversal relatif", filepath.Join("..", "secret.env")},
		{"traversal via subdir", filepath.Join("subdir", "..", "..", "secret.env")},
		{"absolut di luar", secret},
		{"direktori, bukan berkas", root},
		{"berkas tak ada", filepath.Join(root, "tidak-ada.pdf")},
	} {
		t.Run("ditolak: "+tc.nama, func(t *testing.T) {
			if _, err := h.readDocFromWorkDir(tc.path); err == nil {
				t.Fatalf("KEBOCORAN: %s seharusnya ditolak, tapi terbaca", tc.path)
			}
		})
	}

	// Symlink adalah jalan keluar yang TIDAK tertangkap pemeriksaan string biasa:
	// path-nya berada di dalam direktori kerja, targetnya di luar.
	t.Run("ditolak: symlink menunjuk keluar", func(t *testing.T) {
		link := filepath.Join(root, "tautan.env")
		if err := os.Symlink(secret, link); err != nil {
			// Windows tanpa Developer Mode melarang symlink bagi non-admin.
			t.Skipf("tak bisa membuat symlink di %s: %v", runtime.GOOS, err)
		}
		if _, err := h.readDocFromWorkDir(link); err == nil {
			t.Fatal("KEBOCORAN: symlink ke luar direktori kerja seharusnya ditolak")
		}
	})

	t.Run("ditolak: berkas melebihi batas", func(t *testing.T) {
		big := filepath.Join(root, "besar.bin")
		if err := os.WriteFile(big, make([]byte, maxDocBytes+1), 0o600); err != nil {
			t.Fatalf("siapkan berkas besar: %v", err)
		}
		defer os.Remove(big)
		_, err := h.readDocFromWorkDir(big)
		if err == nil {
			t.Fatal("berkas melebihi batas seharusnya ditolak")
		}
		if !strings.Contains(err.Error(), "terlalu besar") {
			t.Fatalf("ditolak karena alasan yang salah: %v", err)
		}
	})

	t.Run("nonaktif bila DocWorkDir kosong", func(t *testing.T) {
		empty := &Handler{}
		if _, err := empty.readDocFromWorkDir("apa saja"); err == nil {
			t.Fatal("seharusnya menolak saat DOC_WORK_DIR tidak aktif")
		}
	})
}
