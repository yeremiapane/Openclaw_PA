// HTML email templates untuk email meeting (port dari bekas email service).
// Setiap template menyertakan signature dari signature.html di bagian bawah.
package services

import (
	"bytes"
	"fmt"
	"html/template"
	"log"
	"os"
	"strings"
	"time"
)

// TemplateEngine menampung signature HTML untuk disisipkan ke tiap email.
type TemplateEngine struct {
	signatureHTML string
}

// NewTemplateEngine memuat signature.html (boleh kosong bila tak ditemukan).
func NewTemplateEngine(signaturePath string) *TemplateEngine {
	sig := ""
	if data, err := os.ReadFile(signaturePath); err != nil {
		log.Printf("[templates] PERINGATAN: gagal baca signature (%s): %v — email tanpa signature", signaturePath, err)
	} else {
		sig = string(data)
		log.Printf("[templates] signature dimuat dari %s (%d bytes)", signaturePath, len(data))
	}
	return &TemplateEngine{signatureHTML: sig}
}

// ── Template Data Structs ─────────────────────────────────────────────

// MeetingEmailData berisi data untuk template undangan meeting.
type MeetingEmailData struct {
	ToName       string
	FromName     string
	Title        string
	Date         string
	Time         string
	Venue        string
	TeamsLink    string
	CalendarLink string
}

// ConfirmationData berisi data untuk template konfirmasi (approved/rejected).
type ConfirmationData struct {
	ToName string
	Title  string
	Date   string
	Time   string
	Venue  string
	Status string // "approved" atau "rejected"
	Reason string
}

// ReminderData berisi data untuk template reminder.
type ReminderData struct {
	ToName    string
	Title     string
	Date      string
	Time      string
	Venue     string
	TeamsLink string
}

// CancellationData berisi data untuk template pembatalan.
type CancellationData struct {
	ToName string
	Title  string
	Date   string
	Time   string
	Reason string
}

// RescheduleData berisi data untuk template perubahan jadwal (lama → baru).
type RescheduleData struct {
	ToName       string
	Title        string
	OldDate      string
	OldTime      string
	NewDate      string
	NewTime      string
	Venue        string
	TeamsLink    string
	CalendarLink string
	Reason       string
}

// ── Format Helpers ────────────────────────────────────────────────────

var indonesianDay = map[time.Weekday]string{
	time.Sunday:    "Minggu",
	time.Monday:    "Senin",
	time.Tuesday:   "Selasa",
	time.Wednesday: "Rabu",
	time.Thursday:  "Kamis",
	time.Friday:    "Jumat",
	time.Saturday:  "Sabtu",
}

var indonesianMonth = map[time.Month]string{
	time.January:   "Januari",
	time.February:  "Februari",
	time.March:     "Maret",
	time.April:     "April",
	time.May:       "Mei",
	time.June:      "Juni",
	time.July:      "Juli",
	time.August:    "Agustus",
	time.September: "September",
	time.October:   "Oktober",
	time.November:  "November",
	time.December:  "Desember",
}

var wibZone = time.FixedZone("WIB", 7*3600)


func FormatDateIndonesian(t time.Time) string {
	t = t.In(wibZone)
	return fmt.Sprintf("%s, %d %s %d", indonesianDay[t.Weekday()], t.Day(), indonesianMonth[t.Month()], t.Year())
}

func FormatTimeRange(start, end time.Time) string {
	start, end = start.In(wibZone), end.In(wibZone)
	return fmt.Sprintf("%s - %s WIB", start.Format("15:04"), end.Format("15:04"))
}

// ── Template Rendering ───────────────────────────────────────────────

