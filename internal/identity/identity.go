// Package identity answers two questions the chat endpoint could not previously ask:
// who is this, and is this conversation theirs.
//
// Before it existed, `conversationId` came from the client with only a length check and
// chat memory was keyed on it. Sending a message with somebody else's conversation id
// appended to their history and composed the model's reply from their context. That is a
// confidentiality break rather than a missing feature, and it is what this package is for.
//
// Sessions are anonymous on purpose. This service does not know who a customer is; the
// product it is embedded in does. What is needed here is only that two customers are not
// the same subject, and that the subject cannot be guessed -- which an opaque server-issued
// token gives without this service growing a user table, a password, or a login.
//
// Verifying an identity asserted by the host product (a JWT, an OIDC subject) is the other
// half, and is deliberately not built: it is a fork in the design that depends on what the
// host product already has. Authenticator is the seam it would arrive through.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Subject is who a request is from. An empty ID means the request is unattributed, which
// is only reachable in Mode "off".
// Subject is who is asking, and for whom.
//
// TenantID is not decoration on the id: it is half the identity. A subject id is 128 bits
// of randomness and would not collide across tenants on its own, but "would not collide"
// is an argument about probability, and every query below carries the tenant as a
// predicate so that isolation is a constraint instead.
type Subject struct {
	ID       string
	TenantID string
}

func (s Subject) Anonymous() bool { return s.ID == "" || s.TenantID == "" }

type Mode string

const (
	// ModeOff keeps the pre-identity behaviour: no sessions, conversation ids supplied by
	// the client, no ownership. It exists because the benchmark and the cross-repository
	// comparison drive the endpoint with `curl` and a fixed id, and changing that would
	// change what the two implementations are measuring. It is not a production mode and
	// the server says so at start-up, loudly, every time.
	ModeOff Mode = "off"
	// ModeSession issues an opaque token, binds conversations to it, and refuses a
	// conversation belonging to someone else.
	ModeSession Mode = "session"
)

func ParseMode(s string) (Mode, error) {
	switch Mode(strings.TrimSpace(s)) {
	case "", ModeOff:
		return ModeOff, nil
	case ModeSession:
		return ModeSession, nil
	default:
		return "", fmt.Errorf("AUTH_MODE %q is not off or session", s)
	}
}

var (
	ErrNoSession  = errors.New("no session")
	ErrNotYours   = errors.New("that conversation belongs to someone else")
	ErrNoSuchConv = errors.New("no such conversation")
)

// Sessions issues and verifies session tokens.
//
// The token is stored as a SHA-256 hash. A database dump then does not hand its reader a
// working session for every customer -- the same reason a password column holds a hash,
// and it costs one hash per request to keep.
type Sessions struct {
	pool *pgxpool.Pool
	ttl  time.Duration
}

func NewSessions(pool *pgxpool.Pool, ttl time.Duration) *Sessions {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Sessions{pool: pool, ttl: ttl}
}

// Issue returns a new session token. The token is returned exactly once and never stored,
// so it cannot be recovered from the database or from a log line.
func (s *Sessions) Issue(ctx context.Context, tenantID string) (token string, subject Subject, expires time.Time, err error) {
	if tenantID == "" {
		// Refused rather than defaulted. A session with no tenant would be a session that
		// belongs to whichever tenant a later query forgets to filter on.
		return "", Subject{}, time.Time{}, errors.New("refusing to issue a session with no tenant")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", Subject{}, time.Time{}, err
	}
	token = base64.RawURLEncoding.EncodeToString(raw)

	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", Subject{}, time.Time{}, err
	}
	subject = Subject{ID: hex.EncodeToString(id), TenantID: tenantID}
	expires = time.Now().Add(s.ttl)

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO chat_session (token_hash, subject, tenant_id, created_at, last_seen_at, expires_at)
		VALUES ($1, $2, $3, now(), now(), $4)`,
		hashToken(token), subject.ID, tenantID, expires); err != nil {
		return "", Subject{}, time.Time{}, err
	}
	return token, subject, expires, nil
}

// Verify resolves a token to its subject, or returns ErrNoSession. An expired row is not
// deleted here: expiry is a read-side condition so that a clock skew or a slow sweep
// cannot resurrect a session, and Sweep removes the rows on its own schedule.
func (s *Sessions) Verify(ctx context.Context, token string) (Subject, error) {
	if token == "" {
		return Subject{}, ErrNoSession
	}
	var subject, tenantID string
	err := s.pool.QueryRow(ctx, `
		UPDATE chat_session SET last_seen_at = now()
		WHERE token_hash = $1 AND expires_at > now()
		RETURNING subject, tenant_id`, hashToken(token)).Scan(&subject, &tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Subject{}, ErrNoSession
	}
	if err != nil {
		return Subject{}, err
	}
	return Subject{ID: subject, TenantID: tenantID}, nil
}

// Sweep deletes sessions that expired more than grace ago. Conversations outlive their
// session on purpose -- the operations surface still has to be able to read what was said.
func (s *Sessions) Sweep(ctx context.Context, grace time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM chat_session WHERE expires_at < now() - $1::interval`,
		grace.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// BearerToken reads `Authorization: Bearer <token>`. Case-insensitive on the scheme,
