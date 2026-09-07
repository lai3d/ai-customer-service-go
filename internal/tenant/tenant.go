// Package tenant is who a request belongs to.
//
// Everything this service stores about a customer belongs to somebody: a conversation, the
// turns under it, the tickets they produced, the corpus they were answered from. Until now
// that somebody was implicit — one FAQ, one budget, one set of operator tokens — and the
// implicit answer is the one that turns into a query nobody filtered.
//
// The design is the Java implementation's ADR 002, adopted rather than reinvented. That is
// the same decision as the corpus versioning: these two repositories are worth having
// because they are comparable, and one of them growing tenants while the other did not
// would cost more than the work.
//
// # What this package is and is not
//
// It resolves an API key to a tenant, and it issues and revokes those keys. It does not
// enforce isolation: enforcement is a predicate every query carries, in the package that
// owns the query, and a test per module that writes as one tenant and reads as another.
// A package that promised isolation from over here would be a promise nothing could keep.
package tenant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Default is the tenant every row written before tenancy belongs to, and the one a
// single-tenant deployment keeps using. It is a real tenant rather than a null: "no
// tenant" as a value is exactly the state a forgotten predicate produces.
const Default = "default"

// KeyPrefix marks a string as one of this service's API keys, so a key pasted into a log,
// a ticket or a repository is greppable — by its owner, and by the scanners that look for
// exactly this shape. It is the reason the prefix is fixed rather than configurable.
const KeyPrefix = "csk"

var (
	// ErrNoSuchKey is every failure a caller is allowed to tell apart from every other:
	// a key that does not parse, a key_id that does not exist, a secret that does not
	// match, a revoked key and a disabled tenant are all this. The caller answers 401 and
	// the log line says which — a client learning *why* its key failed is a client
	// learning which key_ids exist.
	ErrNoSuchKey = errors.New("no such API key")
	// ErrNoSuchTenant is a tenant id that is not in the table. It is separate from
	// ErrNoSuchKey because it is never reached from a request: only an operator naming a
	// tenant can produce it.
	ErrNoSuchTenant = errors.New("no such tenant")
	ErrExists       = errors.New("that tenant id is already taken")
)

// Tenant is the row. Disabled is a state rather than a deletion: the rows outlive the
// contract, because somebody may still have to answer for what was said.
type Tenant struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Disabled   bool       `json:"disabled"`
	CreatedAt  time.Time  `json:"createdAt"`
	CreatedBy  string     `json:"createdBy"`
	DisabledAt *time.Time `json:"disabledAt,omitempty"`
}

type Store struct {
	pool *pgxpool.Pool
	// touched is when each key_id last had its last_used_at written. Resolving a key
	// happens on every request; writing a timestamp on every request would make one row
	// per tenant the hottest in the database for a column nobody reads to the second.
	touched *lastUsed
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, touched: newLastUsed()}
}

// tenantID is deliberately narrow. It reaches a metric label, a log line and a URL path,
// and it is chosen by whoever creates the tenant rather than generated — so the shape has
// to be decided here rather than hoped for.
var tenantID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)

// Create makes a tenant. It does not issue a key: a tenant with no key can be created,
// inspected and named before anything can call as it, which is the order somebody
// setting one up actually works in.
func (s *Store) Create(ctx context.Context, id, name, actor string) (Tenant, error) {
	id = strings.TrimSpace(strings.ToLower(id))
	if !tenantID.MatchString(id) {
		return Tenant{}, fmt.Errorf("a tenant id is 3 to 40 characters of lowercase "+
			"letters, digits and hyphens, starting and ending with one of the first two; "+
			"%q is not", id)
	}
	if strings.TrimSpace(name) == "" {
		return Tenant{}, errors.New("a tenant needs a name")
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO tenant (tenant_id, name, created_by) VALUES ($1,$2,$3)
		ON CONFLICT (tenant_id) DO NOTHING`, id, strings.TrimSpace(name), actor)
	if err != nil {
		return Tenant{}, err
	}
	if tag.RowsAffected() == 0 {
		return Tenant{}, ErrExists
	}
	return s.Get(ctx, id)
}

func (s *Store) Get(ctx context.Context, id string) (Tenant, error) {
	var t Tenant
	err := s.pool.QueryRow(ctx, `
		SELECT tenant_id, name, disabled_at, created_at, created_by
		FROM tenant WHERE tenant_id = $1`, id).
		Scan(&t.ID, &t.Name, &t.DisabledAt, &t.CreatedAt, &t.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNoSuchTenant
	}
	t.Disabled = t.DisabledAt != nil
	return t, err
}

func (s *Store) List(ctx context.Context) ([]Tenant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_id, name, disabled_at, created_at, created_by
		FROM tenant ORDER BY tenant_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Name, &t.DisabledAt, &t.CreatedAt, &t.CreatedBy); err != nil {
			return nil, err
		}
		t.Disabled = t.DisabledAt != nil
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetDisabled stops every key of a tenant answering, without touching a row of its data.
//
// Reversible on purpose. Cancelling an account and erasing what a customer said are two
// decisions, and making the first perform the second is how the second gets made by
// accident.
func (s *Store) SetDisabled(ctx context.Context, id string, disabled bool) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tenant SET disabled_at = CASE WHEN $2 THEN coalesce(disabled_at, now()) END
		WHERE tenant_id = $1`, id, disabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoSuchTenant
	}
	return nil
}

