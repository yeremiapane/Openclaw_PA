// Package memory merakit konteks sebelum disuntik ke OpenClaw (contextAssembler)
// dan menyimpan percakapan ke semua lapisan memori (memoryWriter).
// Lapisan: PostgreSQL = sumber kebenaran; Redis = buffer cepat (TTL 48j).
// OpenClaw diperlakukan stateless per-turn: konteks dirakit di sini dari PG.
package memory

import (
	"context"
	"fmt"
	"strings"

	"pa-ai/api-gateway/src/db"
	"pa-ai/api-gateway/src/model"
)

// fetchLimit = jumlah pesan terakhir yang diambil untuk konteks.
const fetchLimit = 20

// historyInPrompt = jumlah pesan terakhir yang disertakan di preamble inject.
const historyInPrompt = 12

// Service membungkus Store (PostgreSQL) + Cache (Redis).
type Service struct {
	store *db.Store
	cache *db.Cache
}

// New membuat memory service.
func New(store *db.Store, cache *db.Cache) *Service {
	return &Service{store: store, cache: cache}
}

// Context adalah hasil contextAssembler: snapshot yang dipakai untuk merakit
// pesan inject ke agent.
type Context struct {
	ConversationID string
	Contact        *model.Contact
	History        []model.Message
	Facts          []string
	State          string
	IsReturning    bool
	// LiveStatus = status terkini dari sumber kebenaran untuk preamble agent.
	// Kosong = tidak disisipkan.
	LiveStatus string
}

// Assemble merakit konteks untuk satu kontak/percakapan.
// Urutan: Redis (fast path) → fallback PostgreSQL (+ warm-up Redis) → profil + fakta.
func (s *Service) Assemble(ctx context.Context, convID string, contact *model.Contact) (*Context, error) {
	// Step 1: recent messages dari Redis.
	msgs, found, err := s.cache.GetMessages(ctx, convID)
	if err != nil {
		return nil, fmt.Errorf("cache get messages: %w", err)
	}

	// Step 2: fallback ke PostgreSQL bila cache miss, lalu warm-up Redis.
	if !found {
		msgs, err = s.store.RecentMessages(ctx, convID, fetchLimit)
		if err != nil {
			return nil, fmt.Errorf("pg recent messages: %w", err)
		}
		if len(msgs) > 0 {
			if err := s.cache.SetMessages(ctx, convID, msgs); err != nil {
				return nil, fmt.Errorf("cache warm messages: %w", err)
			}
		}
	}

	// Step 3: state (Redis → PostgreSQL → default NEW_CONTACT).
	state, sFound, err := s.cache.GetState(ctx, convID)
	if err != nil {
		return nil, fmt.Errorf("cache get state: %w", err)
	}
	if !sFound {
		state, err = s.store.GetConversationState(ctx, convID)
		if err != nil {
			return nil, fmt.Errorf("pg get state: %w", err)
		}
		if err := s.cache.SetState(ctx, convID, state); err != nil {
			return nil, fmt.Errorf("cache warm state: %w", err)
		}
	}

	// Step 4: fakta kontak (untuk grounding profil).
	var facts []string
	if contact != nil {
		facts, err = s.store.ListFacts(ctx, contact.ID, 30)
		if err != nil {
			return nil, fmt.Errorf("pg list facts: %w", err)
		}
	}

	return &Context{
		ConversationID: convID,
		Contact:        contact,
		History:        msgs,
		Facts:          facts,
		State:          state,
		IsReturning:    len(msgs) > 0,
	}, nil
}

// OCSessionKey mengembalikan session-key OpenClaw dengan suffix epoch jika sesi di-reset.
func (s *Service) OCSessionKey(ctx context.Context, convID string) string {
	if s.cache == nil {
		return convID
	}
	if e := s.cache.GetEpoch(ctx, convID); e > 0 {
		return fmt.Sprintf("%s#%d", convID, e)
	}
	return convID
}

// ResetOCSession menaikkan epoch (membuang sesi OpenClaw yang terkontaminasi) dan
// mengembalikan session-key baru yang bersih.
func (s *Service) ResetOCSession(ctx context.Context, convID string) (string, error) {
	if s.cache == nil {
		return convID, nil
	}
	e, err := s.cache.BumpEpoch(ctx, convID)
	if err != nil {
		return convID, err
	}
	return fmt.Sprintf("%s#%d", convID, e), nil
}

