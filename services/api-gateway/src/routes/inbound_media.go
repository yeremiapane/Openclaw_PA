package routes

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// inboundFile = ringkasan lampiran media dari pesan masuk (sebelum diunduh).
type inboundFile struct {
	URL      string
	Mimetype string
	Filename string
}

// mediaKind = deskripsi jenis berkas + petunjuk cara membacanya untuk agent.
type mediaKind struct {
	label string // deskripsi ringkas untuk pesan
	hint  string // petunjuk konkret cara membaca (Read vs Python + pustaka)
}

// inboundMediaAllow = allowlist mimetype yang boleh diunduh & dianalisa.
// Fase 2: PDF + gambar (dibaca native oleh Claude). Fase 3: Excel/CSV/Word/PPT
// (ekstraksi via Python di sisi agent — lib sudah ada di image brain).
var inboundMediaAllow = map[string]mediaKind{
	"application/pdf": {"PDF", "Pakai Read untuk membaca PDF ini langsung."},
	"image/jpeg":      {"gambar JPEG", "Pakai Read untuk melihat gambar ini langsung."},
	"image/png":       {"gambar PNG", "Pakai Read untuk melihat gambar ini langsung."},
	"image/webp":      {"gambar WebP", "Pakai Read untuk melihat gambar ini langsung."},
	"text/plain":      {"teks", "Pakai Read untuk membaca berkas teks ini."},

	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": {
		"Excel (XLSX)", "Baca dengan Python: pandas.read_excel(path) (atau openpyxl). Ringkas nama kolom, jumlah baris, dan angka penting."},
	"application/vnd.ms-excel": {
		"Excel (XLS)", "Baca dengan Python (pandas.read_excel). Bila gagal karena format lama, minta versi .xlsx."},
	"text/csv": {
		"CSV", "Baca dengan Python: pandas.read_csv(path). Ringkas kolom & jumlah baris."},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": {
		"Word (DOCX)", "Baca dengan Python: python-docx — docx.Document(path), gabungkan teks tiap paragraf."},
	"application/msword": {
		"Word (.doc lama)", "Format .doc lama sulit dibaca; coba Python bila bisa, atau minta versi .docx/PDF."},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {
		"PowerPoint (PPTX)", "Baca dengan Python: python-pptx — pptx.Presentation(path), iterasi slide lalu shape.text."},
	"application/vnd.ms-powerpoint": {
		"PowerPoint (.ppt lama)", "Format .ppt lama sulit dibaca; minta versi .pptx bila gagal."},
}

// extToMime memetakan ekstensi berkas ke mimetype kanonik, sebagai fallback bila
// WhatsApp/WAHA mengirim mimetype generik (mis. application/octet-stream).
var extToMime = map[string]string{
	".pdf":  "application/pdf",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".txt":  "text/plain",
	".md":   "text/plain",
	".csv":  "text/csv",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".xls":  "application/vnd.ms-excel",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".doc":  "application/msword",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".ppt":  "application/vnd.ms-powerpoint",
}

