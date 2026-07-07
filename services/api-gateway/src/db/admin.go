package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"pa-ai/api-gateway/src/model"
)

// nullStr mengembalikan nil untuk string kosong agar tersimpan sbg SQL NULL.
func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// jsonbArg mengembalikan nil untuk JSON kosong; dikirim sebagai string agar `::jsonb` berfungsi.
func jsonbArg(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return string(r)
}

// tagsArg mengembalikan nil bila tags tidak disertakan (nil), sehingga nilai lama dipertahankan.
func tagsArg(t []string) any {
	if t == nil {
		return nil
	}
	return t
}

// Daftar kolom (+ COALESCE) yang dipakai konsisten saat membaca contacts/external.
const contactCols = `id, phone, COALESCE(lid,''), COALESCE(name,''),
	COALESCE(company,''), COALESCE(email,''), COALESCE(address,''), trust_level,
	COALESCE(tags,'{}'), COALESCE(profile,'{}'), COALESCE(notes,''),
	created_at, updated_at, deleted_at`

const externalCols = `id, identifier, kind, COALESCE(phone,''), COALESCE(lid,''),
	COALESCE(display_name,''), COALESCE(email,''), COALESCE(company,''),
	COALESCE(address,''), message_count, status, risk_score,
	COALESCE(tags,'{}'), COALESCE(profile,'{}'), COALESCE(notes,''),
	first_seen, last_seen, created_at, updated_at, deleted_at`

