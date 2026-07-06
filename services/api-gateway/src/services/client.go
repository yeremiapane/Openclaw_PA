// Package services adalah lapisan integrasi layanan eksternal (MS Graph:
// Calendar + Email) milik gateway. Sejak Fase 9 digabung, ini BUKAN lagi klien
// HTTP ke micro-service terpisah — semua dijalankan in-process lewat GraphClient,
// sehingga tak ada port polos tanpa auth dan kredensial hanya dimuat di gateway.
//
// Satu-satunya jalur ke Calendar/Email adalah lewat Client di bawah, yang hanya
// dipanggil oleh approval gate setelah SU menyetujui (lihat scheduleApprovedMeeting).
//
// Teams: tidak ada service teams terpisah. Link Teams diperoleh otomatis dari
// Calendar create event (IsOnline=true → OnlineMeetingURL).
package services

import (
	"context"
	"fmt"
	"time"
)

// Client menyatukan akses Calendar + Email via MS Graph.
type Client struct {
	graph     *GraphClient
	templates *TemplateEngine
	fromEmail string // = MS_GRAPH_USER_UPN (pa@hypernet.co.id)
	fromName  string
	enabled   bool
}

// New membuat Client. enabled=false (fitur nonaktif, graceful) bila kredensial
// MS Graph belum lengkap — approval gate tetap jalan tanpa penjadwalan. refreshToken
// opsional: dipakai sebagai fallback baca email (Email Watch) bila izin aplikasi belum ada.
func New(tenantID, clientID, clientSecret, userUPN, refreshToken, fromName, signaturePath string) *Client {
	enabled := tenantID != "" && clientID != "" && clientSecret != "" && userUPN != ""
	return &Client{
		graph:     NewGraphClient(tenantID, clientID, clientSecret, userUPN, refreshToken),
		templates: NewTemplateEngine(signaturePath),
		fromEmail: userUPN,
		fromName:  fromName,
		enabled:   enabled,
	}
}

// Enabled true bila kredensial MS Graph lengkap.
func (c *Client) Enabled() bool { return c != nil && c.enabled }

// ListRecentEmails mengambil email inbox terbaru (untuk Email Watch). sinceISO opsional
// (RFC3339 UTC) memfilter yang lebih baru; top membatasi jumlah. Mencoba izin aplikasi
// dulu lalu fallback refresh token (lihat GraphClient.ListRecentMessages).
func (c *Client) ListRecentEmails(_ context.Context, sinceISO string, top int) ([]EmailMessage, error) {
	return c.graph.ListRecentMessages(sinceISO, top)
}

// ListSentReplies mengambil email terkirim (folder Sent) sejak sinceISO untuk mendeteksi
// email inbox yang sudah dibalas (Email Watch, Opsi A). Lihat GraphClient.ListSentMessages.
func (c *Client) ListSentReplies(_ context.Context, sinceISO string, top int) ([]SentReply, error) {
	return c.graph.ListSentMessages(sinceISO, top)
}

// ── Calendar ──────────────────────────────────────────────────────────

// CreateEventReq = parameter pembuatan event kalender.
type CreateEventReq struct {
	Title           string
	Datetime        string // RFC3339
	DurationMinutes int
	Venue           string
	Attendees       []string
	IsOnline        bool
	Body            string
}

// EventResult = hasil pembuatan event.
type EventResult struct {
	EventID          string
	CalendarLink     string
	OnlineMeetingURL string
}

// CreateEvent membuat event di O365 Calendar (online → dapat Teams joinUrl).
func (c *Client) CreateEvent(_ context.Context, req CreateEventReq) (*EventResult, error) {
	res, err := c.graph.CreateEvent(CalendarEvent{
		Title:           req.Title,
		Datetime:        req.Datetime,
		DurationMinutes: req.DurationMinutes,
		Venue:           req.Venue,
		Attendees:       req.Attendees,
		Body:            req.Body,
		IsOnline:        req.IsOnline,
	})
	if err != nil {
		return nil, err
	}
	return &EventResult{
		EventID:          res.EventID,
		CalendarLink:     res.CalendarLink,
		OnlineMeetingURL: res.OnlineMeetingURL,
	}, nil
}

// Availability mengambil event SU pada tanggal (YYYY-MM-DD) untuk cek ketersediaan.
func (c *Client) Availability(_ context.Context, date string) ([]AvailabilityEvent, error) {
	return c.graph.ListEvents(date)
}

// UpdateEventReq = parameter reschedule event kalender yang sudah ada.
type UpdateEventReq struct {
	EventID         string
	Title           string // opsional
	Datetime        string // RFC3339 — waktu MULAI baru
	DurationMinutes int
	Venue           string // opsional; kosong = tidak mengubah lokasi
}

