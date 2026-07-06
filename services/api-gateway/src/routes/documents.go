package routes

import (
	"context"
	"encoding/base64"
	"log"
	"mime"
	"path/filepath"
	"strings"

	"pa-ai/api-gateway/src/model"
)

// Batas ukuran dokumen setelah decode.
const maxDocBytes = 8 << 20 // 8 MiB

// sendDocument menangani SEND_DOCUMENT: isi dokumen disusun oleh agent, lalu gateway
// membungkusnya menjadi file dan mengirimkannya via WAHA.
//
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

	data, err := decodeDocContent(a.DocContent, a.DocEncoding)
	if err != nil {
		log.Printf("[DOC] decode isi gagal (%s): %v — diabaikan", filename, err)
		return
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

// sanitizeFilename memangkas komponen path dan karakter berbahaya, sehingga agent
// tidak bisa mengarahkan penulisan/pengiriman ke nama yang menyesatkan. Mengembalikan
// hanya basename yang bersih (maks 120 rune).
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
