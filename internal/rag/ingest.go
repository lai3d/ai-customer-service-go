package rag

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Ingest embeds the corpus and replaces what is in the store, for one tenant.
//
// It runs at startup. That is cheap here — 36 documents, a few hundred milliseconds —
// and it keeps the deployed corpus and the file in the repository from drifting apart.
// A corpus large enough for that to hurt would want an offline indexing job instead.
//
// The tenant is the bundled corpus's owner. It is not a template for other tenants: a new
// tenant starts with nothing and gets grounded refusals until somebody publishes to it,
// which is the correct default — this corpus is one company's returns policy.
func Ingest(ctx context.Context, tenantID, corpusPath string, embedder Embedder, store *Store) (int, error) {
	corpus, err := LoadCorpus(corpusPath)
	if err != nil {
		return 0, err
	}
	docs := corpus.Documents()

	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = d.Content
	}

	started := time.Now()
	vectors, err := embedder.EmbedPassages(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("embed corpus: %w", err)
	}
	embedded := time.Since(started)

	if err := store.Replace(ctx, tenantID, docs, vectors); err != nil {
		return 0, err
	}
	slog.Info("ingested FAQ corpus",
		"documents", len(docs), "entries", len(corpus.Entries),
		"version", corpus.Version, "tenant", tenantID,
		"embed_duration", embedded.Round(time.Millisecond))
	return len(docs), nil
}
