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
	if _, err := s.SeedFromCorpus(context.Background(), corpus); err != nil {
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
	seeded, err := s.SeedFromCorpus(ctx, corpus)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if seeded == 0 || len(entries) != seeded {
		t.Fatalf("seeded %d and the list has %d", seeded, len(entries))
	}

	// An operator deletes one, then the service restarts. Re-seeding would resurrect it,
	// which is a deletion that undoes itself and looks like the operator imagining things.
	if err := s.Delete(ctx, entries[0].EntryID, entries[0].Language, "alex"); err != nil {
		t.Fatal(err)
	}
	again, err := s.SeedFromCorpus(ctx, corpus)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("a restart re-seeded %d entries over an edited knowledge base", again)
	}
	after, err := s.List(ctx)
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
	if _, err := s.Publish(ctx, "alex", "the first one", 0); err != nil {
		t.Fatal(err)
	}
	_, revision, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}

	const marker = "wombats are dispatched on Thursdays"
	if _, err := s.Save(ctx, knowledge.Entry{
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

	version, err := s.Publish(ctx, "alex", "changed shipping", revision)
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
	if _, err := s.Publish(ctx, "alex", "the first one", 0); err != nil {
		t.Fatal(err)
	}

	_, revision, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, "alex", "first", revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, "dana", "from a page loaded before alex published", revision); !errors.Is(err, rag.ErrStaleActivation) {
		t.Errorf("a stale publication returned %v, want ErrStaleActivation", err)
	}
}

func TestAnEmptyKnowledgeBaseIsNotPublishable(t *testing.T) {
	ctx := context.Background()
	s, _ := store(t)
	seed(t, s)
	if _, err := s.Publish(ctx, "alex", "the first one", 0); err != nil {
		t.Fatal(err)
	}
	entries, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := s.Delete(ctx, e.EntryID, e.Language, "alex"); err != nil {
			t.Fatal(err)
		}
	}
	_, revision, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, "alex", "everything deleted", revision); !errors.Is(err, knowledge.ErrEmptyDraft) {
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
		if _, err := s.Save(ctx, c.entry, "alex"); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
	if _, err := s.Save(ctx, knowledge.Entry{EntryID: "e", Language: "en", Category: "c",
		Question: "q", Answer: "a"}, ""); err == nil {
		t.Error("an edit with no author was accepted")
	}
}

// activeAnswers returns every answer in the active corpus version, which is what a customer
// can be told.
func activeAnswers(ctx context.Context, corpus *rag.Store) (string, error) {
	rows, err := pool.Query(ctx, `
		SELECT answer FROM faq_document
		WHERE corpus_version = (SELECT version FROM corpus_active WHERE only_one)`)
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