// RenderInvitation menghasilkan HTML email undangan meeting (RSVP).
func (te *TemplateEngine) RenderInvitation(data MeetingEmailData) (string, error) {
	tmpl := `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="margin:0; padding:0; background-color:#f4f4f4; font-family: Arial, Helvetica, sans-serif;">
<table width="100%" cellpadding="0" cellspacing="0" style="background-color:#f4f4f4; padding:20px 0;">
<tr><td align="center">
<table width="600" cellpadding="0" cellspacing="0" style="background-color:#ffffff; border-radius:8px; overflow:hidden; box-shadow: 0 2px 8px rgba(0,0,0,0.08);">

<!-- Body -->
<tr>
<td style="padding:32px;">
  <p style="font-size:14px; color:#333333; line-height:1.6; margin-top:0;">
    Yth. <strong>{{.ToName}}</strong>,
  </p>
  <p style="font-size:14px; color:#333333; line-height:1.6;">
    Dengan hormat, kami mengundang Bapak/Ibu untuk menghadiri meeting berikut:
  </p>

  <!-- Meeting Details Card -->
  <table width="100%" cellpadding="0" cellspacing="0" style="background-color:#f8f9fa; border-left:4px solid #1a237e; border-radius:4px; margin:24px 0;">
    <tr>
      <td style="padding:20px 24px;">
        <table width="100%" cellpadding="0" cellspacing="0" style="font-size:14px; color:#333333;">
          <tr>
            <td width="100" style="padding:6px 0; color:#666666; vertical-align:top;">Agenda</td>
            <td style="padding:6px 0; font-weight:600;">{{.Title}}</td>
          </tr>
          <tr>
            <td style="padding:6px 0; color:#666666; vertical-align:top;">Tanggal</td>
            <td style="padding:6px 0;">{{.Date}}</td>
          </tr>
          <tr>
            <td style="padding:6px 0; color:#666666; vertical-align:top;">Waktu</td>
            <td style="padding:6px 0;">{{.Time}}</td>
          </tr>
          <tr>
            <td style="padding:6px 0; color:#666666; vertical-align:top;">Alamat</td>
            <td style="padding:6px 0;">{{.Venue}}</td>
          </tr>{{if .TeamsLink}}
          <tr>
            <td style="padding:6px 0; color:#666666; vertical-align:top;">Link</td>
            <td style="padding:6px 0;"><a href="{{.TeamsLink}}" style="color:#1a237e; text-decoration:underline;">Bergabung via Microsoft Teams</a></td>
          </tr>{{end}}
        </table>
      </td>
    </tr>
  </table>

  <p style="font-size:14px; color:#333333; line-height:1.6;">
    Silakan konfirmasi kehadiran Anda. Anda juga dapat menambahkan jadwal ini ke kalender dengan membuka file <strong>.ics</strong> yang terlampir.
  </p>

</td>
</tr>

<!-- Separator -->
<tr><td style="padding:0 32px;"><hr style="border:none; border-top:1px solid #e0e0e0; margin:0;"></td></tr>

<!-- Signature -->
<tr>
<td style="padding:24px 32px;">
  ` + te.signatureHTML + `
</td>
</tr>

</table>
</td></tr>
</table>
</body>
</html>`

	return renderTemplate("invitation", tmpl, data)
}

