package rag_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/rag"
	"github.com/lai3d/ai-customer-service-go/internal/tenant"
)

// The isolation tests the Java side's ADR 002 asks for, on the corpus: write as A, read as
// B, find nothing.
//
// Unlike the edge, there is nothing else here that would refuse the read. A conversation is
// protected by its subject whether or not the tenant is in the predicate; a corpus is
// protected by the predicate and by nothing else, so removing it is immediately visible in
// every test below.

// newTenants makes throwaway tenants and **removes them and everything they own** when the
// test ends.
//
// The cleanup is not tidiness. `Replace` -- the bundled-corpus load -- refuses to run once
// another tenant has documents, which is the guard that stops it clearing a neighbour's
// corpus. A test that leaves a tenant's documents behind therefore breaks every later test
// that reloads the corpus, and it breaks them by *file name order*: this file sorts before
// reload_test.go and retrieval_test.go, so it did.
func newTenants(t *testing.T, names ...string) []string {
	t.Helper()
	ctx := context.Background()
	store := tenant.NewStore(sharedPool)
	out := make([]string, 0, len(names))
	for i, name := range names {
		id := fmt.Sprintf("%s-%d-%d", name, time.Now().UnixNano(), i)
		if _, err := store.Create(ctx, id, name, "platform"); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		// In foreign-key order: documents, then the active pointer, then the versions,
		// then the tenant itself.
		for _, sql := range []string{
			`DELETE FROM faq_document WHERE tenant_id = ANY($1)`,
			`DELETE FROM corpus_active WHERE tenant_id = ANY($1)`,
			`DELETE FROM corpus_version WHERE tenant_id = ANY($1)`,
			`DELETE FROM tenant WHERE tenant_id = ANY($1)`,
		} {
			if _, err := sharedPool.Exec(ctx, sql, out); err != nil {
				t.Errorf("could not clean up the test tenants: %v", err)
			}
		}
	})
	return out
}

// publishFor writes one version of `n` documents for a tenant and activates it. The
// documents carry the tenant in their text, so a passage retrieved from the wrong corpus is
// legible in the failure rather than merely being the wrong count.
func publishFor(t *testing.T, tenantID string, n int) string {
	t.Helper()
	ctx := context.Background()
	docs := make([]rag.Document, n)
	vectors := make([][]float32, n)
	for i := range docs {
		docs[i] = rag.Document{
			ID:       fmt.Sprintf("e%d:en", i),
			EntryID:  fmt.Sprintf("e%d", i),
			Language: "en",
			Category: "test",
			Question: fmt.Sprintf("question %d for %s", i, tenantID),
			Answer:   fmt.Sprintf("answer %d for %s", i, tenantID),
			Content:  fmt.Sprintf("passage: question %d for %s", i, tenantID),
		}
		// Every tenant's documents sit at the *same* points in the vector space, which is
		// the hostile case on purpose: if isolation were left to similarity, these would
		// be indistinguishable.
		vectors[i] = unit(i*7919, 384)
	}
	_, revision, err := sharedStore.Active(ctx, tenantID)
	if err != nil && !errors.Is(err, rag.ErrNoActiveVersion) {
		t.Fatal(err)
	}
	version := fmt.Sprintf("v-%s-%d", tenantID, time.Now().UnixNano())
	if err := sharedStore.Publish(ctx, tenantID, version, docs, vectors,
		"platform", "", revision); err != nil {
		t.Fatal(err)
	}
	return version
}

