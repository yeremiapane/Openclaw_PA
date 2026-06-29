// MS Graph client (Client Credentials flow) — gabungan dari bekas micro-service
// email & calendar Fase 9. Token management + Mail.Send + Calendar API dalam satu
// client supaya kredensial hanya dimuat di proses gateway (tidak ada port polos).
package services

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// GraphClient membungkus koneksi ke Microsoft Graph API.
type GraphClient struct {
	tenantID     string
	clientID     string
	clientSecret string
	userUPN      string // pengirim email & pemilik calendar (pa@hypernet.co.id)

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
	httpClient  *http.Client
}

// NewGraphClient membuat client MS Graph baru.
func NewGraphClient(tenantID, clientID, clientSecret, userUPN string) *GraphClient {
	return &GraphClient{
		tenantID:     tenantID,
		clientID:     clientID,
		clientSecret: clientSecret,
		userUPN:      userUPN,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}
}

// ── Token Management ──────────────────────────────────────────────────

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// getToken mengambil access token (cached, refresh otomatis saat hampir expired).
func (g *GraphClient) getToken() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.accessToken != "" && time.Now().Before(g.expiresAt) {
		return g.accessToken, nil
	}

	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", g.tenantID)
	data := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {g.clientID},
		"client_secret": {g.clientSecret},
		"scope":         {"https://graph.microsoft.com/.default"},
	}

	resp, err := g.httpClient.PostForm(tokenURL, data)
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
		return "", fmt.Errorf("token kosong (HTTP %d): %s", resp.StatusCode, string(body))
	}

	g.accessToken = tr.AccessToken
	g.expiresAt = time.Now().Add(time.Duration(tr.ExpiresIn-60) * time.Second)
	log.Printf("[graph] token diperoleh, berlaku ~%d detik", tr.ExpiresIn)
	return g.accessToken, nil
}

// doRequest menjalankan HTTP request ke MS Graph dengan Bearer token + JSON header.
func (g *GraphClient) doRequest(method, apiURL string, body io.Reader) (*http.Response, error) {
	token, err := g.getToken()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, apiURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return g.httpClient.Do(req)
}

// ── Shared Graph types ────────────────────────────────────────────────

type graphBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type graphEmail struct {
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
}

// ── Mail.Send API ─────────────────────────────────────────────────────

// SendMailRequest berisi data untuk mengirim email via MS Graph.
type SendMailRequest struct {
	To          string
	ToName      string
	Subject     string
	HTMLBody    string
	Attachments []MailAttachment
}

// MailAttachment merepresentasikan satu file attachment.
type MailAttachment struct {
	Name        string
	ContentType string
	Content     []byte // raw; akan di-base64 encode
}

type graphSendMailBody struct {
	Message struct {
		Subject      string            `json:"subject"`
		Body         graphBody         `json:"body"`
		ToRecipients []graphRecipient  `json:"toRecipients"`
		Attachments  []graphAttachment `json:"attachments,omitempty"`
	} `json:"message"`
	SaveToSentItems bool `json:"saveToSentItems"`
}

type graphRecipient struct {
	EmailAddress graphEmail `json:"emailAddress"`
}

type graphAttachment struct {
	ODataType    string `json:"@odata.type"`
	Name         string `json:"name"`
	ContentType  string `json:"contentType"`
	ContentBytes string `json:"contentBytes"` // base64
}

