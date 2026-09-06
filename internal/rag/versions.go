package rag

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

// Corpus versioning: what makes the knowledge base editable without making a customer's
// search race a publication.
//
// A version is built, then activated, and never edited in place. Live retrieval filters on
// the one active version, so a half-written version is invisible rather than half-visible,
// and activating is one row change that either happened or did not.
//
// The bundled corpus is adopted as the first version **without re-embedding**. That is not
// an optimisation: `corpus/faq.json` is byte-identical to the Java implementation's, its
// embeddings are what every retrieval number in the pair was measured against, and
// re-computing them would move the measurement while claiming to preserve it.

var (
	ErrNoActiveVersion = errors.New("no active corpus version")
	// ErrStaleActivation means somebody else activated a version since this caller read
	// which one was active. They publish again from a fresh read; nothing is overwritten.
	ErrStaleActivation = errors.New("the active version changed while this was being published")
)

type Version struct {
	Version   string    `json:"version"`
	Source    string    `json:"source"`
	Documents int       `json:"documents"`
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy string    `json:"createdBy"`
	Note      string    `json:"note,omitempty"`
	Active    bool      `json:"active"`
}

// Active returns the version live retrieval reads, and the revision to hand back when
// activating something else.
func (s *Store) Active(ctx context.Context) (version string, revision int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT version, revision FROM corpus_active WHERE only_one`).Scan(&version, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, ErrNoActiveVersion
	}
	return version, revision, err
}

func (s *Store) Versions(ctx context.Context) ([]Version, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT v.version, v.source, v.documents, v.created_at, v.created_by,
		       coalesce(v.note,''), (a.version IS NOT NULL)
		FROM corpus_version v
		LEFT JOIN corpus_active a ON a.version = v.version
		ORDER BY v.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.Version, &v.Source, &v.Documents, &v.CreatedAt,
			&v.CreatedBy, &v.Note, &v.Active); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// AdoptBundled makes the corpus already in the database the first version, without
// touching a single embedding.
//
// Idempotent, and it has to be: it runs at every start-up. On a database that already has
// an active version it does nothing at all -- a second adoption would stamp published
// documents with the bundled version's name.
func (s *Store) AdoptBundled(ctx context.Context, version string) (adopted bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var active string
	err = tx.QueryRow(ctx, `SELECT version FROM corpus_active WHERE only_one`).Scan(&active)
	if err == nil {
		return false, nil // already versioned; nothing to adopt
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}

	tag, err := tx.Exec(ctx,
		`UPDATE faq_document SET corpus_version = $1 WHERE corpus_version IS NULL`, version)
	if err != nil {
		return false, fmt.Errorf("stamp the bundled corpus: %w", err)
	}
	documents := int(tag.RowsAffected())
	if documents == 0 {
		// Nothing to adopt yet. Ingestion has not run, or the corpus is empty; either way
		// activating an empty version would make every search return nothing.
		return false, nil
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO corpus_version (version, source, documents, created_by, note)
		VALUES ($1, 'bundled', $2, 'system', 'the corpus shipped with this build, adopted without re-embedding')
		ON CONFLICT (version) DO UPDATE SET documents = EXCLUDED.documents`,
		version, documents); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO corpus_active (only_one, version, activated_by)
		VALUES (true, $1, 'system')`, version); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Publish writes a new version's documents and activates it, in one transaction.
//
// expectedRevision is what the caller read from Active. Two operators publishing from two
// stale pages is the ordinary case in a team, and the loser is told rather than silently
// overwritten.
//
// The documents are written before the switch and the switch is a single row: a customer
// searching during a publication reads the old version completely, then the new version
// completely, and never a mixture of the two.
func (s *Store) Publish(ctx context.Context, version string, docs []Document, vectors [][]float32,
	actor, note string, expectedRevision int) error {

	if len(docs) != len(vectors) {
		return fmt.Errorf("have %d documents and %d vectors", len(docs), len(vectors))
	}
	if len(docs) == 0 {
		// An empty version activates into a service that answers nothing from the corpus.
		return errors.New("refusing to publish an empty corpus")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	rows := make([][]any, len(docs))
	for i, d := range docs {
		rows[i] = []any{version + ":" + d.ID, d.EntryID, d.Language, d.Category,
			d.Question, d.Answer, d.Content, pgvector.NewVector(vectors[i]), version}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"faq_document"},
		[]string{"id", "entry_id", "language", "category", "question", "answer",
			"content", "embedding", "corpus_version"},
		pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("write version %s: %w", version, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO corpus_version (version, source, documents, created_by, note)
		VALUES ($1, 'published', $2, $3, NULLIF($4,''))`,
		version, len(docs), actor, note); err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, `
		UPDATE corpus_active SET version = $1, activated_at = now(), activated_by = $2,
		       revision = revision + 1
		WHERE only_one AND revision = $3`, version, actor, expectedRevision)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleActivation
	}
	return tx.Commit(ctx)
}

// Activate switches to a version that already exists. Rollback is this, pointed backwards.
func (s *Store) Activate(ctx context.Context, version, actor string, expectedRevision int) error {
	var documents int
	err := s.pool.QueryRow(ctx,
		`SELECT documents FROM corpus_version WHERE version = $1`, version).Scan(&documents)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("no version %q", version)
	}
	if err != nil {
		return err
	}
	// A retained version whose documents were swept is a name with nothing behind it.
	// Activating it is how a rollback turns an incident into an outage.
	var live int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM faq_document WHERE corpus_version = $1`, version).Scan(&live); err != nil {
		return err
	}
	if live == 0 {
		return fmt.Errorf("version %q has no documents left; it was retained by name only", version)
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE corpus_active SET version = $1, activated_at = now(), activated_by = $2,
		       revision = revision + 1
		WHERE only_one AND revision = $3`, version, actor, expectedRevision)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleActivation
	}
	return nil
}

// Retain deletes the documents of every version except the newest keep, never touching the
// active one.
//
// The version rows survive: a name with a date and an author is worth keeping after its
// documents are gone, and Activate refuses one whose documents went with it rather than
// silently activating an empty corpus.
func (s *Store) Retain(ctx context.Context, keep int) (int64, error) {
	if keep < 1 {
		keep = 1
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM faq_document
		WHERE corpus_version IS NOT NULL
		  AND corpus_version NOT IN (
			SELECT version FROM corpus_version ORDER BY created_at DESC LIMIT $1)
		  AND corpus_version <> (SELECT version FROM corpus_active WHERE only_one)`, keep)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