// Key is what an issue returns. Secret is populated exactly once, here, and is never
// readable again from anywhere — it is not stored, so there is no code path that could
// return it a second time even if somebody wrote one.
type Key struct {
	KeyID      string     `json:"keyId"`
	TenantID   string     `json:"tenantId"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"createdAt"`
	CreatedBy  string     `json:"createdBy"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`

	// Secret is the whole key, prefix and all, and is set only by IssueKey.
	Secret string `json:"secret,omitempty"`
}

// IssueKey mints a key for a tenant and returns it in full, once.
func (s *Store) IssueKey(ctx context.Context, tenantID, label, actor string) (Key, error) {
	if strings.TrimSpace(label) == "" {
		return Key{}, errors.New("a key needs a label saying what it is for")
	}
	if _, err := s.Get(ctx, tenantID); err != nil {
		return Key{}, err
	}

	idBytes := make([]byte, 6)
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		return Key{}, err
	}
	if _, err := rand.Read(secretBytes); err != nil {
		return Key{}, err
	}
	keyID := hex.EncodeToString(idBytes)
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	sum := sha256.Sum256([]byte(secret))

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO tenant_api_key (key_id, tenant_id, key_hash, label, created_by)
		VALUES ($1,$2,$3,$4,$5)`,
		keyID, tenantID, sum[:], strings.TrimSpace(label), actor); err != nil {
		return Key{}, err
	}
	return Key{
		KeyID:    keyID,
		TenantID: tenantID,
		Label:    strings.TrimSpace(label),
		Secret:   KeyPrefix + "_" + keyID + "_" + secret,
	}, nil
}

// Resolve turns a presented key into a tenant id.
//
// Every failure is ErrNoSuchKey, and that is the point rather than laziness: a caller that
// can tell "this key_id does not exist" from "this key_id exists and your secret is wrong"
// has been handed an oracle for which key_ids exist.
func (s *Store) Resolve(ctx context.Context, presented string) (string, error) {
	keyID, secret, ok := splitKey(presented)
	if !ok {
		return "", ErrNoSuchKey
	}

	var tenantID string
	var stored []byte
	var revoked *time.Time
	var disabled *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT k.tenant_id, k.key_hash, k.revoked_at, t.disabled_at
		FROM tenant_api_key k JOIN tenant t ON t.tenant_id = k.tenant_id
		WHERE k.key_id = $1`, keyID).Scan(&tenantID, &stored, &revoked, &disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoSuchKey
	}
	if err != nil {
		// A database failure is not a bad key, and answering 401 for one would tell a
		// client to fix a key that is fine. The caller distinguishes.
		return "", err
	}

	sum := sha256.Sum256([]byte(secret))
	// Constant time, and reached only after an index lookup on key_id so there is one
	// candidate rather than a scan. A comparison that leaks by returning early on the
	// first differing byte leaks a secret one byte at a time.
	if subtle.ConstantTimeCompare(sum[:], stored) != 1 {
		return "", ErrNoSuchKey
	}
	if revoked != nil || disabled != nil {
		return "", ErrNoSuchKey
	}

	s.touch(ctx, keyID)
	return tenantID, nil
}

func (s *Store) Keys(ctx context.Context, tenantID string) ([]Key, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key_id, tenant_id, label, created_at, created_by, last_used_at, revoked_at
		FROM tenant_api_key WHERE tenant_id = $1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.KeyID, &k.TenantID, &k.Label, &k.CreatedAt, &k.CreatedBy,
			&k.LastUsedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeKey stops one key answering. The row stays: which key was revoked, when, and what
// it was for is the question somebody asks after an incident.
func (s *Store) RevokeKey(ctx context.Context, keyID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenant_api_key SET revoked_at = now() WHERE key_id = $1 AND revoked_at IS NULL`,
		keyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoSuchKey
	}
	return nil
}

// splitKey parses `csk_<key_id>_<secret>`.
//
// The prefix is checked rather than skipped: a caller that sends its *admin* token here by
// mistake should be told the key is wrong rather than have this package hash it and look
// it up, which would put an operator credential through a lookup that logs a key_id.
func splitKey(presented string) (keyID, secret string, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(presented), "_", 3)
	if len(parts) != 3 || parts[0] != KeyPrefix {
		return "", "", false
	}
	if len(parts[1]) != 12 || parts[2] == "" {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// touch records that a key was used, at most once a minute per key.
//
// Best effort and deliberately not in the request's error path: a failure to record when a
// key was last used must not refuse a customer whose key is valid.
func (s *Store) touch(ctx context.Context, keyID string) {
	if !s.touched.due(keyID, time.Now()) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = s.pool.Exec(ctx,
			`UPDATE tenant_api_key SET last_used_at = now() WHERE key_id = $1`, keyID)
	}()
}