// RenderConfirmation menghasilkan HTML email konfirmasi (approved/rejected).
func (te *TemplateEngine) RenderConfirmation(data ConfirmationData) (string, error) {
	isApproved := strings.EqualFold(data.Status, "approved")

	headerBg := "#2e7d32"
	headerTitle := "Meeting Disetujui"
	bodyText := "Kami dengan senang hati memberitahukan bahwa meeting berikut telah <strong>disetujui</strong>:"
	footerText := "Kami menantikan kehadiran Bapak/Ibu. Sampai bertemu!"

	if !isApproved {
		headerBg = "#c62828"
		headerTitle = "Meeting Ditolak"
		bodyText = "Dengan hormat, kami memberitahukan bahwa meeting berikut <strong>tidak dapat dilaksanakan</strong>:"
		footerText = "Mohon maaf atas ketidaknyamanannya. Kami akan menghubungi kembali jika ada jadwal alternatif."
		if data.Reason != "" {
			footerText = "Alasan: " + data.Reason + ". " + footerText
		}
	}

	tmplData := struct {
		ConfirmationData
		HeaderBg    string
		HeaderTitle string
		BodyText    template.HTML
		FooterText  string
	}{
		ConfirmationData: data,
		HeaderBg:         headerBg,
		HeaderTitle:      headerTitle,
		BodyText:         template.HTML(bodyText),
		FooterText:       footerText,
	}

	tmpl := `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="margin:0; padding:0; background-color:#f4f4f4; font-family: Arial, Helvetica, sans-serif;">
<table width="100%" cellpadding="0" cellspacing="0" style="background-color:#f4f4f4; padding:20px 0;">
<tr><td align="center">
<table width="600" cellpadding="0" cellspacing="0" style="background-color:#ffffff; border-radius:8px; overflow:hidden; box-shadow: 0 2px 8px rgba(0,0,0,0.08);">

<tr>
<td style="padding:32px;">
  <p style="font-size:14px; color:#333333; line-height:1.6; margin-top:0;">Yth. <strong>{{.ToName}}</strong>,</p>
  <p style="font-size:14px; color:#333333; line-height:1.6;">{{.BodyText}}</p>

  <table width="100%" cellpadding="0" cellspacing="0" style="background-color:#f8f9fa; border-left:4px solid {{.HeaderBg}}; border-radius:4px; margin:24px 0;">
    <tr>
      <td style="padding:20px 24px;">
        <table width="100%" cellpadding="0" cellspacing="0" style="font-size:14px; color:#333333;">
          <tr><td width="100" style="padding:6px 0; color:#666666;">Agenda</td><td style="padding:6px 0; font-weight:600;">{{.Title}}</td></tr>
          <tr><td style="padding:6px 0; color:#666666;">Tanggal</td><td style="padding:6px 0;">{{.Date}}</td></tr>
          <tr><td style="padding:6px 0; color:#666666;">Waktu</td><td style="padding:6px 0;">{{.Time}}</td></tr>
          {{if .Venue}}<tr><td style="padding:6px 0; color:#666666;">Lokasi</td><td style="padding:6px 0;">{{.Venue}}</td></tr>{{end}}
        </table>
      </td>
    </tr>
  </table>

  <p style="font-size:14px; color:#333333; line-height:1.6;">{{.FooterText}}</p>
</td>
</tr>

<tr><td style="padding:0 32px;"><hr style="border:none; border-top:1px solid #e0e0e0; margin:0;"></td></tr>
<tr><td style="padding:24px 32px;">` + te.signatureHTML + `</td></tr>

</table>
</td></tr>
</table>
</body>
</html>`

	return renderTemplate("confirmation", tmpl, tmplData)
}

// RenderReminder menghasilkan HTML email reminder meeting.
func (te *TemplateEngine) RenderReminder(data ReminderData) (string, error) {
	tmpl := `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="margin:0; padding:0; background-color:#f4f4f4; font-family: Arial, Helvetica, sans-serif;">
<table width="100%" cellpadding="0" cellspacing="0" style="background-color:#f4f4f4; padding:20px 0;">
<tr><td align="center">
<table width="600" cellpadding="0" cellspacing="0" style="background-color:#ffffff; border-radius:8px; overflow:hidden; box-shadow: 0 2px 8px rgba(0,0,0,0.08);">

<tr>
<td style="background-color:#f57f17; padding:28px 32px;">
  <h1 style="color:#ffffff; margin:0; font-size:22px; font-weight:600;"> Pengingat Meeting</h1>
</td>
</tr>

<tr>
<td style="padding:32px;">
  <p style="font-size:14px; color:#333333; line-height:1.6; margin-top:0;">Yth. <strong>{{.ToName}}</strong>,</p>
  <p style="font-size:14px; color:#333333; line-height:1.6;">
    Ini adalah pengingat untuk meeting yang akan segera berlangsung:
  </p>

  <table width="100%" cellpadding="0" cellspacing="0" style="background-color:#fff8e1; border-left:4px solid #f57f17; border-radius:4px; margin:24px 0;">
    <tr>
      <td style="padding:20px 24px;">
        <table width="100%" cellpadding="0" cellspacing="0" style="font-size:14px; color:#333333;">
          <tr><td width="100" style="padding:6px 0; color:#666666;">Agenda</td><td style="padding:6px 0; font-weight:600;">{{.Title}}</td></tr>
          <tr><td style="padding:6px 0; color:#666666;">Tanggal</td><td style="padding:6px 0;">{{.Date}}</td></tr>
          <tr><td style="padding:6px 0; color:#666666;">Waktu</td><td style="padding:6px 0;">{{.Time}}</td></tr>
          <tr><td style="padding:6px 0; color:#666666;">Lokasi</td><td style="padding:6px 0;">{{.Venue}}</td></tr>
          {{if .TeamsLink}}<tr><td style="padding:6px 0; color:#666666;">Link</td><td style="padding:6px 0;"><a href="{{.TeamsLink}}" style="color:#1a237e;">Bergabung via Teams</a></td></tr>{{end}}
        </table>
      </td>
    </tr>
  </table>

  <p style="font-size:14px; color:#333333; line-height:1.6;">Mohon pastikan kehadiran Anda tepat waktu. Terima kasih.</p>
</td>
</tr>

<tr><td style="padding:0 32px;"><hr style="border:none; border-top:1px solid #e0e0e0; margin:0;"></td></tr>
<tr><td style="padding:24px 32px;">` + te.signatureHTML + `</td></tr>

</table>
</td></tr>
</table>
</body>
</html>`

	return renderTemplate("reminder", tmpl, data)
}

