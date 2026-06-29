// Generator file .ics (iCalendar) untuk RSVP attachment (port dari email service).
// Menghasilkan VEVENT standar yang bisa di-import ke semua calendar client.
package services

import (
	"fmt"
	"strings"
	"time"
)

// ICSEvent berisi data untuk membuat file .ics.
type ICSEvent struct {
	Title        string
	Description  string
	Location     string
	Organizer    string // email organizer
	OrgName      string // nama organizer
	Attendee     string // email attendee
	AttendeeName string
	StartTime    time.Time
	EndTime      time.Time
	TeamsLink    string // opsional; ditambahkan ke description
}

// GenerateICS membuat konten file .ics (iCalendar) dari ICSEvent.
func GenerateICS(ev ICSEvent) []byte {
	uid := fmt.Sprintf("%d-%s@pa-ai.hypernet.co.id", ev.StartTime.UnixNano(), sanitizeUID(ev.Title))

	dtStart := ev.StartTime.UTC().Format("20060102T150405Z")
	dtEnd := ev.EndTime.UTC().Format("20060102T150405Z")
	dtStamp := time.Now().UTC().Format("20060102T150405Z")

	desc := ev.Description
	if ev.TeamsLink != "" {
		if desc != "" {
			desc += "\\n\\n"
		}
		desc += "Link Meeting (Teams): " + ev.TeamsLink
	}

	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//PA AI System//Hypernet Technologies//ID\r\n")
	b.WriteString("CALSCALE:GREGORIAN\r\n")
	b.WriteString("METHOD:REQUEST\r\n")
	b.WriteString("BEGIN:VEVENT\r\n")
	b.WriteString(fmt.Sprintf("UID:%s\r\n", uid))
	b.WriteString(fmt.Sprintf("DTSTAMP:%s\r\n", dtStamp))
	b.WriteString(fmt.Sprintf("DTSTART:%s\r\n", dtStart))
	b.WriteString(fmt.Sprintf("DTEND:%s\r\n", dtEnd))
	b.WriteString(fmt.Sprintf("SUMMARY:%s\r\n", escapeICS(ev.Title)))

	if desc != "" {
		b.WriteString(fmt.Sprintf("DESCRIPTION:%s\r\n", escapeICS(desc)))
	}
	if ev.Location != "" {
		b.WriteString(fmt.Sprintf("LOCATION:%s\r\n", escapeICS(ev.Location)))
	}
	if ev.Organizer != "" {
		orgName := ev.OrgName
		if orgName == "" {
			orgName = "PA Asisten"
		}
		b.WriteString(fmt.Sprintf("ORGANIZER;CN=%s:mailto:%s\r\n", orgName, ev.Organizer))
	}
	if ev.Attendee != "" {
		attName := ev.AttendeeName
		if attName == "" {
			attName = ev.Attendee
		}
		b.WriteString(fmt.Sprintf("ATTENDEE;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE;CN=%s:mailto:%s\r\n", attName, ev.Attendee))
	}

	b.WriteString("STATUS:CONFIRMED\r\n")
	b.WriteString("SEQUENCE:0\r\n")

	// Alarm reminder 30 menit sebelum.
	b.WriteString("BEGIN:VALARM\r\n")
	b.WriteString("TRIGGER:-PT30M\r\n")
	b.WriteString("ACTION:DISPLAY\r\n")
	b.WriteString(fmt.Sprintf("DESCRIPTION:Reminder: %s\r\n", escapeICS(ev.Title)))
	b.WriteString("END:VALARM\r\n")

	b.WriteString("END:VEVENT\r\n")
	b.WriteString("END:VCALENDAR\r\n")

	return []byte(b.String())
}

// escapeICS meng-escape karakter khusus untuk format iCalendar.
func escapeICS(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, ";", "\\;")
	s = strings.ReplaceAll(s, ",", "\\,")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "")
	return s
}

// sanitizeUID membuat string aman untuk UID iCalendar.
func sanitizeUID(s string) string {
	var result strings.Builder
	for _, c := range strings.ToLower(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result.WriteRune(c)
		}
	}
	uid := result.String()
	if len(uid) > 32 {
		uid = uid[:32]
	}
	return uid
}
