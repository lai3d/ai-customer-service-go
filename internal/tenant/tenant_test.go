package tenant_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/tenant"
	"github.com/lai3d/ai-customer-service-go/internal/testsupport"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	p, stop, err := testsupport.StartPostgres(context.Background(), 384)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	pool = p
	code := m.Run()
	stop()
	os.Exit(code)
}

func newTenant(t *testing.T, s *tenant.Store, name string) tenant.Tenant {
	t.Helper()
	id := fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
	if len(id) > 40 {
		id = id[:40]
	}
	made, err := s.Create(context.Background(), id, strings.ToTitle(name), "platform")
	if err != nil {
		t.Fatal(err)
	}
	return made
}

// The migration has to have made the tenant everything else already belongs to, and it has
// to be a real row rather than a value the code substitutes when it finds none. A null
// standing in for a tenant is the state a forgotten predicate produces, and it would be
// indistinguishable from correct here.
func TestTheDefaultTenantExists(t *testing.T) {
	got, err := tenant.NewStore(pool).Get(context.Background(), tenant.Default)
	if err != nil {
		t.Fatalf("the default tenant is not in the table: %v", err)
	}
	if got.Disabled {
		t.Error("the default tenant is disabled; nothing that exists today would answer")
	}
}

// A key resolves to its own tenant and to no other. This is the resolver's whole job, and
// the second half of it is the half that matters.
func TestAKeyResolvesToItsOwnTenantAndNoOther(t *testing.T) {
	ctx := context.Background()
	s := tenant.NewStore(pool)
	a := newTenant(t, s, "acme")
	b := newTenant(t, s, "globex")

	keyA, err := s.IssueKey(ctx, a.ID, "server-side integration", "platform")
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := s.IssueKey(ctx, b.ID, "server-side integration", "platform")
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Resolve(ctx, keyA.Secret)
	if err != nil || got != a.ID {
		t.Errorf("A's key resolved to %q (%v), want %q", got, err, a.ID)
	}
	got, err = s.Resolve(ctx, keyB.Secret)
	if err != nil || got != b.ID {
		t.Errorf("B's key resolved to %q (%v), want %q", got, err, b.ID)
	}
	if keyA.Secret == keyB.Secret {
		t.Fatal("two issued keys are the same string")
	}
}

