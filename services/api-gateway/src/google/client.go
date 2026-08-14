package google

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// contactsScope = izin baca-tulis kontak (People API createContact).
const contactsScope = "https://www.googleapis.com/auth/contacts"

// Endpoint Google — var (bukan const) agar bisa diarahkan ke server httptest saat uji.
var (
	tokenEndpoint  = "https://oauth2.googleapis.com/token"
	createEndpoint = "https://people.googleapis.com/v1/people:createContact"
)

var nonDigit = regexp.MustCompile(`\D`)

// Client memanggil Google OAuth + People API.
type Client struct {
	clientID     string
	clientSecret string
	refreshToken string

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
	httpClient  *http.Client
}

// New membuat client Google. Bila salah satu kredensial kosong, Enabled() = false
// dan pemanggil harus melewati integrasi ini (nonaktif dengan aman).
func New(clientID, clientSecret, refreshToken string) *Client {
	return &Client{
		clientID:     strings.TrimSpace(clientID),
		clientSecret: strings.TrimSpace(clientSecret),
		refreshToken: strings.TrimSpace(refreshToken),
		httpClient:   &http.Client{Timeout: 20 * time.Second},
	}
}

// Enabled melaporkan apakah kredensial lengkap sehingga client bisa dipakai.
func (c *Client) Enabled() bool {
	if c == nil {
		return false
	}
	return c.clientID != "" && c.clientSecret != "" && c.refreshToken != ""
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// getToken menukar refresh token menjadi access token (cached, refresh otomatis saat
// hampir kedaluwarsa). Google TIDAK merotasi refresh token pada grant ini, jadi lebih
// sederhana dari MS Graph — refresh token .env tetap berlaku sampai dicabut manual.
func (c *Client) getToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.accessToken != "" && time.Now().Before(c.expiresAt) {
		return c.accessToken, nil
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"refresh_token": {c.refreshToken},
		"scope":         {contactsScope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request gagal: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("token decode gagal: %w", err)
	}
	if tr.Error != "" {
		return "", fmt.Errorf("token error: %s — %s", tr.Error, tr.ErrorDesc)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("access token kosong (HTTP %d)", resp.StatusCode)
	}

	c.accessToken = tr.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(tr.ExpiresIn-60) * time.Second)
	log.Printf("[google] access token diperoleh, berlaku ~%d detik", tr.ExpiresIn)
	return c.accessToken, nil
}

type createContactBody struct {
	Names        []contactName  `json:"names"`
	PhoneNumbers []contactPhone `json:"phoneNumbers"`
}

type contactName struct {
	GivenName string `json:"givenName"`
}

type contactPhone struct {
	Value string `json:"value"`
	Type  string `json:"type"`
}

type createContactResponse struct {
	ResourceName string `json:"resourceName"`
}

// toE164 memformat nomor (digit apa pun / +62…) menjadi E.164 "+<digit>" yang disukai
// People API. Menerima "6285…" atau "+62 852…"; keluaran "+6285…".
func toE164(phone string) string {
	digits := nonDigit.ReplaceAllString(phone, "")
	if digits == "" {
		return ""
	}
	return "+" + digits
}

// CreateContact membuat satu kontak baru (nama + nomor) di akun Google pemilik refresh
// token. Mengembalikan resourceName ("people/c…") untuk disimpan sebagai penanda sudah
// tersinkron (idempotensi di sisi pemanggil). Nama kosong → pakai nomor sebagai label.
func (c *Client) CreateContact(ctx context.Context, name, phone string) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("google contacts nonaktif (kredensial kosong)")
	}
	e164 := toE164(phone)
	if e164 == "" {
		return "", fmt.Errorf("nomor kosong/invalid")
	}
	given := strings.TrimSpace(name)
	if given == "" {
		given = e164
	}

	token, err := c.getToken(ctx)
	if err != nil {
		return "", err
	}

	payload, _ := json.Marshal(createContactBody{
		Names:        []contactName{{GivenName: given}},
		PhoneNumbers: []contactPhone{{Value: e164, Type: "mobile"}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, createEndpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("createContact request gagal: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("createContact HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300))
	}
	var out createContactResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decode createContact gagal: %w", err)
	}
	log.Printf("[google] kontak dibuat: %q (%s) → %s", given, e164, out.ResourceName)
	return out.ResourceName, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
