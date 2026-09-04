package rag

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Ingest embeds the corpus and replaces what is in the store.
//
// It runs at startup. That is cheap here — 36 documents, a few hundred milliseconds —
// and it keeps the deployed corpus and the file in the repository from drifting apart.
// A corpus large enough for that to hurt would want an offline indexing job instead.
func Ingest(ctx context.Context, corpusPath string, embedder Embedder, store *Store) (int, error) {
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

	if err := store.Replace(ctx, docs, vectors); err != nil {
		return 0, err
	}
	slog.Info("ingested FAQ corpus",
		"documents", len(docs), "entries", len(corpus.Entries),
		"version", corpus.Version, "embed_duration", embedded.Round(time.Millisecond))
	return len(docs), nil
}