// resolveInboundKind menentukan jenis berkas dari mimetype; bila tak dikenali,
// jatuh ke ekstensi nama berkas. Mengembalikan false bila jenis belum didukung.
func resolveInboundKind(mimetype, filename string) (mediaKind, bool) {
	mime := strings.ToLower(strings.TrimSpace(mimetype))
	// Sebagian mimetype membawa "; charset=..." — ambil bagian utamanya.
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if k, ok := inboundMediaAllow[mime]; ok {
		return k, true
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if canon, ok := extToMime[ext]; ok {
		if k, ok2 := inboundMediaAllow[canon]; ok2 {
			return k, true
		}
	}
	return mediaKind{}, false
}

// ingestInboundFile mengunduh lampiran, menyimpannya di direktori kerja, lalu
// mengembalikan TEKS turn baru untuk orchestrator berupa instruksi membaca &
// menganalisa berkas. `caption` adalah teks yang menyertai file (payload.body).
// Bila jenis tak didukung / unduh gagal / simpan gagal, tetap mengembalikan teks
// yang memberi tahu agar agent membalas dengan anggun (bukan diam).
func (h *Handler) ingestInboundFile(ctx context.Context, convID, caption string, f *inboundFile) string {
	kind, ok := resolveInboundKind(f.Mimetype, f.Filename)
	if !ok {
		log.Printf("[FILE] jenis %q (%s) belum didukung — diabaikan (conv=%s)", f.Mimetype, f.Filename, convID)
		return composeFileText(caption, fmt.Sprintf(
			"[BERKAS BELUM DIDUKUNG] Pak Sudianto mengirim berkas '%s' (%s), tetapi jenis ini "+
				"belum bisa dianalisa. Sampaikan dengan sopan dan tawarkan format lain bila perlu.",
			f.Filename, f.Mimetype))
	}

	if strings.TrimSpace(h.DocWorkDir) == "" {
		log.Printf("[FILE] DOC_WORK_DIR nonaktif — berkas tak bisa disimpan (conv=%s)", convID)
		return composeFileText(caption, fmt.Sprintf(
			"[BERKAS TAK TERPROSES] Pak Sudianto mengirim '%s' tetapi penyimpanan berkas sedang "+
				"nonaktif. Minta maaf dan minta beliau mengirim ulang nanti.", f.Filename))
	}

	data, ctype, err := h.Waha.DownloadMedia(f.URL)
	if err != nil {
		log.Printf("[FILE] unduh gagal (%s, conv=%s): %v", f.Filename, convID, err)
		return composeFileText(caption, fmt.Sprintf(
			"[BERKAS GAGAL DIUNDUH] Berkas '%s' dari Pak Sudianto tidak dapat diambil dari WhatsApp "+
				"(mungkin sudah kedaluwarsa di server). Minta beliau mengirim ulang.", f.Filename))
	}

	savedPath, err := h.stageInboundFile(f.Filename, data)
	if err != nil {
		log.Printf("[FILE] simpan gagal (%s, conv=%s): %v", f.Filename, convID, err)
		return composeFileText(caption, fmt.Sprintf(
			"[BERKAS GAGAL DISIMPAN] Berkas '%s' gagal disiapkan untuk dibaca. Minta maaf dan "+
				"minta kirim ulang.", f.Filename))
	}

	log.Printf("[FILE] tersimpan: %s (%s, %d byte) conv=%s", savedPath, ctype, len(data), convID)

	instr := fmt.Sprintf(
		"[BERKAS DITERIMA]\nPak Sudianto mengirim berkas %s: \"%s\" (%s, %d byte).\n"+
			"Berkas SUDAH tersimpan di path berikut. Cara membaca: %s\n%s\n"+
			"Setelah membaca isinya, tanggapi permintaan Pak Sudianto berdasarkan ISI berkas.",
		kind.label, f.Filename, f.Mimetype, len(data), kind.hint, savedPath)
	return composeFileText(caption, instr)
}

// composeFileText menggabungkan caption (bila ada) dengan instruksi berkas.
func composeFileText(caption, instr string) string {
	caption = strings.TrimSpace(caption)
	if caption == "" {
		return instr
	}
	return caption + "\n\n" + instr
}

// stageInboundFile menulis byte berkas masuk ke DOC_WORK_DIR dengan prefix
// "inbound_" + stempel unik, sehingga janitor dokumen menyapunya otomatis (24 jam)
// seperti berkas keluar. Mengembalikan path absolut berkas.
func (h *Handler) stageInboundFile(name string, data []byte) (string, error) {
	safe := sanitizeFilename(name)
	if safe == "" {
		safe = "berkas"
	}
	fname := fmt.Sprintf("inbound_%d_%s", time.Now().UnixNano(), safe)
	dest := filepath.Join(h.DocWorkDir, fname)
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", err
	}
	return dest, nil
}