func TestASearchReadsOnlyItsOwnTenantsCorpus(t *testing.T) {
	ctx := context.Background()
	ids := newTenants(t, "acme", "globex")
	acme, globex := ids[0], ids[1]
	publishFor(t, acme, 6)
	publishFor(t, globex, 6)

	for _, id := range []string{acme, globex} {
		// Threshold -1 admits everything, including the vectors that sit at negative
		// cosine similarity to the query. The first version of this test left the
		// threshold at 0 and asserted six results, got three, and looked exactly like the
		// HNSW post-filter starvation this repository has been trying to reproduce -- it
		// was the threshold. Measured with EXPLAIN before believing either story: the
		// planner picks the b-tree on (tenant_id, corpus_version) at this size and returns
		// every matching row, with iterative_scan off, strict and relaxed alike.
		passages, err := sharedStore.Search(ctx, unit(0, 384),
			rag.SearchOptions{TopK: 8, TenantID: id, Threshold: -1})
		if err != nil {
			t.Fatal(err)
		}
		if len(passages) == 0 {
			t.Fatalf("%s retrieved nothing from its own corpus", id)
		}
		for _, p := range passages {
			if !strings.Contains(p.Answer, id) {
				t.Errorf("searching as %s returned %q, which belongs to another tenant",
					id, p.Answer)
			}
		}
		// Six documents each, and the top-8 must not be filled from the neighbour.
		if len(passages) != 6 {
			t.Errorf("%s got %d passages from a 6-document corpus", id, len(passages))
		}
	}
}

