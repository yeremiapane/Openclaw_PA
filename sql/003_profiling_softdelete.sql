-- ============================================================
-- PA AI System — Fase 4.7: Profiling + Soft-Delete + Jejak waktu
-- Memperkaya contacts & external_contacts agar AI bisa MENGENALI &
-- MEM-PROFILE kontak (terutama external) saat menghubungi kembali,
-- serta menyimpan history (created/updated/deleted) untuk mitigasi.
--
-- Idempotent (ADD COLUMN IF NOT EXISTS / CREATE ... IF NOT EXISTS);
-- juga dieksekusi otomatis oleh API Gateway saat boot
-- (lihat services/api-gateway/src/db/postgres.go -> Migrate()).
-- ============================================================

-- ── Kolom profiling untuk whitelist contacts ──
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS address    TEXT;
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS tags       TEXT[]      NOT NULL DEFAULT '{}';
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS profile    JSONB       NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;          -- NULL = aktif (soft-delete)

-- ── Kolom profiling untuk external_contacts ──
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS email      VARCHAR(255);
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS company    VARCHAR(255);
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS address    TEXT;
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS tags       TEXT[]      NOT NULL DEFAULT '{}';
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS profile    JSONB       NOT NULL DEFAULT '{}'::jsonb; -- fakta utk AI profiling
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS risk_score INTEGER     NOT NULL DEFAULT 0;            -- prioritas mitigasi
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE external_contacts ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

-- ── Soft-delete aman: UNIQUE biasa → partial unique (boleh re-add nomor yg sudah dihapus) ──
ALTER TABLE contacts DROP CONSTRAINT IF EXISTS contacts_phone_key;
ALTER TABLE contacts DROP CONSTRAINT IF EXISTS contacts_lid_key;
DROP INDEX IF EXISTS idx_contacts_lid;
CREATE UNIQUE INDEX IF NOT EXISTS uq_contacts_phone ON contacts (phone) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_contacts_lid   ON contacts (lid)   WHERE lid IS NOT NULL AND deleted_at IS NULL;

-- ── Auto-maintain updated_at ──
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
