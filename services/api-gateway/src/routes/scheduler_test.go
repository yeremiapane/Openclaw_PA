package routes

import (
	"strings"
	"testing"
	"time"

	"pa-ai/api-gateway/src/model"
)

func TestParseReminderTimeRFC3339(t *testing.T) {
	got, err := parseReminderTime("2026-07-01T15:00:00+07:00")
	if err != nil {
		t.Fatalf("RFC3339 gagal: %v", err)
	}
	want := time.Date(2026, 7, 1, 15, 0, 0, 0, wibZone)
	if !got.Equal(want) {
		t.Errorf("waktu: ingin %s dapat %s", want, got)
	}
}

func TestParseReminderTimeNoZoneAssumesWIB(t *testing.T) {
	// Tanpa zona → diasumsikan WIB (UTC+7). 15:00 WIB == 08:00 UTC.
	got, err := parseReminderTime("2026-07-01 15:00")
	if err != nil {
		t.Fatalf("tanpa zona gagal: %v", err)
	}
	if h := got.UTC().Hour(); h != 8 {
		t.Errorf("jam UTC: ingin 8 dapat %d (got=%s)", h, got)
	}
}

func TestParseReminderTimeInvalid(t *testing.T) {
	if _, err := parseReminderTime("besok pagi"); err == nil {
		t.Error("string non-tanggal harus error")
	}
	if _, err := parseReminderTime(""); err == nil {
		t.Error("string kosong harus error")
	}
}

func TestBuildReminderInstructionContainsNote(t *testing.T) {
	ins := buildReminderInstruction(model.ScheduledTask{Note: "Telepon dokter gigi"})
	if !strings.Contains(ins, "Telepon dokter gigi") {
		t.Errorf("instruksi tidak memuat catatan: %q", ins)
	}
	if !strings.Contains(ins, "Pak Sudianto") {
		t.Errorf("instruksi tidak menyebut penerima: %q", ins)
	}
}

func TestParseHHMM(t *testing.T) {
	cases := []struct {
		in     string
		hh, mm int
		ok     bool
	}{
		{"08:00", 8, 0, true},
		{"23:59", 23, 59, true},
		{" 9:5 ", 9, 5, true},
		{"24:00", 0, 0, false},
		{"08:60", 0, 0, false},
		{"8", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		hh, mm, ok := parseHHMM(c.in)
		if ok != c.ok || (ok && (hh != c.hh || mm != c.mm)) {
			t.Errorf("parseHHMM(%q) = (%d,%d,%v), ingin (%d,%d,%v)", c.in, hh, mm, ok, c.hh, c.mm, c.ok)
		}
	}
}

func TestNextOccurrenceDailyBumpsToTomorrow(t *testing.T) {
	// from lewat jam slot → slot berikutnya besok.
	from := time.Date(2026, 7, 2, 9, 0, 0, 0, wibZone)
	got, ok := nextOccurrence(model.ScheduledTask{RecurKind: "daily", RecurTime: "08:00"}, from)
	if !ok {
		t.Fatal("daily: ok=false")
	}
	want := time.Date(2026, 7, 3, 8, 0, 0, 0, wibZone)
	if !got.Equal(want) {
		t.Errorf("daily besok: ingin %s dapat %s", want, got)
	}
}

func TestNextOccurrenceDailySameDay(t *testing.T) {
	// from sebelum jam slot → slot hari ini.
	from := time.Date(2026, 7, 2, 7, 0, 0, 0, wibZone)
	got, ok := nextOccurrence(model.ScheduledTask{RecurKind: "daily", RecurTime: "08:00"}, from)
	if !ok {
		t.Fatal("daily: ok=false")
	}
	want := time.Date(2026, 7, 2, 8, 0, 0, 0, wibZone)
	if !got.Equal(want) {
		t.Errorf("daily hari ini: ingin %s dapat %s", want, got)
	}
}

func TestNextOccurrenceWeekly(t *testing.T) {
	dow := int(time.Monday) // 1
	from := time.Date(2026, 7, 2, 12, 0, 0, 0, wibZone)
	got, ok := nextOccurrence(model.ScheduledTask{RecurKind: "weekly", RecurTime: "09:00", RecurDow: &dow}, from)
	if !ok {
		t.Fatal("weekly: ok=false")
	}
	if got.Weekday() != time.Monday {
		t.Errorf("weekly: hari %s, ingin Monday", got.Weekday())
	}
	if got.Hour() != 9 || got.Minute() != 0 {
		t.Errorf("weekly: jam %02d:%02d, ingin 09:00", got.Hour(), got.Minute())
	}
	if !got.After(from) {
		t.Errorf("weekly: %s tidak setelah from %s", got, from)
	}
}

func TestNextOccurrenceInvalid(t *testing.T) {
	if _, ok := nextOccurrence(model.ScheduledTask{RecurKind: "daily", RecurTime: "bad"}, time.Now()); ok {
		t.Error("recurTime invalid harus ok=false")
	}
	if _, ok := nextOccurrence(model.ScheduledTask{RecurKind: "weekly", RecurTime: "09:00"}, time.Now()); ok {
		t.Error("weekly tanpa recurDow harus ok=false")
	}
	if _, ok := nextOccurrence(model.ScheduledTask{RecurKind: "none", RecurTime: "09:00"}, time.Now()); ok {
		t.Error("recurKind none harus ok=false")
	}
}
