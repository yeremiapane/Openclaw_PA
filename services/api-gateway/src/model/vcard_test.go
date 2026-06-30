package model

import "testing"

func TestContactsFromRealVCard(t *testing.T) {
	e := &WahaEvent{}
	e.Payload.VCards = []string{
		"BEGIN:VCARD\nVERSION:3.0\nN:Pane;Yeremia;;;\nFN:Yeremia Pane\nTEL;type=Mobile;waid=628970258733:+62 897-0258-733\nEND:VCARD",
	}
	cs := e.Contacts()
	if len(cs) != 1 {
		t.Fatalf("ingin 1 kontak, dapat %d", len(cs))
	}
	if cs[0].Name != "Yeremia Pane" {
		t.Errorf("nama: ingin %q dapat %q", "Yeremia Pane", cs[0].Name)
	}
	if cs[0].Phone != "628970258733" {
		t.Errorf("nomor: ingin %q dapat %q", "628970258733", cs[0].Phone)
	}
	if e.ContactText() == "" {
		t.Error("ContactText kosong")
	}
	t.Logf("ContactText:\n%s", e.ContactText())
}

func TestPhoneFallbackNoWaid(t *testing.T) {
	// TEL tanpa waid → fallback digit nilai; nomor lokal 0… → 62…
	e := &WahaEvent{}
	e.Payload.VCards = []string{
		"BEGIN:VCARD\nVERSION:3.0\nFN:Budi Santoso\nTEL;type=CELL:0812-3456-7890\nEND:VCARD",
	}
	cs := e.Contacts()
	if len(cs) != 1 || cs[0].Phone != "6281234567890" {
		t.Fatalf("fallback gagal: %+v", cs)
	}
	if cs[0].Name != "Budi Santoso" {
		t.Errorf("nama fallback: dapat %q", cs[0].Name)
	}
}

func TestNoVCard(t *testing.T) {
	e := &WahaEvent{}
	e.Payload.Body = "halo"
	if e.ContactText() != "" {
		t.Error("tanpa vCard harus kosong")
	}
}