// RescheduleEvent mem-PATCH event O365 ke jadwal baru (Teams join URL tetap sama).
func (c *Client) RescheduleEvent(_ context.Context, req UpdateEventReq) (*EventResult, error) {
	res, err := c.graph.UpdateEvent(req.EventID, CalendarEvent{
		Title:           req.Title,
		Datetime:        req.Datetime,
		DurationMinutes: req.DurationMinutes,
		Venue:           req.Venue,
	})
	if err != nil {
		return nil, err
	}
	return &EventResult{
		EventID:          res.EventID,
		CalendarLink:     res.CalendarLink,
		OnlineMeetingURL: res.OnlineMeetingURL,
	}, nil
}

// CancelEvent menghapus event O365 (dipakai saat meeting dibatalkan).
func (c *Client) CancelEvent(_ context.Context, eventID string) error {
	return c.graph.CancelEvent(eventID)
}

// ── Email ─────────────────────────────────────────────────────────────

// RSVPReq = parameter undangan meeting (RSVP + .ics).
type RSVPReq struct {
	To              string
	ToName          string
	Title           string
	Datetime        string // RFC3339
	DurationMinutes int
	Venue           string
	CalendarLink    string
	TeamsLink       string
}

// SendRSVP merender undangan meeting + .ics lalu mengirim email ke kontak.
func (c *Client) SendRSVP(_ context.Context, req RSVPReq) error {
	if req.DurationMinutes <= 0 {
		req.DurationMinutes = 60
	}
	start, end, err := parseRange(req.Datetime, req.DurationMinutes)
	if err != nil {
		return err
	}

	venueDisplay := req.Venue
	if venueDisplay == "" && req.TeamsLink != "" {
		venueDisplay = "Online via Microsoft Teams"
	} else if venueDisplay == "" {
		venueDisplay = "Akan diinformasikan"
	}

	htmlBody, err := c.templates.RenderInvitation(MeetingEmailData{
		ToName:       req.ToName,
		FromName:     c.fromName,
		Title:        req.Title,
		Date:         FormatDateIndonesian(start),
		Time:         FormatTimeRange(start, end),
		Venue:        venueDisplay,
		TeamsLink:    req.TeamsLink,
		CalendarLink: req.CalendarLink,
	})
	if err != nil {
		return fmt.Errorf("render undangan gagal: %w", err)
	}

	ics := GenerateICS(ICSEvent{
		Title:        req.Title,
		Description:  "Meeting " + req.Title,
		Location:     req.Venue,
		Organizer:    c.fromEmail,
		OrgName:      c.fromName,
		Attendee:     req.To,
		AttendeeName: req.ToName,
		StartTime:    start,
		EndTime:      end,
		TeamsLink:    req.TeamsLink,
	})

	return c.graph.SendMail(SendMailRequest{
		To:       req.To,
		ToName:   req.ToName,
		Subject:  "Undangan Meeting: " + req.Title,
		HTMLBody: htmlBody,
		Attachments: []MailAttachment{
			{Name: "meeting.ics", ContentType: "text/calendar; method=REQUEST", Content: ics},
		},
	})
}

// ConfirmationReq = parameter email konfirmasi (approved/rejected).
type ConfirmationReq struct {
	To              string
	ToName          string
	Title           string
	Datetime        string
	DurationMinutes int
	Venue           string
	Status          string // "approved" | "rejected"
	Reason          string
}

// SendConfirmation mengirim email konfirmasi disetujui/ditolak.
func (c *Client) SendConfirmation(_ context.Context, req ConfirmationReq) error {
	if req.DurationMinutes <= 0 {
		req.DurationMinutes = 60
	}
	start, end, err := parseRange(req.Datetime, req.DurationMinutes)
	if err != nil {
		return err
	}
	htmlBody, err := c.templates.RenderConfirmation(ConfirmationData{
		ToName: req.ToName, Title: req.Title,
		Date: FormatDateIndonesian(start), Time: FormatTimeRange(start, end),
		Venue: req.Venue, Status: req.Status, Reason: req.Reason,
	})
	if err != nil {
		return fmt.Errorf("render konfirmasi gagal: %w", err)
	}
	subject := "Disetujui: " + req.Title
	if req.Status == "rejected" {
		subject = "Ditolak: " + req.Title
	}
	return c.graph.SendMail(SendMailRequest{To: req.To, ToName: req.ToName, Subject: subject, HTMLBody: htmlBody})
}

// ReminderReq = parameter email pengingat meeting.
type ReminderReq struct {
	To              string
	ToName          string
	Title           string
	Datetime        string
	DurationMinutes int
	Venue           string
	TeamsLink       string
}