// BuildInjectMessage menyusun pesan ke agent. Untuk kontak baru tanpa riwayat
// dan fakta, pesan dikirim apa adanya. Jika ada riwayat/fakta, sisipkan
// preamble konteks agar agent punya grounding dan tahan restart OpenClaw.
func (c *Context) BuildInjectMessage(incoming string) string {
	if !c.IsReturning && len(c.Facts) == 0 && c.LiveStatus == "" {
		return incoming
	}

	var b strings.Builder
	b.WriteString("[KONTEKS PERCAKAPAN — referensi internal untukmu, JANGAN dibalas ulang]\n")

	if c.Contact != nil {
		name := c.Contact.Name
		if name == "" {
			name = "(belum diketahui)"
		}
		b.WriteString("Kontak: " + name)
		if c.Contact.Company != "" {
			b.WriteString(" — " + c.Contact.Company)
		}
		b.WriteString(fmt.Sprintf(" | trust: %s\n", c.Contact.TrustLevel))
		// Profil tersimpan: agar agent tak menanyakan ulang data yang sudah ada.
		if c.Contact.Email != "" {
			b.WriteString("Email kontak: " + c.Contact.Email + "\n")
		}
		if c.Contact.Address != "" {
			b.WriteString("Alamat kontak: " + c.Contact.Address + "\n")
		}
	}
	b.WriteString(fmt.Sprintf("Status percakapan: %s | Kontak kembali: %s\n",
		c.State, yesNo(c.IsReturning)))

	if len(c.Facts) > 0 {
		b.WriteString("Fakta yang sudah diketahui:\n")
		for _, f := range c.Facts {
			b.WriteString("- " + f + "\n")
		}
	}

	if len(c.History) > 0 {
		b.WriteString("Riwayat percakapan terakhir:\n")
		hist := c.History
		if len(hist) > historyInPrompt {
			hist = hist[len(hist)-historyInPrompt:]
		}
		for _, m := range hist {
			b.WriteString("[" + speaker(m.Role) + "] " + m.Text + "\n")
		}
	}

	if c.LiveStatus != "" {
		b.WriteString("\n" + c.LiveStatus + "\n")
	}

	b.WriteString("\n[PESAN MASUK TERBARU — balas hanya bagian ini]\n")
	b.WriteString(incoming)
	return b.String()
}

// Write menyimpan hasil satu turn ke semua lapisan memori (memoryWriter).
//   1) pastikan conversation ada
//   2) simpan pesan user + assistant ke PostgreSQL
//   3) update buffer Redis (sliding window + refresh TTL)
//   4) simpan newFacts ke contact_facts
func (s *Service) Write(ctx context.Context, convID string, contact *model.Contact, agentID, userText, assistantText string, newFacts []string) error {
	contactID := 0
	if contact != nil {
		contactID = contact.ID
	}

	if err := s.store.EnsureConversation(ctx, convID, contactID, agentID); err != nil {
		return fmt.Errorf("ensure conversation: %w", err)
	}
	if err := s.store.SaveMessage(ctx, convID, "user", userText, ""); err != nil {
		return fmt.Errorf("save user message: %w", err)
	}
	if err := s.store.SaveMessage(ctx, convID, "assistant", assistantText, agentID); err != nil {
		return fmt.Errorf("save assistant message: %w", err)
	}

	// Buffer Redis: append kedua pesan, pertahankan sliding window, refresh TTL.
	if err := s.cache.AppendMessages(ctx, convID,
		model.Message{Role: "user", Text: userText},
		model.Message{Role: "assistant", Text: assistantText, AgentID: agentID},
	); err != nil {
		return fmt.Errorf("cache append: %w", err)
	}

	// Fakta baru → contact_facts (deduplikasi via ON CONFLICT).
	for _, f := range newFacts {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if err := s.store.SaveFact(ctx, contactID, f, agentID); err != nil {
			return fmt.Errorf("save fact: %w", err)
		}
	}
	return nil
}

// SetState perbarui state percakapan di PostgreSQL + Redis.
// Update PostgreSQL bersifat best-effort (no-op jika row conversation belum ada);
// Redis selalu di-set agar state konsisten untuk assemble berikutnya.
func (s *Service) SetState(ctx context.Context, convID, state string) error {
	if err := s.store.UpdateConversationState(ctx, convID, state); err != nil {
		return err
	}
	return s.cache.SetState(ctx, convID, state)
}

func yesNo(b bool) string {
	if b {
		return "ya"
	}
	return "tidak"
}

func speaker(role string) string {
	if role == "assistant" {
		return "kamu"
	}
	return "kontak"
}