// because clients get that wrong and the failure is otherwise indistinguishable from a
// bad token.
func BearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if len(header) < 7 || !strings.EqualFold(header[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(header[7:])
}

// Conversations records which subject a conversation belongs to.
type Conversations struct{ pool *pgxpool.Pool }

func NewConversations(pool *pgxpool.Pool) *Conversations {
	return &Conversations{pool: pool}
}

// Claim binds a conversation to a subject, or confirms that it is already theirs.
//
// The insert is the check: `ON CONFLICT DO NOTHING` followed by reading the owner means
// two simultaneous first turns cannot both claim the same id. Doing it as a SELECT and
// then an INSERT is the same race the ticket cap had, and it fails the same way -- rarely,
// and in favour of the attacker.
func (c *Conversations) Claim(ctx context.Context, id string, subject Subject) error {
	if subject.Anonymous() {
		return errors.New("refusing to bind a conversation to an empty subject")
	}
	// The conflict target stays the conversation id alone, and that is deliberate: ids are
	// server-issued and must be unique across the whole service, not merely within a
	// tenant. Two tenants that could each hold conversation `abc` would make every id in a
	// log line ambiguous, and the first cross-tenant join written afterwards would return
	// both.
	//
	// DO UPDATE with the row's own value rather than DO NOTHING, and the difference is a
	// bug this had. DO NOTHING returns no row *and does not wait*, so a loser whose winner
	// has not committed yet sees neither its own insert nor the other's -- and the
	// `UNION ALL` reading the table finds nothing either, because that read is on the same
	// snapshot. The customer got `no rows in result set`, which the edge turns into a 503
	// on the first turn of a conversation.
	//
	// It was rare enough to look like nothing: twelve concurrent claims reproduce it
	// roughly four runs in five, and one claim never does. DO UPDATE takes the row lock,
	// waits for the other transaction to finish, and then returns the winner -- so the
	// loser learns it lost instead of being told the database is broken. The update is a
	// no-op by construction, and Claim runs once per conversation rather than once per
	// turn, so the dead tuple it costs is one per conversation.
	var owner, tenantID string
	err := c.pool.QueryRow(ctx, `
		INSERT INTO conversation_owner (conversation_id, subject, tenant_id, created_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (conversation_id) DO UPDATE
			SET subject = conversation_owner.subject
		RETURNING subject, tenant_id`,
		id, subject.ID, subject.TenantID).Scan(&owner, &tenantID)
	if err != nil {
		return err
	}
	if owner != subject.ID || tenantID != subject.TenantID {
		return ErrNotYours
	}
	return nil
}

// Owns reports whether the subject may use an existing conversation. A conversation that
// does not exist is ErrNoSuchConv, which the handler turns into the same 404 as one owned
// by someone else: a 403 confirms the id exists, and an id someone else can enumerate is
// most of what this is protecting.
func (c *Conversations) Owns(ctx context.Context, id string, subject Subject) error {
	if subject.Anonymous() {
		return ErrNotYours
	}
	// The tenant is in the WHERE clause rather than compared afterwards, so a conversation
	// belonging to another tenant is ErrNoSuchConv rather than ErrNotYours. Both become the
	// same 404 at the edge, and this is the shape that stays correct if they ever stop
	// being the same: one tenant must not learn that another tenant's id exists.
	var owner string
	err := c.pool.QueryRow(ctx,
		`SELECT subject FROM conversation_owner WHERE conversation_id = $1 AND tenant_id = $2`,
		id, subject.TenantID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoSuchConv
	}
	if err != nil {
		return err
	}
	if owner != subject.ID {
		return ErrNotYours
	}
	return nil
}
