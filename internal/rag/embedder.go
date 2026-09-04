package rag

import "context"

// Embedder turns text into vectors.
//
// The interface has two methods rather than one, and that is the whole point. e5 models
// are trained with asymmetric input markers — "query: " before a search query,
// "passage: " before an indexed document — and they are part of the model contract, not
// decoration. Applying them to one side only is measurably worse than applying neither.
//
// Java's implementation wrapped the embedding model in a decorator that inferred which
// case it was in from which overload the vector store happened to call. Here the two
// cases are separate methods, so a caller cannot embed a query as a passage without
// writing something that obviously looks wrong. There is no Embed(text) to misuse.
type Embedder interface {
	// EmbedQuery embeds one search query.
	EmbedQuery(ctx context.Context, query string) ([]float32, error)
	// EmbedPassages embeds documents for indexing, in one batch.
	EmbedPassages(ctx context.Context, passages []string) ([][]float32, error)
	Dimensions() int
	Close() error
}
