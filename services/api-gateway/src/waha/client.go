// Package waha adalah client tipis untuk WAHA REST API.
package waha

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// maxInboundMediaBytes = batas ukuran file masuk yang diunduh dari WAHA.
const maxInboundMediaBytes = 16 << 20 // 16 MiB

var nonDigit = regexp.MustCompile(`\D`)

// Client memanggil WAHA HTTP API.
type Client struct {
	baseURL string
	apiKey  string
	session string
	http    *http.Client
}

// New membuat WAHA client.
func New(baseURL, apiKey, session string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		session: session,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// NormalizeChatID mengubah nomor (mis. "+62 852-7760-3027") menjadi
// format chatId WAHA: "6285277603027@c.us".
func NormalizeChatID(phone string) string {
	digits := nonDigit.ReplaceAllString(phone, "")
	return digits + "@c.us"
}

type sendTextReq struct {
	Session string `json:"session"`
	ChatID  string `json:"chatId"`
	Text    string `json:"text"`
}

// SendText mengirim pesan teks ke nomor tujuan via WAHA.
func (c *Client) SendText(to, text string) error {
	payload := sendTextReq{
		Session: c.session,
		ChatID:  NormalizeChatID(to),
		Text:    text,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/sendText", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kirim ke WAHA gagal: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("WAHA sendText HTTP %d: %s", resp.StatusCode, string(b))
	}
	log.Printf("[OUTBOUND] -> %s : %q", payload.ChatID, text)
	return nil
}

// SendToChat mengirim teks ke chatId yang SUDAH berformat WAHA (mis.
// "6285...@c.us" atau "5296...@lid"). Dipakai untuk membalas ke chat asal
// pesan masuk, sehingga balasan ke kontak @lid tidak salah dinormalisasi
// menjadi @c.us.
func (c *Client) SendToChat(chatID, text string) error {
	payload := sendTextReq{
		Session: c.session,
		ChatID:  chatID,
		Text:    text,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/sendText", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kirim ke WAHA gagal: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("WAHA sendText HTTP %d: %s", resp.StatusCode, string(b))
	}
	log.Printf("[OUTBOUND] -> %s : %q", chatID, text)
	return nil
}

// fileObj = objek file pada payload sendFile WAHA (data = base64).
type fileObj struct {
	Mimetype string `json:"mimetype"`
	Filename string `json:"filename"`
	Data     string `json:"data"`
}

type sendFileReq struct {
	Session string  `json:"session"`
	ChatID  string  `json:"chatId"`
	File    fileObj `json:"file"`
	Caption string  `json:"caption,omitempty"`
}

// SendFile mengirim satu dokumen (lampiran) ke nomor tujuan via WAHA. `data` adalah
// byte mentah file; gateway-lah yang meng-encode base64 sesuai kontrak WAHA. Gateway
// hanya MENGEMAS & MENGIRIM byte — ia tidak tahu/menentukan jenis atau format laporan
// (itu wewenang agent/Claude). Endpoint /api/sendFile tersedia di WAHA Core.
func (c *Client) SendFile(to, filename, mimetype string, data []byte, caption string) error {
	payload := sendFileReq{
		Session: c.session,
		ChatID:  NormalizeChatID(to),
		File: fileObj{
			Mimetype: mimetype,
			Filename: filename,
			Data:     base64.StdEncoding.EncodeToString(data),
		},
		Caption: caption,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/sendFile", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kirim file ke WAHA gagal: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("WAHA sendFile HTTP %d: %s", resp.StatusCode, string(b))
	}
	log.Printf("[OUTBOUND] -> %s : [FILE %s (%s, %d byte)]", payload.ChatID, filename, mimetype, len(data))
	return nil
}

// ResolvePhoneByLid menanyakan WAHA (engine GOWS menyimpan peta LID↔nomor) untuk
// nomor asli (MSISDN digit-only) di balik sebuah LID. Dipakai whitelist saat pesan
// masuk `from` berupa `<lid>@lid` TANPA remoteJidAlt/participantAlt (AltPhone kosong),
// sehingga kontak yang di-whitelist via nomor tetap dikenali. Mengembalikan string
// kosong (tanpa error) bila WAHA tidak punya pemetaannya.
func (c *Client) ResolvePhoneByLid(lid string) (string, error) {
	lid = nonDigit.ReplaceAllString(lid, "") // buang "@lid" / karakter non-digit
	if lid == "" {
		return "", nil
	}
	req, _ := http.NewRequest(http.MethodGet, c.baseURL+"/api/"+c.session+"/lids/"+lid, nil)
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve lid ke WAHA gagal: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil // WAHA tak punya pemetaan untuk LID ini
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("WAHA lids HTTP %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		PN string `json:"pn"` // mis. "6282277531326@c.us"
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return nonDigit.ReplaceAllString(out.PN, ""), nil
}

// DownloadMedia mengunduh file media dari WAHA. rawURL adalah payload.media.url
// yang memakai host INTERNAL WAHA (mis. http://localhost:3000/...); host-nya
// ditulis-ulang ke baseURL client agar terjangkau dari lingkungan ini (dev:
// localhost:13000, prod: waha:3000). WAHA menuntut X-Api-Key (401 tanpa). File
// bersifat FANA (~WHATSAPP_FILES_LIFETIME) → panggil segera saat webhook tiba.
// Mengembalikan byte file + Content-Type dari respons.
func (c *Client) DownloadMedia(rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("URL media invalid: %w", err)
	}
	dl := c.baseURL + u.Path
	if u.RawQuery != "" {
		dl += "?" + u.RawQuery
	}

	req, err := http.NewRequest(http.MethodGet, dl, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("unduh media dari WAHA gagal: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", fmt.Errorf("WAHA download HTTP %d: %s", resp.StatusCode, string(b))
	}

	// Baca sampai batas+1 untuk mendeteksi file yang melampaui batas.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxInboundMediaBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("baca media gagal: %w", err)
	}
	if len(data) > maxInboundMediaBytes {
		return nil, "", fmt.Errorf("media terlalu besar (> %d byte)", maxInboundMediaBytes)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// ── Presence (indikator baca & "mengetik…") ───────────────────────
// Semua presence bersifat best-effort: kegagalan tidak boleh mengganggu
// jalur utama pesan. Endpoint ini tersedia di WAHA Core.

type chatRef struct {
	Session string `json:"session"`
	ChatID  string `json:"chatId"`
}

type sendSeenReq struct {
	Session   string `json:"session"`
	ChatID    string `json:"chatId"`
	MessageID string `json:"messageId,omitempty"`
}

// postJSON mengirim POST JSON ke WAHA dan memeriksa status HTTP. Dipakai
// endpoint presence yang tak perlu membaca body respons.
func (c *Client) postJSON(path string, payload any) error {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kirim ke WAHA gagal: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("WAHA %s HTTP %d: %s", path, resp.StatusCode, string(b))
	}
	return nil
}

// SendSeen menandai pesan masuk sebagai sudah dibaca (centang biru) di chat asal.
func (c *Client) SendSeen(chatID, messageID string) error {
	return c.postJSON("/api/sendSeen", sendSeenReq{Session: c.session, ChatID: chatID, MessageID: messageID})
}

// StartTyping menampilkan indikator "sedang mengetik…" di chat tujuan.
func (c *Client) StartTyping(chatID string) error {
	return c.postJSON("/api/startTyping", chatRef{Session: c.session, ChatID: chatID})
}

// StopTyping menghentikan indikator "sedang mengetik…".
func (c *Client) StopTyping(chatID string) error {
	return c.postJSON("/api/stopTyping", chatRef{Session: c.session, ChatID: chatID})
}

// SessionStatus mengembalikan status session (mis. "WORKING", "STOPPED").
func (c *Client) SessionStatus() (string, error) {
	req, _ := http.NewRequest(http.MethodGet, c.baseURL+"/api/sessions/"+c.session, nil)
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Status, nil
}

// EnsureSessionStarted memastikan session WAHA aktif (status WORKING).
// WAHA Core tidak auto-start session saat container restart, jadi API Gateway
// memanggil ini saat boot. Auth tersimpan di volume → tidak perlu scan QR ulang.
func (c *Client) EnsureSessionStarted() {
	status, err := c.SessionStatus()
	if err != nil {
		log.Printf("[waha] tidak bisa cek status session: %v", err)
		return
	}
	if status == "WORKING" {
		log.Printf("[waha] session %q sudah WORKING", c.session)
		return
	}
	log.Printf("[waha] session %q status %q — mencoba start...", c.session, status)

	req, _ := http.NewRequest(http.MethodPost, c.baseURL+"/api/sessions/"+c.session+"/start", nil)
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		log.Printf("[waha] gagal start session: %v", err)
		return
	}
	resp.Body.Close()

	// Poll hingga WORKING (maks ~30 dtk).
	for i := 0; i < 10; i++ {
		time.Sleep(3 * time.Second)
		if s, _ := c.SessionStatus(); s == "WORKING" {
			log.Printf("[waha] session %q kini WORKING", c.session)
			return
		}
	}
	log.Printf("[waha] session %q belum WORKING setelah start (mungkin perlu scan QR)", c.session)
}
