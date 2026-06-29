package openclaw

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestLiveInject memanggil CLI `openclaw agent` sungguhan lewat client.Inject.
// Hanya jalan bila OPENCLAW_LIVE=1 (butuh gateway openclaw aktif di :18789).
// TIDAK menyentuh WAHA — jadi tak ada pesan WhatsApp nyata terkirim.
func TestLiveInject(t *testing.T) {
	if os.Getenv("OPENCLAW_LIVE") != "1" {
		t.Skip("set OPENCLAW_LIVE=1 untuk menjalankan integration test live")
	}

	c := New("openclaw", "node", "", "pa_communicator", 180*time.Second)

	// Session key unik per-run agar tidak kena dedup NO_REPLY dari run sebelumnya.
	sessionKey := fmt.Sprintf("agent:pa_communicator:fase6smoke-%d", time.Now().Unix())
	msg := "Halo, saya Dimas dari PT Coba Integrasi. Saya ingin menjadwalkan pertemuan dengan Pak Sudianto pekan depan."

	reply, meta, err := c.Inject(context.Background(), sessionKey, msg)
	if err != nil {
		t.Fatalf("Inject gagal: %v", err)
	}

	t.Logf("response        : %s", reply.Response)
	t.Logf("newFacts        : %v", reply.NewFacts)
	t.Logf("requiresApproval: %v", reply.RequiresApproval)
	t.Logf("approvalReason  : %s", reply.ApprovalReason)
	t.Logf("model           : %s tokens(in=%d out=%d cw=%d) dur=%dms",
		meta.Model, meta.Usage.Input, meta.Usage.Output, meta.Usage.CacheWrite, meta.DurationMs)

	if reply.Response == "" {
		t.Fatalf("response kosong")
	}
	// Turn pertama: agent mengumpulkan info dulu (sesuai SOUL.md), jadi
	// requiresApproval=false itu benar. Yang kita verifikasi: pipeline Go->CLI->parse
	// menghasilkan balasan valid dan menangkap fakta kontak.
	if len(reply.NewFacts) == 0 {
		t.Errorf("EXPECT newFacts terisi (nama/perusahaan), dapat kosong")
	}
}
