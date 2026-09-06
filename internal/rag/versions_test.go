package rag_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lai3d/ai-customer-service-go/internal/rag"
)

// unit returns a distinguishable unit vector. Distinct vectors matter here: pgvector's
// HNSW keeps one graph element per *distinct* vector with a list of heap tids, so a test
// built from duplicates measures the deduplication rather than the crowding it is about.
func unit(seed int, dims int) []float32 {
	v := make([]float32, dims)
	// A deterministic spread rather than random, so a failure is reproducible.
	x := uint32(seed*2654435761 + 1)
	var sum float64
	for i := range v {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		f := float32(x%1000)/1000 - 0.5
		v[i] = f
		sum += float64(f) * float64(f)
	}
	norm := float32(1.0)
	if sum > 0 {
		norm = float32(1 / (sum * sum))
		_ = norm
	}
	return v
}

func docsFor(version string, n int) ([]rag.Document, [][]float32) {
	docs := make([]rag.Document, n)
	vectors := make([][]float32, n)
	for i := 0; i < n; i++ {
		docs[i] = rag.Document{
			ID:       fmt.Sprintf("e%d:en", i),
			EntryID:  fmt.Sprintf("e%d", i),
			Language: "en",
			Category: "test",
			Question: fmt.Sprintf("question %d in %s", i, version),
			Answer:   fmt.Sprintf("answer %d in %s", i, version),
			Content:  fmt.Sprintf("passage: question %d in %s", i, version),
		}
		vectors[i] = unit(i*7919+len(version), 384)
	}
	return docs, vectors
}

// The bundled corpus becomes the first version by being stamped, not by being rebuilt.
// Re-embedding it would move the vectors every retrieval number in this pair was measured
// against, while claiming to preserve them.
func TestTheBundledCorpusIsAdoptedWithoutReEmbedding(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	before := embeddingsOf(t, ctx)
	if len(before) == 0 {
		t.Fatal("no corpus in the fixture")
	}

	adopted, err := f.store.AdoptBundled(ctx, "test-bundled-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetVersions(t) })
	if !adopted {
		t.Fatal("nothing was adopted")
	}

	after := embeddingsOf(t, ctx)
	if len(after) != len(before) {
		t.Fatalf("the corpus went from %d documents to %d", len(before), len(after))
	}
	for id, vector := range before {
		if after[id] != vector {
			t.Errorf("%s was re-embedded; its vector changed", id)
			break
		}
	}

	active, _, err := f.store.Active(ctx)
	if err != nil || active != "test-bundled-1" {
		t.Errorf("active version is %q (%v)", active, err)
	}

	// Twice is a no-op. It runs at every start-up, and a second adoption would stamp
	// published documents with the bundled version's name.
	again, err := f.store.AdoptBundled(ctx, "test-bundled-2")
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Error("a second adoption ran against an already-versioned database")
	}
}

func TestPublishingSwitchesInOneStepAndRefusesAStaleWriter(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.store.AdoptBundled(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetVersions(t) })

	_, revision, err := f.store.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}

	docs, vectors := docsFor("v2", 12)
	if err := f.store.Publish(ctx, "v2", docs, vectors, "alex", "first edit", revision); err != nil {
		t.Fatal(err)
	}
	active, newRevision, err := f.store.Active(ctx)
	if err != nil || active != "v2" {
		t.Fatalf("active is %q after publishing v2 (%v)", active, err)
	}
	if newRevision == revision {
		t.Error("the revision did not move, so a stale writer cannot be detected")
	}

	// A second operator publishing from the page they loaded before v2 existed.
	docs3, vectors3 := docsFor("v3", 12)
	err = f.store.Publish(ctx, "v3", docs3, vectors3, "dana", "concurrent edit", revision)
	if !errors.Is(err, rag.ErrStaleActivation) {
		t.Errorf("a stale publication returned %v, want ErrStaleActivation", err)
	}
	if active, _, _ := f.store.Active(ctx); active != "v2" {
		t.Errorf("the stale publication switched the active version to %q anyway", active)
	}

	// An empty publication would activate a corpus that answers nothing.
	if err := f.store.Publish(ctx, "v4", nil, nil, "alex", "", newRevision); err == nil {
		t.Error("an empty corpus was published")
	}
}

