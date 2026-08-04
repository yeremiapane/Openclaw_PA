// Package db menyediakan koneksi PostgreSQL & Redis serta operasi whitelist.
package db

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pa-ai/api-gateway/src/config"
	"pa-ai/api-gateway/src/model"
)

// Store membungkus pool koneksi PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

// schemaDDL adalah cermin dari sql/001_contacts.sql + sql/002_external_and_logs.sql
const schemaDDL = `
CREATE TABLE IF NOT EXISTS contacts (
    id          SERIAL PRIMARY KEY,
    phone       VARCHAR(32)  UNIQUE NOT NULL,
    lid         VARCHAR(32)  UNIQUE,
    name        VARCHAR(255),
    company     VARCHAR(255),
    email       VARCHAR(255),
    trust_level VARCHAR(32)  NOT NULL DEFAULT 'external',
    notes       TEXT,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_contacts_lid ON contacts (lid) WHERE lid IS NOT NULL;

CREATE TABLE IF NOT EXISTS external_contacts (
    id            SERIAL PRIMARY KEY,
    identifier    VARCHAR(64)  UNIQUE NOT NULL,
    kind          VARCHAR(16)  NOT NULL,
    phone         VARCHAR(32),
    lid           VARCHAR(32),
    display_name  VARCHAR(255),
    message_count INTEGER      NOT NULL DEFAULT 0,
    status        VARCHAR(16)  NOT NULL DEFAULT 'pending',
    notes         TEXT,
    first_seen    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    last_seen     TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_external_status ON external_contacts (status);

CREATE TABLE IF NOT EXISTS access_logs (
    id           BIGSERIAL PRIMARY KEY,
    identifier   VARCHAR(64)  NOT NULL,
    kind         VARCHAR(16)  NOT NULL,
    phone        VARCHAR(32),
    contact_id   INTEGER REFERENCES contacts(id) ON DELETE SET NULL,
    decision     VARCHAR(24)  NOT NULL,
    reason       VARCHAR(64),
    body_preview VARCHAR(256),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_logs_identifier ON access_logs (identifier);
CREATE INDEX IF NOT EXISTS idx_logs_decision   ON access_logs (decision);
CREATE INDEX IF NOT EXISTS idx_logs_created     ON access_logs (created_at DESC);

-- ── profiling + soft-delete + jejak waktu (cermin sql/003_*.sql) ──
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS address    TEXT;
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS tags       TEXT[]      NOT NULL DEFAULT '{}';
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS profile    JSONB       NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS email      VARCHAR(255);
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS company    VARCHAR(255);
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS address    TEXT;
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS tags       TEXT[]      NOT NULL DEFAULT '{}';
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS profile    JSONB       NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS risk_score INTEGER     NOT NULL DEFAULT 0;
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

ALTER TABLE contacts DROP CONSTRAINT IF EXISTS contacts_phone_key;
ALTER TABLE contacts DROP CONSTRAINT IF EXISTS contacts_lid_key;
DROP INDEX IF EXISTS idx_contacts_lid;
CREATE UNIQUE INDEX IF NOT EXISTS uq_contacts_phone ON contacts (phone) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_contacts_lid   ON contacts (lid)   WHERE lid IS NOT NULL AND deleted_at IS NULL;

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_contacts_updated ON contacts;
CREATE TRIGGER trg_contacts_updated BEFORE UPDATE ON contacts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
DROP TRIGGER IF EXISTS trg_external_updated ON external_contacts;
CREATE TRIGGER trg_external_updated BEFORE UPDATE ON external_contacts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX IF NOT EXISTS idx_external_risk ON external_contacts (risk_score DESC);

-- ── memory layer (conversations + messages + contact_facts) ──
CREATE TABLE IF NOT EXISTS conversations (
    id          VARCHAR(96)  PRIMARY KEY,                       -- {agent}:{phone}
    contact_id  INTEGER      REFERENCES contacts(id) ON DELETE SET NULL,
    agent_id    VARCHAR(64)  NOT NULL,
    state       VARCHAR(48)  NOT NULL DEFAULT 'NEW_CONTACT',
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS trg_conversations_updated ON conversations;
CREATE TRIGGER trg_conversations_updated BEFORE UPDATE ON conversations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS messages (
    id              BIGSERIAL    PRIMARY KEY,
    conversation_id VARCHAR(96)  NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role            VARCHAR(16)  NOT NULL,                       -- 'user' | 'assistant'
    text            TEXT         NOT NULL,
    agent_id        VARCHAR(64),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_messages_conv         ON messages (conversation_id);
CREATE INDEX IF NOT EXISTS idx_messages_conv_created ON messages (conversation_id, created_at DESC);

CREATE TABLE IF NOT EXISTS contact_facts (
    id           BIGSERIAL    PRIMARY KEY,
    contact_id   INTEGER      NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    fact         TEXT         NOT NULL,
    source       VARCHAR(64),
    extracted_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (contact_id, fact)
);
CREATE INDEX IF NOT EXISTS idx_facts_contact ON contact_facts (contact_id);

-- ── approval gate (pesan keluar ditahan menunggu persetujuan SU) ──
CREATE TABLE IF NOT EXISTS approval_pending (
    id              BIGSERIAL    PRIMARY KEY,
    conversation_id VARCHAR(96)  NOT NULL,
    agent_id        VARCHAR(64)  NOT NULL,
    contact_id      INTEGER      REFERENCES contacts(id) ON DELETE SET NULL,
    target_chat     VARCHAR(64)  NOT NULL,                 -- chat tujuan pengiriman
    user_text       TEXT         NOT NULL DEFAULT '',      -- pesan pemicu (utk memori saat approve)
    response_text   TEXT         NOT NULL,                 -- pesan keluar yang ditahan
    approval_reason TEXT,
    new_facts       JSONB        NOT NULL DEFAULT '[]'::jsonb,
    status          VARCHAR(16)  NOT NULL DEFAULT 'pending', -- pending|approved|rejected
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    decided_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_approval_status ON approval_pending (status);
CREATE INDEX IF NOT EXISTS idx_approval_created ON approval_pending (created_at DESC);

-- ── Observability & evaluasi (token usage, trace, meeting lifecycle) ──

-- Satu baris per giliran agent (inject). Inti evaluasi: token, model, durasi,
-- actions, outcome — untuk trace & analisa jawaban AI.
CREATE TABLE IF NOT EXISTS agent_executions (
    id                  BIGSERIAL    PRIMARY KEY,
    run_id              VARCHAR(64),                 -- runId dari output CLI
    oc_session_id       VARCHAR(64),                 -- sessionId internal OpenClaw
    session_key         VARCHAR(96),                 -- session-key yang dikirim (== conversation_id)
    conversation_id     VARCHAR(96),
    contact_id          INTEGER      REFERENCES contacts(id) ON DELETE SET NULL,
    agent_id            VARCHAR(64)  NOT NULL,
    provider            VARCHAR(32),
    model               VARCHAR(64),
    input_text          TEXT,                        -- pesan yang diinject (termasuk preamble konteks)
    response_text       TEXT,                        -- balasan agent (kosong bila gagal/diam)
    requires_approval   BOOLEAN      NOT NULL DEFAULT false,
    actions             JSONB        NOT NULL DEFAULT '[]'::jsonb,
    new_facts           JSONB        NOT NULL DEFAULT '[]'::jsonb,
    finish_reason       VARCHAR(32),
    stop_reason         VARCHAR(32),
    refusal             BOOLEAN      NOT NULL DEFAULT false,
    input_tokens        INTEGER      NOT NULL DEFAULT 0,
    output_tokens       INTEGER      NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER      NOT NULL DEFAULT 0,
    cache_write_tokens  INTEGER      NOT NULL DEFAULT 0,
    total_tokens        INTEGER      NOT NULL DEFAULT 0,
    system_prompt_chars INTEGER,
    prompt_chars        INTEGER,
    duration_ms         INTEGER,
    fallback_used       BOOLEAN      NOT NULL DEFAULT false,
    runner              VARCHAR(32),
    outcome             VARCHAR(24)  NOT NULL DEFAULT 'ok', -- ok|no_reply|parse_error|error|empty
    error_text          TEXT,
    raw_meta            JSONB,                       -- executionTrace + ringkasan systemPromptReport
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_exec_conv    ON agent_executions (conversation_id);
CREATE INDEX IF NOT EXISTS idx_exec_contact ON agent_executions (contact_id);
CREATE INDEX IF NOT EXISTS idx_exec_agent   ON agent_executions (agent_id);
CREATE INDEX IF NOT EXISTS idx_exec_created ON agent_executions (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_exec_model   ON agent_executions (model);

-- Untuk trace chat agent
CREATE TABLE IF NOT EXISTS outbound_messages (
    id              BIGSERIAL    PRIMARY KEY,
    execution_id    BIGINT       REFERENCES agent_executions(id) ON DELETE SET NULL,
    conversation_id VARCHAR(96),
    contact_id      INTEGER      REFERENCES contacts(id) ON DELETE SET NULL,
    agent_id        VARCHAR(64),
    target_chat     VARCHAR(64)  NOT NULL,
    kind            VARCHAR(32)  NOT NULL,           -- agent_reply|approval_notify|orchestrator_notify|approval_decision|system
    text            TEXT         NOT NULL,
    status          VARCHAR(16)  NOT NULL DEFAULT 'sent', -- sent|held|failed
    approval_id     BIGINT       REFERENCES approval_pending(id) ON DELETE SET NULL,
    error_text      TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_out_conv    ON outbound_messages (conversation_id);
CREATE INDEX IF NOT EXISTS idx_out_contact ON outbound_messages (contact_id);
CREATE INDEX IF NOT EXISTS idx_out_target  ON outbound_messages (target_chat);
CREATE INDEX IF NOT EXISTS idx_out_kind    ON outbound_messages (kind);
CREATE INDEX IF NOT EXISTS idx_out_created ON outbound_messages (created_at DESC);

-- Request meeting + status terkini. Dibuat otomatis dari approval gate.
CREATE TABLE IF NOT EXISTS meeting_requests (
    id                BIGSERIAL    PRIMARY KEY,
    conversation_id   VARCHAR(96),
    contact_id        INTEGER      REFERENCES contacts(id) ON DELETE SET NULL,
    agent_id          VARCHAR(64)  NOT NULL,
    approval_id       BIGINT       REFERENCES approval_pending(id) ON DELETE SET NULL,
    requested_via     VARCHAR(16)  NOT NULL DEFAULT 'external', -- su|external
    external_name     VARCHAR(255),
    external_company  VARCHAR(255),
    topic             TEXT,
    meeting_type      VARCHAR(16),                  -- online|offline|NULL(belum diketahui)
    proposed_datetime TIMESTAMPTZ,
    venue             TEXT,
    status            VARCHAR(24)  NOT NULL DEFAULT 'pending', -- pending|approved|rejected|scheduled|cancelled|completed
    details           JSONB        NOT NULL DEFAULT '{}'::jsonb, -- approvalReason, newFacts, payload mentah
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    approved_at       TIMESTAMPTZ,
    rejected_at       TIMESTAMPTZ,
    scheduled_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_meeting_status   ON meeting_requests (status);
CREATE INDEX IF NOT EXISTS idx_meeting_conv     ON meeting_requests (conversation_id);
CREATE INDEX IF NOT EXISTS idx_meeting_contact  ON meeting_requests (contact_id);
CREATE INDEX IF NOT EXISTS idx_meeting_approval ON meeting_requests (approval_id);
CREATE INDEX IF NOT EXISTS idx_meeting_created  ON meeting_requests (created_at DESC);
DROP TRIGGER IF EXISTS trg_meeting_updated ON meeting_requests;
CREATE TRIGGER trg_meeting_updated BEFORE UPDATE ON meeting_requests
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Riwayat transisi status meeting (data historis lengkap).
CREATE TABLE IF NOT EXISTS meeting_status_history (
    id          BIGSERIAL    PRIMARY KEY,
    meeting_id  BIGINT       NOT NULL REFERENCES meeting_requests(id) ON DELETE CASCADE,
    from_status VARCHAR(24),
    to_status   VARCHAR(24)  NOT NULL,
    changed_by  VARCHAR(64),                         -- su|admin|system|<agent_id>
    reason      TEXT,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_msh_meeting ON meeting_status_history (meeting_id, created_at);

-- Tugas terjadwal (pengingat). Worker latar belakang memproses baris 'pending'
CREATE TABLE IF NOT EXISTS scheduled_tasks (
    id          BIGSERIAL    PRIMARY KEY,
    fire_at     TIMESTAMPTZ  NOT NULL,
    kind        VARCHAR(32)  NOT NULL,                  -- reminder|meeting_reminder
    note        TEXT         NOT NULL DEFAULT '',
    meeting_id  BIGINT       REFERENCES meeting_requests(id) ON DELETE CASCADE,
    status      VARCHAR(16)  NOT NULL DEFAULT 'pending', -- pending|fired|cancelled|error
    created_by  VARCHAR(64)  NOT NULL DEFAULT 'su',
    error_text  TEXT,
    recur_kind  VARCHAR(16)  NOT NULL DEFAULT 'none',   -- none|daily|weekly
    recur_time  VARCHAR(5),                             -- "HH:MM" WIB (berulang)
    recur_dow   SMALLINT,                               -- 0-6 (weekly)
    label       VARCHAR(80)  NOT NULL DEFAULT '',       -- rujukan singkat untuk SU
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    fired_at    TIMESTAMPTZ
);
-- Migrasi aditif untuk instalasi lama (kolom rekurensi ditambahkan setelah rilis awal).
ALTER TABLE scheduled_tasks ADD COLUMN IF NOT EXISTS recur_kind VARCHAR(16) NOT NULL DEFAULT 'none';
ALTER TABLE scheduled_tasks ADD COLUMN IF NOT EXISTS recur_time VARCHAR(5);
ALTER TABLE scheduled_tasks ADD COLUMN IF NOT EXISTS recur_dow  SMALLINT;
ALTER TABLE scheduled_tasks ADD COLUMN IF NOT EXISTS label      VARCHAR(80) NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_sched_due     ON scheduled_tasks (status, fire_at);
CREATE INDEX IF NOT EXISTS idx_sched_meeting ON scheduled_tasks (meeting_id);

-- agent_preferences: overlay GAYA & SEBAGIAN PERILAKU per-agent yang disetel Pak Sudianto.
CREATE TABLE IF NOT EXISTS agent_preferences (
    agent      VARCHAR(32)  PRIMARY KEY,
    prefs      TEXT         NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Pemantauan email (Email Watch): agent melapor proaktif untuk email yang cocok kriteria.
-- Worker cek inbox berkala
CREATE TABLE IF NOT EXISTS email_watches (
    id              BIGSERIAL    PRIMARY KEY,
    criteria        TEXT         NOT NULL,                  -- kriteria bahasa alami
    label           VARCHAR(80)  NOT NULL DEFAULT '',       -- rujukan singkat untuk SU
    from_filter     TEXT         NOT NULL DEFAULT '',       -- pra-saring: substring pengirim
    keyword_filter  TEXT         NOT NULL DEFAULT '',       -- pra-saring: kata kunci subjek/isi
    created_by      VARCHAR(64)  NOT NULL DEFAULT 'su',
    status          VARCHAR(16)  NOT NULL DEFAULT 'active', -- active|cancelled|expired
    last_seen_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),    -- hanya email setelah ini dinilai
    expires_at      TIMESTAMPTZ,                            -- opsional auto-kedaluwarsa
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    last_checked_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_email_watch_active ON email_watches (status, created_by);
`

