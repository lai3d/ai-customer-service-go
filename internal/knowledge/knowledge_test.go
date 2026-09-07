package knowledge_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/knowledge"
	"github.com/lai3d/ai-customer-service-go/internal/rag"
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

// spreading returns a distinct non-zero vector per text. Distinct because pgvector's HNSW
// keeps one graph element per distinct vector; non-zero because a zero vector has NaN
// cosine distance and a search silently returns nothing.
type spreading struct{ calls int }

func (s *spreading) EmbedPassages(_ context.Context, texts []string) ([][]float32, error) {
	s.calls++
	out := make([][]float32, len(texts))
	for i := range out {
		v := make([]float32, 384)
		v[(s.calls*7+i)%384] = 1
		out[i] = v
	}
	return out, nil
}

func store(t *testing.T) (*knowledge.Store, *rag.Store) {
	t.Helper()
	corpus := rag.NewStore(pool)
	t.Cleanup(func() { reset(t) })
	return knowledge.NewStore(pool, corpus, &spreading{}), corpus
}

func reset(t *testing.T) {
	ctx := context.Background()
	for _, sql := range []string{
		`DELETE FROM faq_document`,
		`DELETE FROM corpus_active`,
		`DELETE FROM corpus_version`,
		`DELETE FROM knowledge_entry`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("reset: %v", err)
		}
	}
}

func seed(t *testing.T, s *knowledge.Store) {
	t.Helper()
	corpus, err := rag.LoadCorpus("../../corpus/faq.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SeedFromCorpus(context.Background(), tenant.Default, corpus); err != nil {
		t.Fatal(err)
	}
}

// The editor must not open on an empty list. If it does, the first publication means
// "replace the knowledge base with whatever one person just typed", and nobody finds out
// until customers stop being answered.
func TestTheDraftsStartAsTheBundledCorpusAndAreNotReseeded(t *testing.T) {
	ctx := context.Background()
	s, _ := store(t)

	corpus, err := rag.LoadCorpus("../../corpus/faq.json")
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := s.SeedFromCorpus(ctx, tenant.Default, corpus)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := s.List(ctx, tenant.Default)
	if err != nil {
		t.Fatal(err)
	}
	if seeded == 0 || len(entries) != seeded {
		t.Fatalf("seeded %d and the list has %d", seeded, len(entries))
	}

	// An operator deletes one, then the service restarts. Re-seeding would resurrect it,
	// which is a deletion that undoes itself and looks like the operator imagining things.
	if err := s.Delete(ctx, tenant.Default, entries[0].EntryID, entries[0].Language, "alex"); err != nil {
		t.Fatal(err)
	}
	again, err := s.SeedFromCorpus(ctx, tenant.Default, corpus)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("a restart re-seeded %d entries over an edited knowledge base", again)
	}
	after, err := s.List(ctx, tenant.Default)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range after {
		if e.EntryID == entries[0].EntryID && e.Language == entries[0].Language && !e.Deleted {
			t.Error("the deleted entry came back")
		}
	}
}

// Editing does not change what customers are answered from. Publishing does. If that is
// not true, an operator saving a half-written sentence has put it in front of somebody.
func TestAnEditIsNotLiveUntilItIsPublished(t *testing.T) {
	ctx := context.Background()
	s, corpus := store(t)
	seed(t, s)

	// The first publication on a database with no documents: revision 0 says "there was no
	// active version when I read the state". This is the path a service started with
	// IngestOnStartup off would take, and it did not exist until this test needed it --
	// AdoptBundled has nothing to stamp when faq_document is empty, so without it such a
	// database could never get a version at all.
	if _, err := s.Publish(ctx, tenant.Default, "alex", "the first one", 0); err != nil {
		t.Fatal(err)
	}
	_, revision, err := s.State(ctx, tenant.Default)
	if err != nil {
		t.Fatal(err)
	}

	const marker = "wombats are dispatched on Thursdays"
	if _, err := s.Save(ctx, tenant.Default, knowledge.Entry{
		EntryID: "shipping-times", Language: "en", Category: "shipping",
		Question: "when does it ship?", Answer: marker,
	}, "alex"); err != nil {
		t.Fatal(err)
	}

	live, err := activeAnswers(ctx, corpus)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(live, marker) {
		t.Error("an unpublished edit is already being retrieved")
	}

	version, err := s.Publish(ctx, tenant.Default, "alex", "changed shipping", revision)
	if err != nil {
		t.Fatal(err)
	}
	if version == "" {
		t.Fatal("no version name")
	}
	live, err = activeAnswers(ctx, corpus)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(live, marker) {
		t.Error("the edit is still not live after publishing")
	}
}

