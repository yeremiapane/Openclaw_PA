package routes

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pa-ai/api-gateway/src/model"
)

// Batas ukuran dokumen setelah decode.
const maxDocBytes = 8 << 20 // 8 MiB

// sendDocument menangani SEND_DOCUMENT: isi dokumen disusun oleh agent, lalu gateway
// membungkusnya menjadi file dan mengirimkannya via WAHA.
// Hanya percakapan ber-trust 'su' yang boleh memicu. Pengiriman hanya ke SU.
func (h *Handler) sendDocument(ctx context.Context, convID string, initiator *model.Contact, a model.Action, execID int64) {
	if initiator == nil || initiator.TrustLevel != "su" {
		trust := "(nil)"
		if initiator != nil {
			trust = initiator.TrustLevel
		}
		log.Printf("[DOC] DITOLAK: inisiator non-SU (trust=%s)", trust)
		return
	}
	if strings.TrimSpace(h.SUPhone) == "" {
		log.Printf("[DOC] SUPhone kosong — dokumen diabaikan")
		return
	}

	filename := sanitizeFilename(a.DocFilename)
	if filename == "" {
		log.Printf("[DOC] nama file kosong/invalid — diabaikan")
		return
	}

	// Isi bisa dari docPath (berkas kerja) atau docContent (inline).
	// Jika keduanya ada, docPath diprioritaskan.
	var data []byte
	var err error
	if strings.TrimSpace(a.DocPath) != "" {
		data, err = h.readDocFromWorkDir(a.DocPath)
		if err != nil {
			log.Printf("[DOC] baca berkas gagal (%s): %v — diabaikan", a.DocPath, err)
			return
		}
	} else {
		data, err = decodeDocContent(a.DocContent, a.DocEncoding)
		if err != nil {
			log.Printf("[DOC] decode isi gagal (%s): %v — diabaikan", filename, err)
			return
		}
	}
	if len(data) == 0 {
		log.Printf("[DOC] isi kosong (%s) — diabaikan", filename)
		return
	}
	if len(data) > maxDocBytes {
		log.Printf("[DOC] isi terlalu besar (%s, %d byte > %d) — diabaikan", filename, len(data), maxDocBytes)
		return
	}

	mimeType := resolveMime(a.DocMime, filename)
	caption := strings.TrimSpace(a.DocCaption)

	log.Printf("[DOC] kirim ke SU: %s (%s, %d byte)", filename, mimeType, len(data))
	h.sendAndRecord(ctx, func() error { return h.Waha.SendFile(h.SUPhone, filename, mimeType, data, caption) },
		model.OutboundMessage{
			ExecutionID: execPtr(execID), ConversationID: convID, ContactID: cidPtr(initiator),
			AgentID: "orchestrator", TargetChat: h.SUPhone, Kind: "document",
			Text: "[DOKUMEN] " + filename + " — " + caption,
		})
}

// docRetention = berapa lama berkas di direktori kerja dibiarkan sebelum disapu.
// Cukup lama agar Pak Sudianto sempat minta kirim ulang berkas yang sama.
const docRetention = 24 * time.Hour

// StartDocJanitor menyapu berkas lama di direktori kerja dokumen.
func (h *Handler) StartDocJanitor(ctx context.Context) {
	if strings.TrimSpace(h.DocWorkDir) == "" {
		return
	}
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			h.sweepDocWorkDir()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func (h *Handler) sweepDocWorkDir() {
	entries, err := os.ReadDir(h.DocWorkDir)
	if err != nil {
		log.Printf("[DOC-JANITOR] baca %s gagal: %v", h.DocWorkDir, err)
		return
	}
	cutoff := time.Now().Add(-docRetention)
	removed := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(h.DocWorkDir, e.Name())); err == nil {
			removed++
		}
	}
	if removed > 0 {
		log.Printf("[DOC-JANITOR] %d berkas lama disapu dari %s", removed, h.DocWorkDir)
	}
}

// buildDocWorkDirSnapshot memberi tahu orchestrator path direktori kerja yang disuntik
// per-lingkungan, bukan dihardcode.
func (h *Handler) buildDocWorkDirSnapshot() string {
	if strings.TrimSpace(h.DocWorkDir) == "" {
		return ""
	}
	return "[DIREKTORI KERJA DOKUMEN]\n" +
		"Untuk membuat berkas BINER (XLSX/PDF/PPTX/PNG/DOCX), tulis berkasnya SUNGGUHAN " +
		"di direktori ini dengan perkakasmu (mis. python + openpyxl/reportlab/python-pptx), " +
		"lalu kirim lewat SEND_DOCUMENT dengan `docPath` berisi path berkas itu:\n" +
		h.DocWorkDir + "\n" +
		"JANGAN mengarang base64 untuk format biner — hasilnya berkas rusak yang tak " +
		"terdeteksi sistem. Berkas di luar direktori ini DITOLAK. Untuk format TEKS " +
		"(CSV/MD/HTML/JSON/TXT) tetap pakai `docContent` seperti biasa, lebih ringkas."
}

// readDocFromWorkDir membaca berkas hanya jika masih berada di dalam DocWorkDir.
func (h *Handler) readDocFromWorkDir(p string) ([]byte, error) {
	if strings.TrimSpace(h.DocWorkDir) == "" {
		return nil, errors.New("DOC_WORK_DIR tidak aktif")
	}

	root, err := filepath.EvalSymlinks(h.DocWorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve DOC_WORK_DIR: %w", err)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, fmt.Errorf("berkas tak ditemukan/tak terbaca: %w", err)
	}

	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path di luar direktori kerja (%s) — ditolak", h.DocWorkDir)
	}

	st, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	// Direktori, pipe, device: bukan dokumen. Membaca /dev/zero akan menggantung
	// goroutine ini selamanya, jadi tolak apa pun yang bukan berkas biasa.
	if !st.Mode().IsRegular() {
		return nil, errors.New("bukan berkas biasa")
	}
	if st.Size() > maxDocBytes {
		return nil, fmt.Errorf("berkas terlalu besar (%d byte > %d)", st.Size(), maxDocBytes)
	}

	return os.ReadFile(real)
}

// decodeDocContent mengubah isi action menjadi byte file. encoding "base64" → decode
// base64; selain itu (default "utf8"/"text"/kosong) → byte teks apa adanya.
func decodeDocContent(content, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64", "b64":
		clean := strings.TrimSpace(content)
		return base64.StdEncoding.DecodeString(clean)
	default:
		return []byte(content), nil
	}
}

// sanitizeFilename membersihkan nama file dari path dan karakter berbahaya.
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	// Buang komponen direktori (baik / maupun \).
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	// Tolak referensi naik-direktori & nama tersembunyi/kosong.
	if name == "." || name == ".." || name == "/" {
		return ""
	}
	// Buang karakter kontrol.
	name = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > 120 {
		name = string(r[:120])
	}
	return name
}

// resolveMime menentukan tipe konten: utamakan mime eksplisit dari agent; bila kosong,
// tebak dari ekstensi file; fallback application/octet-stream.
func resolveMime(explicit, filename string) string {
	if m := strings.TrimSpace(explicit); m != "" {
		return m
	}
	if ext := filepath.Ext(filename); ext != "" {
		if m := mime.TypeByExtension(ext); m != "" {
			// mime.TypeByExtension bisa menyertakan "; charset=..." — pertahankan apa adanya.
			return m
		}
	}
	return "application/octet-stream"
}