// The secret is shown once and is not in the database. Checked by looking, rather than by
// reading the INSERT: what makes this true is that nothing stores it, and a test that
// asserts the code does not store it is a test of the code somebody is about to change.
func TestTheSecretIsNeverStoredAndNeverReturnedAgain(t *testing.T) {
	ctx := context.Background()
	s := tenant.NewStore(pool)
	made := newTenant(t, s, "acme")

	key, err := s.IssueKey(ctx, made.ID, "the one key", "platform")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key.Secret, tenant.KeyPrefix+"_") {
		t.Errorf("the issued key %q does not carry the greppable prefix", key.Secret)
	}

	// Nowhere in the row, in any column, in any form this test can think of.
	var found int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM tenant_api_key
		WHERE key_id = $1 AND (
			label = $2 OR created_by = $2 OR encode(key_hash,'escape') LIKE '%'||$2||'%')`,
		key.KeyID, key.Secret).Scan(&found); err != nil {
		t.Fatal(err)
	}
	if found != 0 {
		t.Error("the secret is in the row")
	}

	listed, err := s.Keys(ctx, made.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("%d keys listed, want 1", len(listed))
	}
	if listed[0].Secret != "" {
		t.Error("listing a tenant's keys hands back the secret")
	}
	if listed[0].KeyID != key.KeyID || listed[0].Label != "the one key" {
		t.Errorf("the listed key is %+v", listed[0])
	}
}

// Every way a key can fail is the same failure, and that is the design rather than an
// omission. A caller that can tell "this key_id does not exist" from "it exists and your
// secret is wrong" has an oracle for which key_ids exist.
func TestEveryWayAKeyCanFailLooksIdentical(t *testing.T) {
	ctx := context.Background()
	s := tenant.NewStore(pool)
	live := newTenant(t, s, "acme")
	off := newTenant(t, s, "globex")

	good, err := s.IssueKey(ctx, live.ID, "good", "platform")
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := s.IssueKey(ctx, live.ID, "revoked", "platform")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeKey(ctx, revoked.KeyID); err != nil {
		t.Fatal(err)
	}
	disabled, err := s.IssueKey(ctx, off.ID, "on a disabled tenant", "platform")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled(ctx, off.ID, true); err != nil {
		t.Fatal(err)
	}

	// The right key_id with the wrong secret: the case a comparison that returns early
	// would leak, and the one an attacker who has seen a key_id in a log would try.
	wrongSecret := tenant.KeyPrefix + "_" + good.KeyID + "_" + strings.Repeat("A", 43)

	for _, c := range []struct {
		what string
		key  string
	}{
		{"an empty string", ""},
		{"something that is not a key at all", "hello"},
		{"an operator token sent by mistake", "alex:0123456789abcdef:operator"},
		{"the right shape with an unknown key_id", tenant.KeyPrefix + "_0123456789ab_" + strings.Repeat("A", 43)},
		{"a key_id that is not hex", tenant.KeyPrefix + "_zzzzzzzzzzzz_" + strings.Repeat("A", 43)},
		{"the right key_id and the wrong secret", wrongSecret},
		{"a revoked key", revoked.Secret},
		{"a key on a disabled tenant", disabled.Secret},
	} {
		if _, err := s.Resolve(ctx, c.key); !errors.Is(err, tenant.ErrNoSuchKey) {
			t.Errorf("%s returned %v, want ErrNoSuchKey", c.what, err)
		}
	}

	// And the good one still works, so the list above is not passing because everything
	// fails.
	if got, err := s.Resolve(ctx, good.Secret); err != nil || got != live.ID {
		t.Errorf("the valid key resolved to %q (%v)", got, err)
	}
}

// Disabling is reversible and touches no data. Cancelling an account and erasing what a
// customer said are two decisions, and the second is not a side effect of the first.
func TestDisablingATenantIsReversibleAndKeepsItsRows(t *testing.T) {
	ctx := context.Background()
	s := tenant.NewStore(pool)
	made := newTenant(t, s, "acme")
	key, err := s.IssueKey(ctx, made.ID, "k", "platform")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SetDisabled(ctx, made.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, key.Secret); !errors.Is(err, tenant.ErrNoSuchKey) {
		t.Errorf("a disabled tenant's key still resolves: %v", err)
	}
	got, err := s.Get(ctx, made.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Disabled || got.DisabledAt == nil {
		t.Error("the tenant does not report itself disabled")
	}

	if err := s.SetDisabled(ctx, made.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Resolve(ctx, key.Secret); err != nil || got != made.ID {
		t.Errorf("re-enabling did not bring the key back: %q %v", got, err)
	}
}

func TestATenantIdIsValidatedAndUnique(t *testing.T) {
	ctx := context.Background()
	s := tenant.NewStore(pool)

	for _, bad := range []string{"", "a", "ab", "-acme", "acme-", "ACME!", "acme corp",
		strings.Repeat("a", 41), "acme/../default"} {
		if _, err := s.Create(ctx, bad, "Name", "platform"); err == nil {
			t.Errorf("the tenant id %q was accepted", bad)
		}
	}
	if _, err := s.Create(ctx, "valid-id-here", "", "platform"); err == nil {
		t.Error("a tenant with no name was accepted")
	}

	made := newTenant(t, s, "acme")
	if _, err := s.Create(ctx, made.ID, "Someone Else", "platform"); !errors.Is(err, tenant.ErrExists) {
		t.Errorf("creating a tenant twice returned %v", err)
	}
	// Taking a tenant id that already exists must not hand its keys to the second caller,
	// which is the whole reason the insert is ON CONFLICT DO NOTHING rather than an upsert.
	if _, err := s.Create(ctx, tenant.Default, "Not Yours", "platform"); !errors.Is(err, tenant.ErrExists) {
		t.Errorf("the default tenant could be taken over: %v", err)
	}
}

func TestIssuingAKeyForATenantThatDoesNotExistIsRefused(t *testing.T) {
	ctx := context.Background()
	s := tenant.NewStore(pool)
	if _, err := s.IssueKey(ctx, "no-such-tenant", "k", "platform"); !errors.Is(err, tenant.ErrNoSuchTenant) {
		t.Errorf("issuing against a missing tenant returned %v", err)
	}
	made := newTenant(t, s, "acme")
	if _, err := s.IssueKey(ctx, made.ID, "  ", "platform"); err == nil {
		t.Error("a key with no label was issued; revoking the right one would be a guess")
	}
}

// Revoking twice is not an error worth failing a request over, and it must not report
// success for something it did not do.
func TestRevokingAKeyTwiceSaysSo(t *testing.T) {
	ctx := context.Background()
	s := tenant.NewStore(pool)
	made := newTenant(t, s, "acme")
	key, err := s.IssueKey(ctx, made.ID, "k", "platform")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeKey(ctx, key.KeyID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeKey(ctx, key.KeyID); !errors.Is(err, tenant.ErrNoSuchKey) {
		t.Errorf("revoking an already-revoked key returned %v", err)
	}
	listed, err := s.Keys(ctx, made.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].RevokedAt == nil {
		t.Error("the revoked key is not in the list, or does not say it was revoked; " +
			"which key was revoked and when is the question asked after an incident")
	}
}
