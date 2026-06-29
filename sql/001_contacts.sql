-- ============================================================
-- PA AI System — Fase 4: Security Layer
-- Tabel contacts = whitelist + profil kontak (ground truth di PostgreSQL).
-- DDL ini idempotent; juga dieksekusi otomatis oleh API Gateway saat boot
-- (lihat services/api-gateway/src/db/postgres.go -> Migrate()).
-- ============================================================

CREATE TABLE IF NOT EXISTS contacts (
    id          SERIAL PRIMARY KEY,
    phone       VARCHAR(32)  UNIQUE NOT NULL,           -- nomor internasional tanpa '+' (mis. 628970258733)
    lid         VARCHAR(32)  UNIQUE,                     -- WhatsApp LID (privacy id) bila kontak datang sebagai @lid
    name        VARCHAR(255),
    company     VARCHAR(255),
    email       VARCHAR(255),
    trust_level VARCHAR(32)  NOT NULL DEFAULT 'external',-- su | semi_trusted | external | external_unknown
    notes       TEXT,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Indeks bantu untuk lookup whitelist by lid (auth middleware).
CREATE INDEX IF NOT EXISTS idx_contacts_lid ON contacts (lid) WHERE lid IS NOT NULL;