// RenderCancellation menghasilkan HTML email pembatalan meeting.
func (te *TemplateEngine) RenderCancellation(data CancellationData) (string, error) {
	tmpl := `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="margin:0; padding:0; background-color:#f4f4f4; font-family: Arial, Helvetica, sans-serif;">
<table width="100%" cellpadding="0" cellspacing="0" style="background-color:#f4f4f4; padding:20px 0;">
<tr><td align="center">
<table width="600" cellpadding="0" cellspacing="0" style="background-color:#ffffff; border-radius:8px; overflow:hidden; box-shadow: 0 2px 8px rgba(0,0,0,0.08);">

<tr>
<td style="background-color:#616161; padding:28px 32px;">
  <h1 style="color:#ffffff; margin:0; font-size:22px; font-weight:600;">Meeting Dibatalkan</h1>
</td>
</tr>

<tr>
<td style="padding:32px;">
  <p style="font-size:14px; color:#333333; line-height:1.6; margin-top:0;">Yth. <strong>{{.ToName}}</strong>,</p>
  <p style="font-size:14px; color:#333333; line-height:1.6;">
    Dengan hormat, kami memberitahukan bahwa meeting berikut <strong>dibatalkan</strong>:
  </p>

  <table width="100%" cellpadding="0" cellspacing="0" style="background-color:#f5f5f5; border-left:4px solid #616161; border-radius:4px; margin:24px 0;">
    <tr>
      <td style="padding:20px 24px;">
        <table width="100%" cellpadding="0" cellspacing="0" style="font-size:14px; color:#333333;">
          <tr><td width="100" style="padding:6px 0; color:#666666;">Agenda</td><td style="padding:6px 0; font-weight:600; text-decoration:line-through;">{{.Title}}</td></tr>
          <tr><td style="padding:6px 0; color:#666666;">Tanggal</td><td style="padding:6px 0; text-decoration:line-through;">{{.Date}}</td></tr>
          <tr><td style="padding:6px 0; color:#666666;">Waktu</td><td style="padding:6px 0; text-decoration:line-through;">{{.Time}}</td></tr>
        </table>
      </td>
    </tr>
  </table>

  {{if .Reason}}<p style="font-size:14px; color:#333333; line-height:1.6;"><strong>Alasan:</strong> {{.Reason}}</p>{{end}}
  <p style="font-size:14px; color:#333333; line-height:1.6;">Mohon maaf atas ketidaknyamanannya. Kami akan menghubungi kembali untuk jadwal pengganti jika diperlukan.</p>
</td>
</tr>

<tr><td style="padding:0 32px;"><hr style="border:none; border-top:1px solid #e0e0e0; margin:0;"></td></tr>
<tr><td style="padding:24px 32px;">` + te.signatureHTML + `</td></tr>

</table>
</td></tr>
</table>
</body>
</html>`

	return renderTemplate("cancellation", tmpl, data)
}

