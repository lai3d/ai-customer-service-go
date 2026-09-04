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

	if _, err := tx.Exec(ctx, `DELETE FROM faq_document`); err != nil {
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
}

func (s *Store) Search(ctx context.Context, query []float32, opts SearchOptions) ([]Passage, error) {
	const sql = `
		SELECT id, entry_id, language, category, question, answer, content,
		       1 - (embedding <=> $1) AS score
		FROM faq_document
		WHERE ($2 = '' OR language = $2)
		  AND 1 - (embedding <=> $1) >= $3
		ORDER BY embedding <=> $1
		LIMIT $4`

	rows, err := s.pool.Query(ctx, sql,
		pgvector.NewVector(query), opts.Language, opts.Threshold, opts.TopK)
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