// SendMail mengirim email via MS Graph Mail.Send API (202 = sukses).
func (g *GraphClient) SendMail(req SendMailRequest) error {
	var body graphSendMailBody
	body.Message.Subject = req.Subject
	body.Message.Body = graphBody{ContentType: "HTML", Content: req.HTMLBody}
	body.Message.ToRecipients = []graphRecipient{
		{EmailAddress: graphEmail{Address: req.To, Name: req.ToName}},
	}
	body.SaveToSentItems = true

	for _, att := range req.Attachments {
		body.Message.Attachments = append(body.Message.Attachments, graphAttachment{
			ODataType:    "#microsoft.graph.fileAttachment",
			Name:         att.Name,
			ContentType:  att.ContentType,
			ContentBytes: base64.StdEncoding.EncodeToString(att.Content),
		})
	}

	payload, _ := json.Marshal(body)
	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/sendMail", g.userUPN)

	resp, err := g.doRequest(http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("send mail request gagal: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send mail HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}
	log.Printf("[email] email terkirim ke %s <%s> subj=%q", req.ToName, req.To, req.Subject)
	return nil
}

// ── Calendar API ──────────────────────────────────────────────────────

// CalendarEvent adalah request untuk membuat event di O365.
type CalendarEvent struct {
	Title           string
	Datetime        string // RFC3339 (mis. 2026-06-25T14:00:00+07:00)
	DurationMinutes int    // default 60
	Venue           string // lokasi; kosong = online
	Attendees       []string
	ReminderMinutes int  // default 180 (3 jam)
	Body            string
	IsOnline        bool // true → onlineMeetingProvider teamsForBusiness
}

// CalendarEventResult berisi hasil pembuatan event.
type CalendarEventResult struct {
	EventID          string
	CalendarLink     string
	OnlineMeetingURL string
}

type graphEventBody struct {
	Subject                    string          `json:"subject"`
	Body                       *graphBody      `json:"body,omitempty"`
	Start                      graphDateTime   `json:"start"`
	End                        graphDateTime   `json:"end"`
	Location                   *graphLocation  `json:"location,omitempty"`
	Attendees                  []graphAttendee `json:"attendees,omitempty"`
	IsReminderOn               bool            `json:"isReminderOn"`
	ReminderMinutesBeforeStart int             `json:"reminderMinutesBeforeStart"`
	IsOnlineMeeting            bool            `json:"isOnlineMeeting,omitempty"`
	OnlineMeetingProvider      string          `json:"onlineMeetingProvider,omitempty"`
}

type graphDateTime struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

type graphLocation struct {
	DisplayName string `json:"displayName"`
}

type graphAttendee struct {
	EmailAddress graphEmail `json:"emailAddress"`
	Type         string     `json:"type"`
}

type graphEventResponse struct {
	ID            string `json:"id"`
	WebLink       string `json:"webLink"`
	OnlineMeeting *struct {
		JoinURL string `json:"joinUrl"`
	} `json:"onlineMeeting"`
}

// CreateEvent membuat event baru di O365 Calendar milik userUPN.
func (g *GraphClient) CreateEvent(ev CalendarEvent) (*CalendarEventResult, error) {
	if ev.DurationMinutes <= 0 {
		ev.DurationMinutes = 60
	}
	if ev.ReminderMinutes <= 0 {
		ev.ReminderMinutes = 180
	}

	startTime, err := time.Parse(time.RFC3339, ev.Datetime)
	if err != nil {
		return nil, fmt.Errorf("format datetime tidak valid (gunakan RFC3339): %w", err)
	}
	endTime := startTime.Add(time.Duration(ev.DurationMinutes) * time.Minute)

	const graphFmt = "2006-01-02T15:04:05" // tanpa offset; timeZone terpisah

	body := graphEventBody{
		Subject:                    ev.Title,
		Start:                      graphDateTime{DateTime: startTime.Format(graphFmt), TimeZone: "Asia/Jakarta"},
		End:                        graphDateTime{DateTime: endTime.Format(graphFmt), TimeZone: "Asia/Jakarta"},
		IsReminderOn:               true,
		ReminderMinutesBeforeStart: ev.ReminderMinutes,
	}
	if ev.Body != "" {
		body.Body = &graphBody{ContentType: "HTML", Content: ev.Body}
	}
	if ev.Venue != "" {
		body.Location = &graphLocation{DisplayName: ev.Venue}
	}
	if ev.IsOnline {
		body.IsOnlineMeeting = true
		body.OnlineMeetingProvider = "teamsForBusiness"
	}
	for _, email := range ev.Attendees {
		body.Attendees = append(body.Attendees, graphAttendee{
			EmailAddress: graphEmail{Address: strings.TrimSpace(email)},
			Type:         "required",
		})
	}

	payload, _ := json.Marshal(body)
	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/calendar/events", g.userUPN)

	resp, err := g.doRequest(http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create event request gagal: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("create event HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}

	var result graphEventResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode event response gagal: %w", err)
	}

	r := &CalendarEventResult{EventID: result.ID, CalendarLink: result.WebLink}
	if result.OnlineMeeting != nil {
		r.OnlineMeetingURL = result.OnlineMeeting.JoinURL
	}
	log.Printf("[calendar] event dibuat: id=%s link=%s online=%s", r.EventID, r.CalendarLink, r.OnlineMeetingURL)
	return r, nil
}

// UpdateEvent mem-PATCH event yang sudah ada. Hanya field relevan yang dikirim:
// subject, start/end, dan location bila venue diisi. Online meeting/join URL
// Teams tidak diubah, jadi tautan tetap berlaku. Mengembalikan link kalender
// dan join URL terbaru bila event online.
func (g *GraphClient) UpdateEvent(eventID string, ev CalendarEvent) (*CalendarEventResult, error) {
	if strings.TrimSpace(eventID) == "" {
		return nil, fmt.Errorf("eventID kosong")
	}
	if ev.DurationMinutes <= 0 {
		ev.DurationMinutes = 60
	}
	startTime, err := time.Parse(time.RFC3339, ev.Datetime)
	if err != nil {
		return nil, fmt.Errorf("format datetime tidak valid (gunakan RFC3339): %w", err)
	}
	endTime := startTime.Add(time.Duration(ev.DurationMinutes) * time.Minute)

	const graphFmt = "2006-01-02T15:04:05"

	// PATCH parsial: hanya start/end (+ subject & location bila diisi).
	patch := map[string]any{
		"start": graphDateTime{DateTime: startTime.Format(graphFmt), TimeZone: "Asia/Jakarta"},
		"end":   graphDateTime{DateTime: endTime.Format(graphFmt), TimeZone: "Asia/Jakarta"},
	}
	if ev.Title != "" {
		patch["subject"] = ev.Title
	}
	if ev.Venue != "" {
		patch["location"] = graphLocation{DisplayName: ev.Venue}
	}

	payload, _ := json.Marshal(patch)
	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/calendar/events/%s", g.userUPN, url.PathEscape(eventID))

	resp, err := g.doRequest(http.MethodPatch, apiURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("update event request gagal: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("update event HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}

	var result graphEventResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode update event response gagal: %w", err)
	}
	r := &CalendarEventResult{EventID: result.ID, CalendarLink: result.WebLink}
	if result.OnlineMeeting != nil {
		r.OnlineMeetingURL = result.OnlineMeeting.JoinURL
	}
	log.Printf("[calendar] event diperbarui: id=%s link=%s online=%s", r.EventID, r.CalendarLink, r.OnlineMeetingURL)
	return r, nil
}

// CancelEvent menghapus event dari kalender userUPN (DELETE). 204/200 = sukses.
// Pemberitahuan ke peserta ditangani terpisah (email pembatalan + WhatsApp), jadi
// DELETE polos sudah cukup. Event yang sudah tak ada dianggap sukses (idempoten).
func (g *GraphClient) CancelEvent(eventID string) error {
	if strings.TrimSpace(eventID) == "" {
		return fmt.Errorf("eventID kosong")
	}
	apiURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/users/%s/calendar/events/%s", g.userUPN, url.PathEscape(eventID))
	resp, err := g.doRequest(http.MethodDelete, apiURL, nil)
	if err != nil {
		return fmt.Errorf("delete event request gagal: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		log.Printf("[calendar] event %s sudah tidak ada — dianggap terhapus", eventID)
		return nil
	}
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete event HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}
	log.Printf("[calendar] event dihapus: id=%s", eventID)
	return nil
}

// ── Calendar View (Availability) ──────────────────────────────────────

// AvailabilityEvent merepresentasikan satu event di Calendar View.
type AvailabilityEvent struct {
	Subject  string `json:"subject"`
	Start    string `json:"start"`
	End      string `json:"end"`
	Location string `json:"location"`
	IsOnline bool   `json:"isOnline"`
}

type graphCalendarViewResponse struct {
	Value []struct {
		Subject string `json:"subject"`
		Start   struct {
			DateTime string `json:"dateTime"`
		} `json:"start"`
		End struct {
			DateTime string `json:"dateTime"`
		} `json:"end"`
		Location struct {
			DisplayName string `json:"displayName"`
		} `json:"location"`
		IsOnlineMeeting bool `json:"isOnlineMeeting"`
	} `json:"value"`
}

// ListEvents mengambil semua event pada tanggal (YYYY-MM-DD) dari calendar userUPN.
func (g *GraphClient) ListEvents(date string) ([]AvailabilityEvent, error) {
	startDT := date + "T00:00:00"
	endDT := date + "T23:59:59"

	apiURL := fmt.Sprintf(
		"https://graph.microsoft.com/v1.0/users/%s/calendarView?startDateTime=%s&endDateTime=%s&$select=subject,start,end,location,isOnlineMeeting",
		g.userUPN, url.QueryEscape(startDT), url.QueryEscape(endDT),
	)

	token, err := g.getToken()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Prefer", `outlook.timezone="Asia/Jakarta"`)

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list events request gagal: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list events HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}

	var result graphCalendarViewResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode calendar view gagal: %w", err)
	}

	events := make([]AvailabilityEvent, 0, len(result.Value))
	for _, e := range result.Value {
		events = append(events, AvailabilityEvent{
			Subject:  e.Subject,
			Start:    e.Start.DateTime,
			End:      e.End.DateTime,
			Location: e.Location.DisplayName,
			IsOnline: e.IsOnlineMeeting,
		})
	}
	log.Printf("[calendar] availability date=%s: %d event(s)", date, len(events))
	return events, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