// RenderReschedule menghasilkan HTML email perubahan jadwal (jadwal lama dicoret,
// jadwal baru ditonjolkan).
func (te *TemplateEngine) RenderReschedule(data RescheduleData) (string, error) {
	tmpl := `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body style="margin:0; padding:0; background-color:#f4f4f4; font-family: Arial, Helvetica, sans-serif;">
<table width="100%" cellpadding="0" cellspacing="0" style="background-color:#f4f4f4; padding:20px 0;">
<tr><td align="center">
<table width="600" cellpadding="0" cellspacing="0" style="background-color:#ffffff; border-radius:8px; overflow:hidden; box-shadow: 0 2px 8px rgba(0,0,0,0.08);">

<tr>
<td style="background-color:#1565c0; padding:28px 32px;">
  <h1 style="color:#ffffff; margin:0; font-size:22px; font-weight:600;">Perubahan Jadwal Meeting</h1>
</td>
</tr>

<tr>
<td style="padding:32px;">
  <p style="font-size:14px; color:#333333; line-height:1.6; margin-top:0;">Yth. <strong>{{.ToName}}</strong>,</p>
  <p style="font-size:14px; color:#333333; line-height:1.6;">
    Dengan hormat, kami memberitahukan bahwa jadwal meeting <strong>{{.Title}}</strong> mengalami perubahan:
  </p>

  <table width="100%" cellpadding="0" cellspacing="0" style="background-color:#fbe9e7; border-left:4px solid #d32f2f; border-radius:4px; margin:16px 0;">
    <tr><td style="padding:16px 24px;">
      <p style="margin:0; font-size:12px; color:#999999; text-transform:uppercase;">Jadwal Lama</p>
      <p style="margin:4px 0 0; font-size:14px; color:#333333; text-decoration:line-through;">{{.OldDate}} &bull; {{.OldTime}}</p>
    </td></tr>
  </table>

  <table width="100%" cellpadding="0" cellspacing="0" style="background-color:#e8f5e9; border-left:4px solid #2e7d32; border-radius:4px; margin:0 0 24px;">
    <tr><td style="padding:20px 24px;">
      <p style="margin:0; font-size:12px; color:#2e7d32; text-transform:uppercase; font-weight:600;">Jadwal Baru</p>
      <table width="100%" cellpadding="0" cellspacing="0" style="font-size:14px; color:#333333; margin-top:8px;">
        <tr><td width="100" style="padding:6px 0; color:#666666;">Agenda</td><td style="padding:6px 0; font-weight:600;">{{.Title}}</td></tr>
        <tr><td style="padding:6px 0; color:#666666;">Tanggal</td><td style="padding:6px 0; font-weight:600;">{{.NewDate}}</td></tr>
        <tr><td style="padding:6px 0; color:#666666;">Waktu</td><td style="padding:6px 0; font-weight:600;">{{.NewTime}}</td></tr>
        <tr><td style="padding:6px 0; color:#666666;">Lokasi</td><td style="padding:6px 0;">{{.Venue}}</td></tr>
      </table>
    </td></tr>
  </table>

  {{if .TeamsLink}}<p style="font-size:14px; line-height:1.6;"><a href="{{.TeamsLink}}" style="background-color:#1565c0; color:#ffffff; padding:10px 20px; border-radius:4px; text-decoration:none; display:inline-block;">Gabung via Microsoft Teams</a></p>{{end}}
  {{if .Reason}}<p style="font-size:14px; color:#333333; line-height:1.6;"><strong>Alasan:</strong> {{.Reason}}</p>{{end}}
  <p style="font-size:14px; color:#333333; line-height:1.6;">Mohon maaf atas perubahan ini. Undangan kalender terbaru terlampir. Terima kasih atas pengertiannya.</p>
</td>
</tr>

<tr><td style="padding:0 32px;"><hr style="border:none; border-top:1px solid #e0e0e0; margin:0;"></td></tr>
<tr><td style="padding:24px 32px;">` + te.signatureHTML + `</td></tr>

</table>
</td></tr>
</table>
</body>
</html>`

	return renderTemplate("reschedule", tmpl, data)
}

// renderTemplate mem-parse dan mengeksekusi template HTML.
func renderTemplate(name, tmpl string, data any) (string, error) {
	t, err := template.New(name).Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template %s gagal: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template %s gagal: %w", name, err)
	}
	return buf.String(), nil
}