// A tenant with no corpus retrieves nothing rather than somebody else's. That is the shape
// of a new tenant on its first day, and the answer it should produce is a grounded refusal.
func TestATenantWithNoCorpusRetrievesNothing(t *testing.T) {
	ctx := context.Background()
	ids := newTenants(t, "acme", "empty")
	publishFor(t, ids[0], 6)

	passages, err := sharedStore.Search(ctx, unit(0, 384),
		rag.SearchOptions{TopK: 8, TenantID: ids[1], Threshold: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(passages) != 0 {
		t.Errorf("a tenant with no corpus retrieved %d passages: %v",
			len(passages), passages[0].Answer)
	}
}

// One active version per tenant, which is what replacing the primary key on a constant
// bought. Activating for one tenant must not move another's.
func TestEachTenantHasItsOwnActiveVersion(t *testing.T) {
	ctx := context.Background()
	ids := newTenants(t, "acme", "globex")
	acme, globex := ids[0], ids[1]

	first := publishFor(t, acme, 4)
	theirs := publishFor(t, globex, 4)

	got, _, err := sharedStore.Active(ctx, acme)
	if err != nil || got != first {
		t.Fatalf("A's active version is %q (%v), want %q", got, err, first)
	}

	second := publishFor(t, acme, 4)
	if got, _, err := sharedStore.Active(ctx, acme); err != nil || got != second {
		t.Errorf("A did not move to its new version: %q %v", got, err)
	}
	if got, _, err := sharedStore.Active(ctx, globex); err != nil || got != theirs {
		t.Errorf("B's active version moved when A published: %q, want %q", got, theirs)
	}
}

// A version is not activatable from another tenant, even by name. Version names are
// globally unique and therefore guessable from a log line or a screenshot, which is exactly
// the way one would be tried.
func TestAVersionCannotBeActivatedByAnotherTenant(t *testing.T) {
	ctx := context.Background()
	ids := newTenants(t, "acme", "globex")
	acme, globex := ids[0], ids[1]

	mine := publishFor(t, acme, 4)
	theirs := publishFor(t, globex, 4)

	_, revision, err := sharedStore.Active(ctx, globex)
	if err != nil {
		t.Fatal(err)
	}
	if err := sharedStore.Activate(ctx, globex, mine, "platform", revision); err == nil {
		t.Error("one tenant activated another tenant's version")
	}
	if got, _, err := sharedStore.Active(ctx, globex); err != nil || got != theirs {
		t.Errorf("B's active version is now %q; the refused activation took effect", got)
	}
	// And listing versions shows only its own.
	versions, err := sharedStore.Versions(ctx, globex)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range versions {
		if v.Version == mine {
			t.Error("one tenant's version list contains another tenant's version")
		}
	}
}

// Retention keeps the newest N *per tenant*. A global newest-N would retire a neighbour's
// retained history the moment one tenant published N times in an afternoon.
//
// The quiet tenant publishes **twice**, and the assertion is about its *older* version.
// That is not incidental: the first version of this test published once and asserted the
// tenant could still be searched, which passed under a deliberately global retention --
// because a tenant's active version is protected by a separate clause that has nothing to
// do with tenancy. The perturbation was run and it passed. What separates the two rules is
// a version that is this tenant's second-newest and the whole service's sixth.
func TestRetentionKeepsTheNewestVersionsPerTenantRatherThanGlobally(t *testing.T) {
	ctx := context.Background()
	ids := newTenants(t, "quiet", "busy")
	quiet, busy := ids[0], ids[1]

	older := publishFor(t, quiet, 4)
	publishFor(t, quiet, 4) // the quiet tenant's active version
	for i := 0; i < 5; i++ {
		publishFor(t, busy, 4)
	}

	if _, err := sharedStore.Retain(ctx, 2); err != nil {
		t.Fatal(err)
	}

	// The quiet tenant's second-newest version is still there: keep=2 means two per
	// tenant. Globally it is the sixth-newest of seven and would have gone.
	var kept int
	if err := sharedPool.QueryRow(ctx,
		`SELECT count(*) FROM faq_document WHERE corpus_version = $1`, older).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept == 0 {
		t.Errorf("the quiet tenant's version %s was swept because a neighbour published "+
			"five times; retention is counting globally", older)
	}

	// And the quiet tenant can still be searched, which is the active-version guard doing
	// its own separate job.
	passages, err := sharedStore.Search(ctx, unit(0, 384),
		rag.SearchOptions{TopK: 8, TenantID: quiet, Threshold: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(passages) == 0 {
		t.Error("the quiet tenant's active corpus was swept")
	}

	// And the busy tenant's own retention still happened, so this is not passing by
	// retaining everything.
	var remaining int
	if err := sharedPool.QueryRow(ctx, `
		SELECT count(DISTINCT corpus_version) FROM faq_document WHERE tenant_id = $1`,
		busy).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining > 2 {
		t.Errorf("the busy tenant kept %d versions of documents, want at most 2", remaining)
	}
}

// The bundled-corpus load clears the whole table, so it refuses once somebody else has
// documents. Destroying a neighbour's corpus and silently degrading retrieval are both
// worse than a start-up error naming what is in the way -- see the comment on Replace.
func TestLoadingTheBundledCorpusRefusesWhenAnotherTenantHasDocuments(t *testing.T) {
	ctx := context.Background()
	ids := newTenants(t, "acme")
	publishFor(t, ids[0], 4)

	docs, vectors := docsFor("bundled", 3)
	err := sharedStore.Replace(ctx, tenant.Default, docs, vectors)
	if !errors.Is(err, rag.ErrWouldClearOtherTenants) {
		t.Fatalf("the bundled load returned %v, want ErrWouldClearOtherTenants", err)
	}

	// And the other tenant still has its corpus, which is the thing the refusal protects.
	passages, err := sharedStore.Search(ctx, unit(0, 384),
		rag.SearchOptions{TopK: 8, TenantID: ids[0], Threshold: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(passages) == 0 {
		t.Error("the refused load cleared the other tenant's corpus anyway")
	}
}

// A search with no tenant is refused rather than defaulted. Defaulting would read whichever
// documents the rest of the predicate matched, across every customer of this service, and
// would look exactly like a search that found nothing relevant.
func TestASearchWithNoTenantIsRefused(t *testing.T) {
	if _, err := sharedStore.Search(context.Background(), unit(0, 384),
		rag.SearchOptions{TopK: 8}); err == nil {
		t.Error("a search with no tenant was allowed")
	}
	if _, err := sharedRetrier.Retrieve(context.Background(), "", "anything"); err == nil {
		t.Error("a retrieval with no tenant was allowed")
	}
}