// SendReminder mengirim email pengingat meeting.
func (c *Client) SendReminder(_ context.Context, req ReminderReq) error {
	if req.DurationMinutes <= 0 {
		req.DurationMinutes = 60
	}
	start, end, err := parseRange(req.Datetime, req.DurationMinutes)
	if err != nil {
		return err
	}
	htmlBody, err := c.templates.RenderReminder(ReminderData{
		ToName: req.ToName, Title: req.Title,
		Date: FormatDateIndonesian(start), Time: FormatTimeRange(start, end),
		Venue: req.Venue, TeamsLink: req.TeamsLink,
	})
	if err != nil {
		return fmt.Errorf("render reminder gagal: %w", err)
	}
	return c.graph.SendMail(SendMailRequest{To: req.To, ToName: req.ToName, Subject: "Reminder: " + req.Title, HTMLBody: htmlBody})
}

// RescheduleReq = parameter email pemberitahuan perubahan jadwal (reschedule).
type RescheduleReq struct {
	To              string
	ToName          string
	Title           string
	OldDatetime     string // RFC3339 — jadwal lama
	NewDatetime     string // RFC3339 — jadwal baru
	DurationMinutes int
	Venue           string
	CalendarLink    string
	TeamsLink       string
	Reason          string
}

// SendReschedule mengirim email pemberitahuan perubahan jadwal (lama → baru) +
// melampirkan .ics jadwal baru agar kalender peserta bisa diperbarui.
func (c *Client) SendReschedule(_ context.Context, req RescheduleReq) error {
	if req.DurationMinutes <= 0 {
		req.DurationMinutes = 60
	}
	oldStart, oldEnd, err := parseRange(req.OldDatetime, req.DurationMinutes)
	if err != nil {
		return err
	}
	newStart, newEnd, err := parseRange(req.NewDatetime, req.DurationMinutes)
	if err != nil {
		return err
	}

	venueDisplay := req.Venue
	if venueDisplay == "" && req.TeamsLink != "" {
		venueDisplay = "Online via Microsoft Teams"
	} else if venueDisplay == "" {
		venueDisplay = "Akan diinformasikan"
	}

	htmlBody, err := c.templates.RenderReschedule(RescheduleData{
		ToName:       req.ToName,
		Title:        req.Title,
		OldDate:      FormatDateIndonesian(oldStart),
		OldTime:      FormatTimeRange(oldStart, oldEnd),
		NewDate:      FormatDateIndonesian(newStart),
		NewTime:      FormatTimeRange(newStart, newEnd),
		Venue:        venueDisplay,
		TeamsLink:    req.TeamsLink,
		CalendarLink: req.CalendarLink,
		Reason:       req.Reason,
	})
	if err != nil {
		return fmt.Errorf("render reschedule gagal: %w", err)
	}

	ics := GenerateICS(ICSEvent{
		Title:        req.Title,
		Description:  "Meeting (jadwal diperbarui) " + req.Title,
		Location:     req.Venue,
		Organizer:    c.fromEmail,
		OrgName:      c.fromName,
		Attendee:     req.To,
		AttendeeName: req.ToName,
		StartTime:    newStart,
		EndTime:      newEnd,
		TeamsLink:    req.TeamsLink,
	})

	return c.graph.SendMail(SendMailRequest{
		To:       req.To,
		ToName:   req.ToName,
		Subject:  "Perubahan Jadwal: " + req.Title,
		HTMLBody: htmlBody,
		Attachments: []MailAttachment{
			{Name: "meeting.ics", ContentType: "text/calendar; method=REQUEST", Content: ics},
		},
	})
}

// CancellationReq = parameter email pembatalan meeting.
type CancellationReq struct {
	To              string
	ToName          string
	Title           string
	Datetime        string
	DurationMinutes int
	Reason          string
}

// SendCancellation mengirim email pemberitahuan pembatalan.
func (c *Client) SendCancellation(_ context.Context, req CancellationReq) error {
	if req.DurationMinutes <= 0 {
		req.DurationMinutes = 60
	}
	start, end, err := parseRange(req.Datetime, req.DurationMinutes)
	if err != nil {
		return err
	}
	htmlBody, err := c.templates.RenderCancellation(CancellationData{
		ToName: req.ToName, Title: req.Title,
		Date: FormatDateIndonesian(start), Time: FormatTimeRange(start, end),
		Reason: req.Reason,
	})
	if err != nil {
		return fmt.Errorf("render pembatalan gagal: %w", err)
	}
	return c.graph.SendMail(SendMailRequest{To: req.To, ToName: req.ToName, Subject: "Pembatalan: " + req.Title, HTMLBody: htmlBody})
}

// parseRange mem-parse RFC3339 dan menghitung waktu selesai.
func parseRange(datetime string, durationMinutes int) (start, end time.Time, err error) {
	start, err = time.Parse(time.RFC3339, datetime)
	if err != nil {
		return start, end, fmt.Errorf("format datetime tidak valid (gunakan RFC3339): %w", err)
	}
	end = start.Add(time.Duration(durationMinutes) * time.Minute)
	return start, end, nil
}