func TestRetrievalReadsOnlyTheActiveVersion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.store.AdoptBundled(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetVersions(t) })

	_, revision, err := f.store.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	docs, vectors := docsFor("only-here", 12)
	if err := f.store.Publish(ctx, "vnew", docs, vectors, "alex", "", revision); err != nil {
		t.Fatal(err)
	}

	// A question the bundled corpus answers well. With vnew active, the bundled documents
	// must not come back at all -- they are a retired version, not a fallback.
	passages, err := f.retriever.Retrieve(ctx, "How long do I have to return an item?")
	if err != nil {
		t.Fatal(err)
	}
	if len(passages) == 0 {
		t.Fatal("no passages at all")
	}
	for _, p := range passages {
		if p.Category != "test" {
			t.Errorf("a document from a retired version was retrieved: %s (%s)", p.EntryID, p.Category)
			break
		}
	}

	// Rolling back restores the previous corpus exactly.
	_, revision, err = f.store.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Activate(ctx, "base", "alex", revision); err != nil {
		t.Fatal(err)
	}
	passages, err = f.retriever.Retrieve(ctx, "How long do I have to return an item?")
	if err != nil {
		t.Fatal(err)
	}
	if len(passages) == 0 || passages[0].EntryID != "returns-window" {
		t.Errorf("after rolling back, the top passage is %v", passages)
	}
}

// Retention keeps names and deletes documents, so a rollback to a swept version has to be
// refused rather than silently activating an empty corpus.
func TestRetentionNeverStrandsTheActiveVersionOrAllowsAnEmptyRollback(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.store.AdoptBundled(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetVersions(t) })

	for _, v := range []string{"v2", "v3", "v4"} {
		_, revision, err := f.store.Active(ctx)
		if err != nil {
			t.Fatal(err)
		}
		docs, vectors := docsFor(v, 8)
		if err := f.store.Publish(ctx, v, docs, vectors, "alex", "", revision); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := f.store.Retain(ctx, 2); err != nil {
		t.Fatal(err)
	}

	// The active version keeps its documents whatever the retention count says.
	active, revision, err := f.store.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	passages, err := f.retriever.Retrieve(ctx, "question 1 in v4")
	if err != nil {
		t.Fatal(err)
	}
	if len(passages) == 0 {
		t.Errorf("retention emptied the active version %q", active)
	}

	// And rolling back to a swept version is refused rather than activated empty.
	err = f.store.Activate(ctx, "base", "alex", revision)
	if err == nil {
		t.Error("rolled back to a version whose documents were swept")
	}
	if got, _, _ := f.store.Active(ctx); got != active {
		t.Errorf("the refused rollback switched the active version to %q", got)
	}
}

func embeddingsOf(t *testing.T, ctx context.Context) map[string]string {
	t.Helper()
	rows, err := sharedPool.Query(ctx, `SELECT id, embedding::text FROM faq_document`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, vector string
		if err := rows.Scan(&id, &vector); err != nil {
			t.Fatal(err)
		}
		out[id] = vector
	}
	return out
}

// resetVersions puts the shared fixture back: the other tests in this package expect an
// unversioned corpus, and a version left active would silently change what they retrieve.
func resetVersions(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Published documents only, selected by their version's source rather than by the
	// shape of their id. The first version of this matched `id LIKE '%:%:%'`, which is
	// also what a bundled id looks like (`faq:returns-window:en`) -- so the cleanup
	// deleted the corpus, and three tests failed with "no active corpus version" while
	// the defect was in the tidying.
	for _, sql := range []string{
		`DELETE FROM faq_document WHERE corpus_version IN
		   (SELECT version FROM corpus_version WHERE source = 'published')`,
		`DELETE FROM corpus_active`,
		`DELETE FROM corpus_version`,
		`UPDATE faq_document SET corpus_version = NULL`,
	} {
		if _, err := sharedPool.Exec(ctx, sql); err != nil {
			t.Fatalf("could not reset the corpus versions: %v", err)
		}
	}
	// And the shared corpus is put back if a test legitimately swept it.
	//
	// That happens for a real reason rather than an accident: once the bundled corpus has
	// been superseded by published versions, it *is* a retired version, and Retain deletes
	// retired versions' documents. The retention test therefore destroys the fixture every
	// other test in this package measures retrieval against, and re-ingesting is the only
	// honest repair -- asserting the corpus survived would be asserting that retention does
	// not do its job.
	var documents int
	if err := sharedPool.QueryRow(ctx, `SELECT count(*) FROM faq_document`).Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if documents != 36 {
		if _, err := rag.Ingest(ctx, corpusPath, embedder, sharedStore); err != nil {
			t.Fatalf("could not restore the shared corpus: %v", err)
		}
	}
}

