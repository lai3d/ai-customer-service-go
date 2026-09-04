package rag

import (
	"context"
	"fmt"
	"log/slog"
)

// Retriever is the whole retrieval path: embed the question, search, return passages.
//
// It is a plain struct with one method rather than a chain of composable advisors.
// Spring AI's QuestionAnswerAdvisor also rewrote the user's message to carry the
// passages, which is what made advisor ordering a correctness constraint; here
// retrieval returns passages and the caller decides what to do with them, so the
// ordering hazard is gone by construction rather than pinned by a test.
type Retriever struct {
	embedder Embedder
	store    *Store
	opts     SearchOptions
}

func NewRetriever(embedder Embedder, store *Store, topK int, threshold float64) *Retriever {
	return &Retriever{
		embedder: embedder,
		store:    store,
		opts:     SearchOptions{TopK: topK, Threshold: threshold},
	}
}

// Retrieve returns the passages most similar to the question.
//
// The threshold this applies is a floor for degenerate input, not a relevance filter.
// With e5 the relevant and off-topic score distributions are about 0.006 apart, so no
// threshold separates them; judging relevance is the model's job, and the system prompt
// tells it that some of what it is given will be unrelated. See docs/retrieval.md.
func (r *Retriever) Retrieve(ctx context.Context, question string) ([]Passage, error) {
	vector, err := r.embedder.EmbedQuery(ctx, question)
	if err != nil {
		return nil, fmt.Errorf("embed question: %w", err)
	}
	passages, err := r.store.Search(ctx, vector, r.opts)
	if err != nil {
		return nil, err
	}
	// The question itself is deliberately absent from this log line, as it is from the
	// spans. A support question is often the most sensitive thing in the request, and
	// logs and traces are retained and read widely.
	slog.Debug("retrieved passages", "count", len(passages), "top_k", r.opts.TopK)
	return passages, nil
}

// RetrieveIn is Retrieve restricted to one language, for cross-lingual measurement.
func (r *Retriever) RetrieveIn(ctx context.Context, question, language string) ([]Passage, error) {
	vector, err := r.embedder.EmbedQuery(ctx, question)
	if err != nil {
		return nil, fmt.Errorf("embed question: %w", err)
	}
	opts := r.opts
	opts.Language = language
	return r.store.Search(ctx, vector, opts)
}
