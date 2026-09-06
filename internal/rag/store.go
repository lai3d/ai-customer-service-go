package rag

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// Passage is one retrieved document and how well it matched.
type Passage struct {
	Document
	// Score is cosine similarity in [-1, 1]: 1 - the distance pgvector returns.
	Score float64
}

// Store holds the corpus vectors.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Replace writes the corpus, discarding whatever was there before, in one transaction.
//
// Appending instead is the obvious bug and it is not merely wasteful: duplicates crowd
// out distinct passages inside the top-k window, so the model sees one answer four
// times instead of four different ones.
func (s *Store) Replace(ctx context.Context, docs []Document, vectors [][]float32) error {
	if len(docs) != len(vectors) {
		return fmt.Errorf("have %d documents and %d vectors", len(docs), len(vectors))
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// TRUNCATE, not DELETE, and this is a correctness fix rather than a speed one.
	//
	// DELETE leaves the old rows in the HNSW index as dead entries. An HNSW scan is
	// approximate: it collects hnsw.ef_search candidates from the graph and only then
	// drops the ones whose heap tuples are dead, so after enough reloads the candidates
	// are mostly dead and `ORDER BY embedding <=> $1 LIMIT 8` quietly returns fewer than
	// eight live rows -- or none. Reproduced here on pgvector 0.8.6 with autovacuum off:
	// thirty DELETE-and-reinsert cycles over the same 36 rows, and an index scan returned
	// 2 of 8 while a sequential scan returned 8 and a VACUUM restored the index. With
	// autovacuum on it is a race with the daemon, which is exactly the kind of failure
	// that shows up in one deployment in four and never on a laptop.
	//
	// TRUNCATE rebuilds the index empty. Readers block on its ACCESS EXCLUSIVE lock until
	// the new rows are committed, which for a 36-document load is the right trade: a
	// reader that waits is better than a reader that silently retrieves nothing.
	//
	// Found by the .NET implementation of this system, which shares the shape. This
	// repository's own tests never met it because they ingest once per package.
	if _, err := tx.Exec(ctx, `TRUNCATE faq_document`); err != nil {
		return fmt.Errorf("clear corpus: %w", err)
	}
	rows := make([][]any, len(docs))
	for i, d := range docs {
		rows[i] = []any{d.ID, d.EntryID, d.Language, d.Category, d.Question, d.Answer,
			d.Content, pgvector.NewVector(vectors[i])}
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"faq_document"},
		[]string{"id", "entry_id", "language", "category", "question", "answer", "content", "embedding"},
		pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("insert corpus: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM faq_document`).Scan(&n)
	return n, err
}

// SearchOptions narrows a search. Language exists for one reason: on the full corpus,
// same-language matches score high enough that every Chinese passage outranks every
// English one, so cross-lingual retrieval is invisible. Filtering to the other language
// is how you find out whether it works at all — which is what matters for an entry
// nobody has translated yet.
type SearchOptions struct {
	TopK      int
	Threshold float64
	Language  string
	// Version searches a specific corpus version instead of the active one. Empty means
	// the active one, which is what every customer-facing search uses; naming a version is
	// how an operator previews a draft before activating it.
	Version string
}

// Search reads the active corpus version only.
//
// The version predicate is a *post-filter* on an HNSW scan: the index walks the graph,
// collects candidates, and only then discards the ones from other versions. With several
// versions retained, candidates spent on retired documents are candidates not spent on live
// ones, and a LIMIT 8 search starts returning fewer than eight. That is why
// `hnsw.iterative_scan` is set on every pooled connection -- see internal/store, and
// docs/knowledge.md for the measurement.
//
// `corpus_version IS NULL` is deliberate and is what makes this change safe to deploy: a
// database whose corpus has not been adopted yet -- during a rollout, or in a test that
// ingests without versioning -- keeps working exactly as before.
func (s *Store) Search(ctx context.Context, query []float32, opts SearchOptions) ([]Passage, error) {
	const sql = `
		SELECT id, entry_id, language, category, question, answer, content,
		       1 - (embedding <=> $1) AS score
		FROM faq_document
		WHERE ($2 = '' OR language = $2)
		  AND 1 - (embedding <=> $1) >= $3
		  AND (corpus_version IS NULL
		       OR corpus_version = coalesce($5, (SELECT version FROM corpus_active WHERE only_one)))
		ORDER BY embedding <=> $1
		LIMIT $4`

	var version any
	if opts.Version != "" {
		version = opts.Version
	}
	rows, err := s.pool.Query(ctx, sql,
		pgvector.NewVector(query), opts.Language, opts.Threshold, opts.TopK, version)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}
	defer rows.Close()

	var passages []Passage
	for rows.Next() {
		var p Passage
		if err := rows.Scan(&p.ID, &p.EntryID, &p.Language, &p.Category,
			&p.Question, &p.Answer, &p.Content, &p.Score); err != nil {
			return nil, err
		}
		passages = append(passages, p)
	}
	return passages, rows.Err()
}