// Two operators, two stale pages. The loser is told rather than silently overwriting the
// winner -- the same optimistic concurrency the ticket workflow uses.
func TestASecondPublicationFromAStalePageIsRefused(t *testing.T) {
	ctx := context.Background()
	s, _ := store(t)
	seed(t, s)
	if _, err := s.Publish(ctx, tenant.Default, "alex", "the first one", 0); err != nil {
		t.Fatal(err)
	}

	_, revision, err := s.State(ctx, tenant.Default)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, tenant.Default, "alex", "first", revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, tenant.Default, "dana", "from a page loaded before alex published", revision); !errors.Is(err, rag.ErrStaleActivation) {
		t.Errorf("a stale publication returned %v, want ErrStaleActivation", err)
	}
}

func TestAnEmptyKnowledgeBaseIsNotPublishable(t *testing.T) {
	ctx := context.Background()
	s, _ := store(t)
	seed(t, s)
	if _, err := s.Publish(ctx, tenant.Default, "alex", "the first one", 0); err != nil {
		t.Fatal(err)
	}
	entries, err := s.List(ctx, tenant.Default)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := s.Delete(ctx, tenant.Default, e.EntryID, e.Language, "alex"); err != nil {
			t.Fatal(err)
		}
	}
	_, revision, err := s.State(ctx, tenant.Default)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, tenant.Default, "alex", "everything deleted", revision); !errors.Is(err, knowledge.ErrEmptyDraft) {
		t.Errorf("publishing an empty knowledge base returned %v", err)
	}
}