// NewStore membuka pool ke PostgreSQL.
func NewStore(ctx context.Context, cfg config.Config) (*Store, error) {
	pool, err := pgxpool.New(ctx, cfg.DBConnString())
	if err != nil {
		return nil, err
	}
	// Verifikasi koneksi cepat.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Close menutup pool.
func (s *Store) Close() { s.pool.Close() }

// Migrate menjalankan DDL skema (idempotent).
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, schemaDDL)
	return err
}

// upsertContact menambah/memperbarui satu kontak whitelist berdasarkan phone.
func (s *Store) upsertContact(ctx context.Context, phone, lid, name, trust string) error {
	if phone == "" {
		return nil
	}
	var lidArg any
	if lid != "" {
		lidArg = lid
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO contacts (phone, lid, name, trust_level)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (phone) WHERE deleted_at IS NULL DO UPDATE
		   SET lid         = COALESCE(EXCLUDED.lid, contacts.lid),
		       name        = EXCLUDED.name,
		       trust_level = EXCLUDED.trust_level
	`, phone, lidArg, name, trust)
	return err
}

// SeedTrustedContacts menanam SU & Nova ke whitelist (idempotent).
func (s *Store) SeedTrustedContacts(ctx context.Context, cfg config.Config) error {
	if err := s.upsertContact(ctx, cfg.SUPhone, cfg.SULid, "Pak Sudianto (SU)", "su"); err != nil {
		return err
	}
	if err := s.upsertContact(ctx, cfg.NovaPhone, cfg.NovaLid, "Bu Nova", "semi_trusted"); err != nil {
		return err
	}
	if err := s.upsertContact(ctx, cfg.AdminPhone, cfg.AdminLid, "Admin", "admin"); err != nil {
		return err
	}
	log.Printf("[db] seed kontak trusted selesai (SU=%s lid=%s, Nova=%s lid=%s, Admin=%s lid=%s)",
		cfg.SUPhone, cfg.SULid, cfg.NovaPhone, cfg.NovaLid, cfg.AdminPhone, cfg.AdminLid)
	return nil
}

// ErrNotWhitelisted dikembalikan bila kontak tidak ada di whitelist.
var ErrNotWhitelisted = errors.New("kontak tidak ada di whitelist")

// EnsureContact ambil kontak whitelist via phone; jika belum ada, buat
// sebagai trust 'external' agar balasan (SPAWN_AGENT) tetap lolos security.
func (s *Store) EnsureContact(ctx context.Context, phone, name, company, email string) (*model.Contact, error) {
	c, err := s.FindContact(ctx, model.Identifier{Kind: "phone", Value: phone})
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, ErrNotWhitelisted) {
		return nil, err
	}
	c, err = s.AddContact(ctx, ContactInput{
		Phone: phone, Name: name, Company: company, Email: email,
		TrustLevel: "external",
		Notes:      "Dibuat otomatis dari inisiasi SU (SPAWN_AGENT)",
	})
	if err != nil {
		// Balapan: dua spawn paralel ke nomor sama bisa sama-sama lolos FindContact
		// lalu bertabrakan di unique constraint (uq_contacts_phone). Alih-alih gagal,
		// ambil ulang kontak yang sudah keburu dibuat goroutine lain.
		if existing, ferr := s.FindContact(ctx, model.Identifier{Kind: "phone", Value: phone}); ferr == nil {
			return existing, nil
		}
		return nil, err
	}
	return c, nil
}

// AutoWhitelistExternal mendaftarkan nomor tak dikenal sebagai kontak 'external'
// saat WHITELIST_MODE=open (semua penelepon dilayani pa_communicator). Idempotent
// & aman terhadap balapan (mirip EnsureContact): bila kontak sudah ada, dikembalikan
// apa adanya tanpa menimpa trust/profil. `lid` opsional (diisi bila datang via @lid).
func (s *Store) AutoWhitelistExternal(ctx context.Context, phone, lid string) (*model.Contact, error) {
	if c, err := s.FindContact(ctx, model.Identifier{Kind: "phone", Value: phone}); err == nil {
		return c, nil
	} else if !errors.Is(err, ErrNotWhitelisted) {
		return nil, err
	}
	c, err := s.AddContact(ctx, ContactInput{
		Phone: phone, Lid: lid, TrustLevel: "external",
		Notes: "Auto-whitelist (WHITELIST_MODE=open): penelepon publik dilayani pa_communicator",
	})
	if err != nil {
		// Balapan: dua pesan paralel dari nomor sama bisa sama-sama lolos FindContact
		// lalu bertabrakan di uq_contacts_phone. Ambil ulang yang sudah keburu dibuat.
		if existing, ferr := s.FindContact(ctx, model.Identifier{Kind: "phone", Value: phone}); ferr == nil {
			return existing, nil
		}
		return nil, err
	}
	return c, nil
}

// FindContactByName mencari kontak aktif berdasarkan nama (case-insensitive).
// Opsional filter by company. Digunakan untuk rekonsiliasi nomor pada SPAWN_AGENT.
func (s *Store) FindContactByName(ctx context.Context, name, company string) (*model.Contact, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrNotWhitelisted
	}
	var c model.Contact
	err := s.pool.QueryRow(ctx, `
		SELECT id, phone, COALESCE(lid,''), COALESCE(name,''),
		       COALESCE(company,''), COALESCE(email,''), trust_level
		FROM contacts
		WHERE deleted_at IS NULL
		  AND lower(name) = lower($1)
		  AND ($2 = '' OR lower(COALESCE(company,'')) = lower($2))
		ORDER BY id
		LIMIT 1`, name, strings.TrimSpace(company)).Scan(
		&c.ID, &c.Phone, &c.Lid, &c.Name, &c.Company, &c.Email, &c.TrustLevel)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotWhitelisted
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// FindContact mencari kontak berdasarkan identifier (phone atau lid).
// Mengembalikan ErrNotWhitelisted jika tidak ditemukan.
func (s *Store) FindContact(ctx context.Context, id model.Identifier) (*model.Contact, error) {
	var query string
	switch id.Kind {
	case "phone":
		query = `SELECT id, phone, COALESCE(lid,''), COALESCE(name,''),
		                COALESCE(company,''), COALESCE(email,''), trust_level
		         FROM contacts WHERE phone = $1 AND deleted_at IS NULL`
	case "lid":
		query = `SELECT id, phone, COALESCE(lid,''), COALESCE(name,''),
		                COALESCE(company,''), COALESCE(email,''), trust_level
		         FROM contacts WHERE lid = $1 AND deleted_at IS NULL`
	default:
		return nil, ErrNotWhitelisted // grup/broadcast tidak pernah di-whitelist
	}

	var c model.Contact
	err := s.pool.QueryRow(ctx, query, id.Value).Scan(
		&c.ID, &c.Phone, &c.Lid, &c.Name, &c.Company, &c.Email, &c.TrustLevel)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotWhitelisted
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// SetContactLid menyimpan LID WhatsApp ke kontak. Dipakai saat kontak
// membalas via @lid; tidak menimpa LID berbeda milik kontak lain.
func (s *Store) SetContactLid(ctx context.Context, contactID int, lid string) error {
	if lid == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE contacts SET lid = $2, updated_at = now()
		WHERE id = $1 AND (lid IS NULL OR lid <> $2)
		  AND NOT EXISTS (SELECT 1 FROM contacts WHERE lid = $2 AND id <> $1 AND deleted_at IS NULL)
	`, contactID, lid)
	return err
}
