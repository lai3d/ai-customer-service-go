package rag_test

import (
	"context"
	"testing"

	"github.com/lai3d/ai-customer-service-go/internal/rag"
)

// Reloading the corpus must not degrade retrieval, and with DELETE it did.
//
// An HNSW scan is approximate: it collects hnsw.ef_search candidates from the graph and
// only afterwards drops the ones whose heap tuples are dead. DELETE leaves every previous
// generation of the corpus in the index as dead entries, so after enough restarts the
// candidates are mostly dead and a LIMIT 8 search returns fewer than eight live rows --
// or none at all, with no error anywhere and a perfectly healthy-looking service.
//
// Autovacuum normally hides this, which is what makes it dangerous: it is a race with a
// background daemon, so it appears in the deployment that restarts often and never on a
// laptop. The table's autovacuum is disabled here to remove the race, which is the only
// way this test says the same thing twice.
//
// The defect was found in the .NET implementation of this system and reported across; it
// was reproduced here in psql before this test was written, and this test was red on the
// DELETE version of Store.Replace.
func TestRetrievalSurvivesManyCorpusReloads(t *testing.T) {
	if testing.Short() {
		t.Skip("sixty corpus reloads")
	}
	ctx := context.Background()
	f := newFixture(t)

	if _, err := sharedPool.Exec(ctx,
		`ALTER TABLE faq_document SET (autovacuum_enabled = false)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := sharedPool.Exec(context.Background(),
			`ALTER TABLE faq_document RESET (autovacuum_enabled)`); err != nil {
			t.Errorf("could not re-enable autovacuum: %v", err)
		}
		// Leave the shared corpus as the other tests expect to find it.
		if _, err := rag.Ingest(context.Background(), f.corpus, f.embedder, f.store); err != nil {
			t.Errorf("could not restore the corpus: %v", err)
		}
	})

	// Sixty restarts' worth of ingestion, and the number is measured rather than chosen:
	// thirty was not enough to make this test fail on the DELETE version, which made it a
	// test that could not tell the two apart. Sixty separate transactions reproduce it in
	// psql and here. The real service ingests once per process start, so this is a month
	// of ordinary rollouts between autovacuum runs, not an abusive loop.
	for i := 0; i < 60; i++ {
		if _, err := rag.Ingest(ctx, f.corpus, f.embedder, f.store); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}

	live, err := f.store.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	passages, err := f.retriever.Retrieve(ctx, "How long do I have to return an item?")
	if err != nil {
		t.Fatal(err)
	}
	if len(passages) < 8 {
		t.Errorf("after 60 reloads a search returned %d passages from a corpus of %d live "+
			"rows; the index is full of dead entries and retrieval is silently degraded",
			len(passages), live)
	}
}
