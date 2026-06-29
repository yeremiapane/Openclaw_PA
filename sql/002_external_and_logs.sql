-- ============================================================
-- PA AI System — Pemisahan kontak external + audit log
-- Dieksekusi idempotent juga oleh API Gateway saat boot
-- (lihat services/api-gateway/src/db/postgres.go -> Migrate()).
-- ============================================================

-- external_contacts = nomor di LUAR whitelist yang pernah menghubungi bot.
-- Dipisah dari `contacts` agar mitigasi (blokir/promote) lebih mudah & terisolasi.
CREATE TABLE IF NOT EXISTS external_contacts (
    id            SERIAL PRIMARY KEY,
    identifier    VARCHAR(64)  UNIQUE NOT NULL,          -- raw "from" WAHA, mis. 3939206447269@lid
    kind          VARCHAR(16)  NOT NULL,                 -- phone | lid | other
    phone         VARCHAR(32),                           -- diisi jika datang sbg @c.us
    lid           VARCHAR(32),                           -- diisi jika datang sbg @lid
    display_name  VARCHAR(255),
    message_count INTEGER      NOT NULL DEFAULT 0,
    status        VARCHAR(16)  NOT NULL DEFAULT 'pending',-- pending | blocked | promoted
    notes         TEXT,
    first_seen    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    last_seen     TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_external_status ON external_contacts (status);

-- access_logs = jejak audit setiap keputusan security layer (history & profil).
CREATE TABLE IF NOT EXISTS access_logs (
    id           BIGSERIAL PRIMARY KEY,
    identifier   VARCHAR(64)  NOT NULL,                  -- raw "from"
    kind         VARCHAR(16)  NOT NULL,                  -- phone | lid | other
    phone        VARCHAR(32),                            -- nomor kanonik bila kontak dikenal
    contact_id   INTEGER REFERENCES contacts(id) ON DELETE SET NULL,
    decision     VARCHAR(24)  NOT NULL,                  -- allowed|blocked|rate_limited|injection_blocked|ignored
    reason       VARCHAR(64),
    body_preview VARCHAR(256),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_logs_identifier ON access_logs (identifier);
CREATE INDEX IF NOT EXISTS idx_logs_decision   ON access_logs (decision);
CREATE INDEX IF NOT EXISTS idx_logs_created     ON access_logs (created_at DESC);