// scanContact memetakan satu baris (urutan contactCols) ke model.Contact.
func scanContact(row pgx.Row) (*model.Contact, error) {
	var c model.Contact
	err := row.Scan(&c.ID, &c.Phone, &c.Lid, &c.Name, &c.Company, &c.Email,
		&c.Address, &c.TrustLevel, &c.Tags, &c.Profile, &c.Notes,
		&c.CreatedAt, &c.UpdatedAt, &c.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// scanExternal memetakan satu baris (urutan externalCols) ke model.ExternalContact.
func scanExternal(row pgx.Row) (*model.ExternalContact, error) {
	var ec model.ExternalContact
	err := row.Scan(&ec.ID, &ec.Identifier, &ec.Kind, &ec.Phone, &ec.Lid,
		&ec.DisplayName, &ec.Email, &ec.Company, &ec.Address, &ec.MessageCount,
		&ec.Status, &ec.RiskScore, &ec.Tags, &ec.Profile, &ec.Notes,
		&ec.FirstSeen, &ec.LastSeen, &ec.CreatedAt, &ec.UpdatedAt, &ec.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &ec, nil
}

// ─── Access logs (audit) ──────────────────────────────────────────

// RecordAccess menyimpan satu keputusan security layer ke access_logs.
func (s *Store) RecordAccess(ctx context.Context, e model.AccessLog) error {
	preview := e.BodyPreview
	if len([]rune(preview)) > 256 {
		preview = string([]rune(preview)[:256])
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO access_logs (identifier, kind, phone, contact_id, decision, reason, body_preview)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		e.Identifier, e.Kind, nullStr(e.Phone), e.ContactID, e.Decision,
		nullStr(e.Reason), nullStr(preview))
	return err
}

// ListAccessLogs mengembalikan log terbaru (opsional filter decision & identifier).
func (s *Store) ListAccessLogs(ctx context.Context, decision, identifier string, limit int) ([]model.AccessLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	conds := []string{}
	args := []any{}
	if decision != "" {
		args = append(args, decision)
		conds = append(conds, fmt.Sprintf("decision = $%d", len(args)))
	}
	if identifier != "" {
		args = append(args, identifier)
		conds = append(conds, fmt.Sprintf("identifier = $%d", len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT id, identifier, kind, COALESCE(phone,''), contact_id, decision,
		       COALESCE(reason,''), COALESCE(body_preview,''), created_at
		FROM access_logs %s ORDER BY created_at DESC LIMIT $%d`, where, len(args))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AccessLog
	for rows.Next() {
		var l model.AccessLog
		if err := rows.Scan(&l.ID, &l.Identifier, &l.Kind, &l.Phone, &l.ContactID,
			&l.Decision, &l.Reason, &l.BodyPreview, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ─── External contacts ────────────────────────────────────────────

// UpsertExternalContact mencatat / memperbarui nomor di luar whitelist yang
// menghubungi bot. Mengembalikan baris terkini (termasuk status & profil).
func (s *Store) UpsertExternalContact(ctx context.Context, id model.Identifier) (*model.ExternalContact, error) {
	var phone, lid any
	switch id.Kind {
	case "phone":
		phone = id.Value
	case "lid":
		lid = id.Value
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO external_contacts (identifier, kind, phone, lid, message_count, last_seen)
		VALUES ($1,$2,$3,$4,1, now())
		ON CONFLICT (identifier) DO UPDATE
		   SET message_count = external_contacts.message_count + 1,
		       last_seen     = now(),
		       deleted_at    = NULL
		RETURNING `+externalCols,
		id.Raw, id.Kind, phone, lid)
	return scanExternal(row)
}

// ListExternalContacts mengembalikan kontak external (opsional filter status).
// Baris yang sudah soft-delete (deleted_at) disembunyikan.
func (s *Store) ListExternalContacts(ctx context.Context, status string, limit int) ([]model.ExternalContact, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT ` + externalCols + ` FROM external_contacts WHERE deleted_at IS NULL`
	args := []any{}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(" AND status = $%d", len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY last_seen DESC LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ExternalContact
	for rows.Next() {
		ec, err := scanExternal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ec)
	}
	return out, rows.Err()
}

// GetExternalContact mengambil satu kontak external berdasarkan identifier.
func (s *Store) GetExternalContact(ctx context.Context, identifier string) (*model.ExternalContact, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+externalCols+` FROM external_contacts WHERE identifier=$1`, identifier)
	ec, err := scanExternal(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ec, err
}

// ErrNotFound dikembalikan bila baris yang dituju tidak ada.
var ErrNotFound = errors.New("data tidak ditemukan")

// ExternalInput = payload admin untuk memperkaya/profiling kontak external.
type ExternalInput struct {
	DisplayName string          `json:"display_name"`
	Phone       string          `json:"phone"`
	Email       string          `json:"email"`
	Company     string          `json:"company"`
	Address     string          `json:"address"`
	Status      string          `json:"status"`
	RiskScore   *int            `json:"risk_score"`
	Tags        []string        `json:"tags"`
	Profile     json.RawMessage `json:"profile"`
	Notes       string          `json:"notes"`
}

// UpdateExternalContact memperkaya profil kontak external
func (s *Store) UpdateExternalContact(ctx context.Context, identifier string, in ExternalInput) (*model.ExternalContact, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE external_contacts SET
		   display_name = COALESCE($2, display_name),
		   phone        = COALESCE($3, phone),
		   email        = COALESCE($4, email),
		   company      = COALESCE($5, company),
		   address      = COALESCE($6, address),
		   status       = COALESCE($7, status),
		   risk_score   = COALESCE($8, risk_score),
		   tags         = COALESCE($9, tags),
		   profile      = profile || COALESCE($10,'{}')::jsonb,
		   notes        = COALESCE($11, notes)
		WHERE identifier = $1
		RETURNING `+externalCols,
		identifier, nullStr(in.DisplayName), nullStr(in.Phone), nullStr(in.Email),
		nullStr(in.Company), nullStr(in.Address), nullStr(in.Status), in.RiskScore,
		tagsArg(in.Tags), jsonbArg(in.Profile), nullStr(in.Notes))
	ec, err := scanExternal(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ec, err
}

// SetExternalStatus mengubah status kontak external (mis. 'blocked').
func (s *Store) SetExternalStatus(ctx context.Context, identifier, status, notes string) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE external_contacts SET status=$2, notes=COALESCE($3, notes) WHERE identifier=$1`,
		identifier, status, nullStr(notes))
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// lidByPhone mengambil LID kontak berdasarkan nomor, termasuk yang sudah
// soft-delete (agar block/unblock tetap menemukan bentuk identifier @lid).
// Mengembalikan "" bila kontak tak ada / tak punya lid.
func (s *Store) lidByPhone(ctx context.Context, phone string) string {
	var lid string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(lid,'') FROM contacts
		WHERE phone=$1 ORDER BY deleted_at NULLS FIRST LIMIT 1`, phone).Scan(&lid)
	if err != nil {
		return ""
	}
	return lid
}

// blockExternalIdentifier menandai satu identifier 'blocked' di external_contacts,
// membuat barisnya bila belum ada (upsert). Dipakai block cepat mode open.
func (s *Store) blockExternalIdentifier(ctx context.Context, identifier, kind, phone, lid, notes string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO external_contacts (identifier, kind, phone, lid, status, notes, message_count, last_seen)
		VALUES ($1,$2,$3,$4,'blocked',$5,0, now())
		ON CONFLICT (identifier) DO UPDATE
		   SET status     = 'blocked',
		       notes      = COALESCE($5, external_contacts.notes),
		       deleted_at = NULL`,
		identifier, kind, nullStr(phone), nullStr(lid), nullStr(notes))
	return err
}

// BlockContactByPhone memblokir sebuah nomor secara menyeluruh untuk mode open:
//  1. soft-delete kontak whitelist (agar tak lagi dilayani auto-whitelist), dan
//  2. tandai 'blocked' di external_contacts untuk SEMUA bentuk identifier yang
//     dikenal (<phone>@c.us, dan <lid>@lid bila kontak punya lid) sehingga
//     pesan berikutnya tetap tertolak apa pun jalur masuknya.
//
// Idempotent & aman dipanggil untuk nomor yang belum pernah di-whitelist
// (mem-blokir preemptif). Mengembalikan daftar identifier yang diblokir.
func (s *Store) BlockContactByPhone(ctx context.Context, phone, notes string) ([]string, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return nil, errors.New("phone kosong")
	}
	lid := s.lidByPhone(ctx, phone)

	// Cabut dari whitelist (best-effort; abaikan bila memang belum ada).
	if _, err := s.pool.Exec(ctx,
		`UPDATE contacts SET deleted_at=now() WHERE phone=$1 AND deleted_at IS NULL`, phone); err != nil {
		return nil, err
	}

	blocked := []string{}
	cusID := phone + "@c.us"
	if err := s.blockExternalIdentifier(ctx, cusID, "phone", phone, lid, notes); err != nil {
		return nil, err
	}
	blocked = append(blocked, cusID)
	if lid != "" {
		lidID := lid + "@lid"
		if err := s.blockExternalIdentifier(ctx, lidID, "lid", phone, lid, notes); err != nil {
			return nil, err
		}
		blocked = append(blocked, lidID)
	}
	return blocked, nil
}

// UnblockContactByPhone membalik BlockContactByPhone: hapus status 'blocked'
// (kembalikan ke 'pending') pada external_contacts untuk <phone>@c.us dan
// <lid>@lid. TIDAK otomatis mem-whitelist ulang; di mode open pesan berikutnya
// dari nomor itu akan auto-whitelist seperti biasa. Mengembalikan identifier
// yang berhasil di-unblock.
func (s *Store) UnblockContactByPhone(ctx context.Context, phone string) ([]string, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return nil, errors.New("phone kosong")
	}
	lid := s.lidByPhone(ctx, phone)

	ids := []string{phone + "@c.us"}
	if lid != "" {
		ids = append(ids, lid+"@lid")
	}
	unblocked := []string{}
	for _, id := range ids {
		ct, err := s.pool.Exec(ctx,
			`UPDATE external_contacts SET status='pending' WHERE identifier=$1 AND status='blocked'`, id)
		if err != nil {
			return nil, err
		}
		if ct.RowsAffected() > 0 {
			unblocked = append(unblocked, id)
		}
	}
	return unblocked, nil
}

// SoftDeleteExternalContact melakukan SOFT-DELETE kontak external (set deleted_at)
func (s *Store) SoftDeleteExternalContact(ctx context.Context, identifier string) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE external_contacts SET deleted_at=now() WHERE identifier=$1 AND deleted_at IS NULL`, identifier)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// IsExternalBlocked mengecek apakah identifier ada di external_contacts berstatus 'blocked'.
func (s *Store) IsExternalBlocked(ctx context.Context, identifier string) (bool, error) {
	var blocked bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM external_contacts WHERE identifier=$1 AND status='blocked')`,
		identifier).Scan(&blocked)
	return blocked, err
}

// ─── Contacts (whitelist) CRUD — admin ───────────

// ContactInput adalah payload untuk menambah/memperbarui kontak whitelist.
type ContactInput struct {
	Phone      string          `json:"phone"`
	Lid        string          `json:"lid"`
	Name       string          `json:"name"`
	Company    string          `json:"company"`
	Email      string          `json:"email"`
	Address    string          `json:"address"`
	TrustLevel string          `json:"trust_level"`
	Tags       []string        `json:"tags"`
	Profile    json.RawMessage `json:"profile"`
	Notes      string          `json:"notes"`
}

// AddContact memasukkan kontak baru ke whitelist (gagal jika phone aktif sudah ada).
func (s *Store) AddContact(ctx context.Context, in ContactInput) (*model.Contact, error) {
	trust := in.TrustLevel
	if trust == "" {
		trust = "external"
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO contacts (phone, lid, name, company, email, address, trust_level, tags, profile, notes)
		VALUES ($1,$2,$3,$4,$5,$6,$7, COALESCE($8,'{}'::text[]), COALESCE($9,'{}')::jsonb, $10)
		RETURNING `+contactCols,
		in.Phone, nullStr(in.Lid), nullStr(in.Name), nullStr(in.Company),
		nullStr(in.Email), nullStr(in.Address), trust, tagsArg(in.Tags),
		jsonbArg(in.Profile), nullStr(in.Notes))
	return scanContact(row)
}

// UpdateContact memperbarui profil/trust kontak aktif berdasarkan phone
func (s *Store) UpdateContact(ctx context.Context, phone string, in ContactInput) (*model.Contact, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE contacts SET
		   lid         = COALESCE($2, lid),
		   name        = COALESCE($3, name),
		   company     = COALESCE($4, company),
		   email       = COALESCE($5, email),
		   address     = COALESCE($6, address),
		   trust_level = COALESCE($7, trust_level),
		   tags        = COALESCE($8, tags),
		   profile     = profile || COALESCE($9,'{}')::jsonb,
		   notes       = COALESCE($10, notes)
		WHERE phone = $1 AND deleted_at IS NULL
		RETURNING `+contactCols,
		phone, nullStr(in.Lid), nullStr(in.Name), nullStr(in.Company),
		nullStr(in.Email), nullStr(in.Address), nullStr(in.TrustLevel),
		tagsArg(in.Tags), jsonbArg(in.Profile), nullStr(in.Notes))
	c, err := scanContact(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// DeleteContact melakukan SOFT-DELETE kontak (set deleted_at) agar history tetap ada.
func (s *Store) DeleteContact(ctx context.Context, phone string) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE contacts SET deleted_at=now() WHERE phone=$1 AND deleted_at IS NULL`, phone)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListContacts mengembalikan kontak whitelist (default hanya yang aktif).
func (s *Store) ListContacts(ctx context.Context, includeDeleted bool) ([]model.Contact, error) {
	q := `SELECT ` + contactCols + ` FROM contacts`
	if !includeDeleted {
		q += ` WHERE deleted_at IS NULL`
	}
	q += ` ORDER BY id`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Contact
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// PromoteExternal memindahkan kontak external ke whitelist `contacts` - Transksaksional
func (s *Store) PromoteExternal(ctx context.Context, identifier string, in ContactInput) (*model.Contact, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Ambil data external sebagai default bila input admin kosong.
	var exPhone, exLid, exName, exEmail, exCompany, exAddress string
	var exTags []string
	var exProfile json.RawMessage
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(phone,''), COALESCE(lid,''), COALESCE(display_name,''),
		       COALESCE(email,''), COALESCE(company,''), COALESCE(address,''),
		       COALESCE(tags,'{}'), COALESCE(profile,'{}')
		FROM external_contacts WHERE identifier=$1`, identifier).Scan(
		&exPhone, &exLid, &exName, &exEmail, &exCompany, &exAddress, &exTags, &exProfile)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	pick := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	phone := pick(in.Phone, exPhone)
	lid := pick(in.Lid, exLid)
	name := pick(in.Name, exName)
	company := pick(in.Company, exCompany)
	email := pick(in.Email, exEmail)
	address := pick(in.Address, exAddress)
	if phone == "" {
		return nil, fmt.Errorf("promote butuh phone (external ini hanya punya lid=%s) — sertakan phone di body", exLid)
	}
	tags := in.Tags
	if tags == nil {
		tags = exTags
	}
	profile := in.Profile
	if len(profile) == 0 {
		profile = exProfile
	}
	trust := in.TrustLevel
	if trust == "" {
		trust = "external"
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO contacts (phone, lid, name, company, email, address, trust_level, tags, profile, notes)
		VALUES ($1,$2,$3,$4,$5,$6,$7, COALESCE($8,'{}'::text[]), COALESCE($9,'{}')::jsonb, $10)
		ON CONFLICT (phone) WHERE deleted_at IS NULL DO UPDATE
		   SET lid=COALESCE(EXCLUDED.lid, contacts.lid),
		       name=EXCLUDED.name, trust_level=EXCLUDED.trust_level,
		       company=COALESCE(EXCLUDED.company, contacts.company),
		       email=COALESCE(EXCLUDED.email, contacts.email),
		       address=COALESCE(EXCLUDED.address, contacts.address),
		       profile=contacts.profile || EXCLUDED.profile
		RETURNING `+contactCols,
		phone, nullStr(lid), nullStr(name), nullStr(company), nullStr(email),
		nullStr(address), trust, tagsArg(tags), jsonbArg(profile), nullStr(in.Notes))
	c, err := scanContact(row)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE external_contacts SET status='promoted' WHERE identifier=$1`, identifier); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}