// A full page of results still comes back after twenty publications and a retention sweep.
//
// **This test does not justify `hnsw.iterative_scan`,** and saying so is the point: it
// passes with that setting and without it. It was written to justify the setting, could
// not, and is kept for the property it does measure -- that version churn plus retention
// does not quietly degrade retrieval -- rather than deleted or left carrying a claim it
// cannot support. See docs/knowledge.md for what was measured and what remains unexplained.
//
// The vectors are all distinct on purpose: pgvector's HNSW stores one graph element per
// distinct vector with a list of heap tids, so a version built by copying the same vectors
// costs one candidate for all its copies and measures the deduplication instead. That is
// the mistake that made an earlier probe here report 8 of 8 and mean nothing.
func TestAFilteredSearchStillReturnsAFullPageWithManyRetiredVersions(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if _, err := f.store.AdoptBundled(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetVersions(t) })

	// Twenty publications where every entry changes, which is the workload the Java
	// implementation measured at 1 of 8 with the setting off.
	const versions, perVersion = 20, 36
	for v := 0; v < versions; v++ {
		_, revision, err := f.store.Active(ctx)
		if err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("churn-%02d", v)
		docs := make([]rag.Document, perVersion)
		vectors := make([][]float32, perVersion)
		for i := range docs {
			docs[i] = rag.Document{
				ID: fmt.Sprintf("e%d:en", i), EntryID: fmt.Sprintf("e%d", i),
				Language: "en", Category: "churn",
				Question: fmt.Sprintf("q%d %s", i, name),
				Answer:   fmt.Sprintf("a%d %s", i, name),
				Content:  fmt.Sprintf("passage: q%d %s", i, name),
			}
			// Distinct per version as well as per entry: an entry whose text changed.
			vectors[i] = unit(v*1000+i, 384)
		}
		if err := f.store.Publish(ctx, name, docs, vectors, "alex", "", revision); err != nil {
			t.Fatal(err)
		}
	}

	// Retention, which is what turns retired documents into *dead tuples* rather than
	// merely filtered ones. That distinction is the whole measurement: a live row the
	// filter rejects is discarded by the executor, which can ask the index for more, while
	// a dead tuple is discarded inside the index scan and costs a candidate outright.
	// Autovacuum is disabled first so this is a measurement rather than a race with a
	// background daemon.
	if _, err := sharedPool.Exec(ctx,
		`ALTER TABLE faq_document SET (autovacuum_enabled = false)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sharedPool.Exec(context.Background(),
			`ALTER TABLE faq_document RESET (autovacuum_enabled)`)
	})
	if _, err := f.store.Retain(ctx, 2); err != nil {
		t.Fatal(err)
	}

	var live, total int
	if err := sharedPool.QueryRow(ctx, `SELECT count(*) FROM faq_document`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	active, _, err := f.store.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := sharedPool.QueryRow(ctx,
		`SELECT count(*) FROM faq_document WHERE corpus_version = $1`, active).Scan(&live); err != nil {
		t.Fatal(err)
	}

	// The search a customer makes: top 8 from the active version.
	passages, err := f.store.Search(ctx, unit(19*1000+3, 384), rag.SearchOptions{TopK: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(passages) < 8 {
		t.Errorf("a top-8 search returned %d passages with %d live documents of %d in the "+
			"index; version churn plus retention has degraded retrieval. Read `Rows "+
			"Removed by Filter` from EXPLAIN ANALYZE before concluding what fixed it.",
			len(passages), live, total)
	}
	for _, p := range passages {
		if p.Category != "churn" {
			t.Errorf("a retired document was returned: %s", p.ID)
			break
		}
	}
}