// The length bound is the one property of an entry that can be checked without judging its
// content, and an editable knowledge base is an input the model reads.
func TestAnEntryIsBoundedAndValidated(t *testing.T) {
	ctx := context.Background()
	s, _ := store(t)

	cases := []struct {
		name  string
		entry knowledge.Entry
	}{
		{"no id", knowledge.Entry{Language: "en", Question: "q", Answer: "a"}},
		{"no language", knowledge.Entry{EntryID: "e", Question: "q", Answer: "a"}},
		{"no question", knowledge.Entry{EntryID: "e", Language: "en", Answer: "a"}},
		{"no answer", knowledge.Entry{EntryID: "e", Language: "en", Question: "q"}},
		{"an answer longer than the bound", knowledge.Entry{EntryID: "e", Language: "en",
			Question: "q", Answer: strings.Repeat("x", knowledge.MaxAnswerLength+1)}},
	}
	for _, c := range cases {
		if _, err := s.Save(ctx, tenant.Default, c.entry, "alex"); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
	if _, err := s.Save(ctx, tenant.Default, knowledge.Entry{EntryID: "e", Language: "en", Category: "c",
		Question: "q", Answer: "a"}, ""); err == nil {
		t.Error("an edit with no author was accepted")
	}
}

// activeAnswers returns every answer in the active corpus version, which is what a customer
// can be told.
func activeAnswers(ctx context.Context, corpus *rag.Store) (string, error) {
	rows, err := pool.Query(ctx, `
		SELECT answer FROM faq_document
		WHERE corpus_version = (SELECT version FROM corpus_active WHERE tenant_id = $1)`,
		tenant.Default)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var all strings.Builder
	for rows.Next() {
		var answer string
		if err := rows.Scan(&answer); err != nil {
			return "", err
		}
		all.WriteString(answer)
		all.WriteString("\n")
	}
	return all.String(), rows.Err()
}

// Two tenants can both have an entry called `returns-window`, and they are different
// entries. Without the tenant in the primary key the second save silently overwrites the
// first, and the operator who typed it sees their own text back -- the failure that looks
// most like everything working.
func TestTwoTenantsCanBothHaveAnEntryWithTheSameId(t *testing.T) {
	ctx := context.Background()
	s, _ := store(t)
	tenants := tenant.NewStore(pool)
	a, err := tenants.Create(ctx, fmt.Sprintf("acme-%d", os.Getpid()), "Acme", "platform")
	if err != nil {
		t.Fatal(err)
	}
	b, err := tenants.Create(ctx, fmt.Sprintf("globex-%d", os.Getpid()), "Globex", "platform")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM knowledge_entry WHERE tenant_id = ANY($1)`,
			[]string{a.ID, b.ID})
		_, _ = pool.Exec(ctx, `DELETE FROM tenant WHERE tenant_id = ANY($1)`,
			[]string{a.ID, b.ID})
	})

	entry := func(answer string) knowledge.Entry {
		return knowledge.Entry{EntryID: "returns-window", Language: "en",
			Category: "returns", Question: "how long?", Answer: answer}
	}
	if _, err := s.Save(ctx, a.ID, entry("thirty days"), "alex"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, b.ID, entry("fourteen days"), "dana"); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ id, want string }{{a.ID, "thirty days"}, {b.ID, "fourteen days"}} {
		entries, err := s.List(ctx, c.id)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("%s has %d entries, want 1: a tenant is seeing another's drafts", c.id, len(entries))
		}
		if entries[0].Answer != c.want {
			t.Errorf("%s reads %q, want %q", c.id, entries[0].Answer, c.want)
		}
	}

	// And deleting one tenant's entry leaves the other's alone.
	if err := s.Delete(ctx, a.ID, "returns-window", "en", "alex"); err != nil {
		t.Fatal(err)
	}
	entries, err := s.List(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Deleted {
		t.Error("deleting one tenant's entry deleted another tenant's")
	}
}

// Seeding is idempotent per tenant. A global count would mean the second tenant to exist is
// never seeded, and its editor would open empty for a reason nobody could find.
func TestSeedingIsIdempotentPerTenantRatherThanGlobally(t *testing.T) {
	ctx := context.Background()
	s, _ := store(t)
	tenants := tenant.NewStore(pool)
	b, err := tenants.Create(ctx, fmt.Sprintf("seed-%d", os.Getpid()), "Seeded", "platform")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM knowledge_entry WHERE tenant_id = $1`, b.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM tenant WHERE tenant_id = $1`, b.ID)
	})

	corpus := rag.Corpus{Entries: []rag.Entry{{
		ID: "returns-window", Category: "returns",
		Localized: []rag.Localized{{Language: "en", Question: "how long?", Answer: "thirty days"}},
	}}}

	if seeded, err := s.SeedFromCorpus(ctx, tenant.Default, corpus); err != nil || seeded == 0 {
		t.Fatalf("the default tenant was not seeded: %d %v", seeded, err)
	}
	// The second tenant is empty and must be seeded, even though the table is not.
	seeded, err := s.SeedFromCorpus(ctx, b.ID, corpus)
	if err != nil {
		t.Fatal(err)
	}
	if seeded == 0 {
		t.Fatal("a new tenant was not seeded because another tenant already had drafts; " +
			"its editor would open empty and the first publication would replace its corpus")
	}
	entries, err := s.List(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the new tenant has %d entries", len(entries))
	}
	// And seeding it again does nothing, which is the property that survives a restart.
	if again, err := s.SeedFromCorpus(ctx, b.ID, corpus); err != nil || again != 0 {
		t.Errorf("re-seeding an edited tenant added %d entries (%v)", again, err)
	}
}
