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
