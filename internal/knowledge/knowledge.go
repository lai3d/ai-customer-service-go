// Package knowledge is the editing side of the corpus: what operators change, and how a
// change becomes something customers are answered from.
//
// Nothing here is retrievable. Entries are drafts; a publication turns the drafts into a
// corpus version, embeds them, and activates it in one step (internal/rag). That separation
// is the whole design: an edit that took effect the moment it was saved would put a
// half-finished answer in front of a customer, and a publication that edited the live
// corpus in place would let a search read a corpus that was mid-write.
package knowledge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/rag"
)

// MaxAnswerLength bounds what one entry can put into a prompt.
//
// A knowledge base that many people can edit is an input the model reads, and the length
// of that input is the one property that can be bounded without judging its content. It is
// generous for an answer and small compared with the context.
const MaxAnswerLength = 4000

var (
	ErrNotFound   = errors.New("no such entry")
	ErrEmptyDraft = errors.New("refusing to publish an empty knowledge base")
)

type Entry struct {
	EntryID   string    `json:"entryId"`
	Language  string    `json:"language"`
	Category  string    `json:"category"`
	Question  string    `json:"question"`
	Answer    string    `json:"answer"`
	Deleted   bool      `json:"deleted"`
	UpdatedAt time.Time `json:"updatedAt"`
	UpdatedBy string    `json:"updatedBy"`
}

func (e Entry) validate() error {
	switch {
	case strings.TrimSpace(e.EntryID) == "":
		return errors.New("an entry needs an id")
	case strings.TrimSpace(e.Language) == "":
		return errors.New("an entry needs a language")
	case strings.TrimSpace(e.Question) == "":
		return errors.New("an entry needs a question")
	case strings.TrimSpace(e.Answer) == "":
		return errors.New("an entry needs an answer")
	case len(e.Answer) > MaxAnswerLength:
		return fmt.Errorf("the answer is %d characters; the limit is %d",
			len(e.Answer), MaxAnswerLength)
	}
	return nil
}

// Embedder is the part of internal/rag a publication needs.
type Embedder interface {
	EmbedPassages(ctx context.Context, texts []string) ([][]float32, error)
}

type Store struct {
	pool     *pgxpool.Pool
	corpus   *rag.Store
	embedder Embedder
}

func NewStore(pool *pgxpool.Pool, corpus *rag.Store, embedder Embedder) *Store {
	return &Store{pool: pool, corpus: corpus, embedder: embedder}
}

// SeedFromCorpus copies the bundled corpus into the drafts, once.
//
// Without this the editor opens on an empty list, and the first publication replaces the
// whole knowledge base with whatever one operator typed. That is not a hypothetical
// mistake: it is what "publish the drafts" means when the drafts are empty, and the person
// making it would have no way to know until customers stopped being answered.
//
// Idempotent by count rather than by flag: if there are any drafts, somebody has edited,
// and re-seeding would resurrect entries they deleted.
func (s *Store) SeedFromCorpus(ctx context.Context, corpus rag.Corpus) (int, error) {
	var existing int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_entry`).Scan(&existing); err != nil {
		return 0, err
	}
	if existing > 0 {
		return 0, nil
	}

	batch := &pgx.Batch{}
	for _, entry := range corpus.Entries {
		for _, l := range entry.Localized {
			batch.Queue(`
				INSERT INTO knowledge_entry
					(entry_id, language, category, question, answer, updated_by)
				VALUES ($1,$2,$3,$4,$5,'system')
				ON CONFLICT (entry_id, language) DO NOTHING`,
				entry.ID, l.Language, entry.Category, l.Question, l.Answer)
		}
	}
	if batch.Len() == 0 {
		return 0, nil
	}
	if err := s.pool.SendBatch(ctx, batch).Close(); err != nil {
		return 0, fmt.Errorf("seed the knowledge base: %w", err)
	}
	return batch.Len(), nil
}

// List returns the drafts. Deleted entries are included, marked: an operator needs to see
// that an entry is gone in order to bring it back, and a list that hides them makes a
// deletion look like the entry never existed.
func (s *Store) List(ctx context.Context) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT entry_id, language, category, question, answer, deleted, updated_at, updated_by
		FROM knowledge_entry ORDER BY entry_id, language`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.EntryID, &e.Language, &e.Category, &e.Question, &e.Answer,
			&e.Deleted, &e.UpdatedAt, &e.UpdatedBy); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Save creates or replaces one draft entry. Returns what the entry looked like before, so
// the caller can put the change in the audit trail: "alex edited returns-window" records
// that something happened, and not what.
func (s *Store) Save(ctx context.Context, e Entry, actor string) (before *Entry, err error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(actor) == "" {
		return nil, errors.New("an edit needs an author")
	}

	var prev Entry
	err = s.pool.QueryRow(ctx, `
		SELECT entry_id, language, category, question, answer, deleted, updated_at, updated_by
		FROM knowledge_entry WHERE entry_id = $1 AND language = $2`, e.EntryID, e.Language).
		Scan(&prev.EntryID, &prev.Language, &prev.Category, &prev.Question, &prev.Answer,
			&prev.Deleted, &prev.UpdatedAt, &prev.UpdatedBy)
	switch {
	case err == nil:
		before = &prev
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO knowledge_entry
			(entry_id, language, category, question, answer, deleted, updated_at, updated_by)
		VALUES ($1,$2,$3,$4,$5,false,now(),$6)
		ON CONFLICT (entry_id, language) DO UPDATE SET
			category = EXCLUDED.category, question = EXCLUDED.question,
			answer = EXCLUDED.answer, deleted = false,
			updated_at = now(), updated_by = EXCLUDED.updated_by`,
		e.EntryID, e.Language, e.Category, e.Question, e.Answer, actor); err != nil {
		return nil, err
	}
	return before, nil
}

// Delete marks an entry deleted. Soft, because a published version was built from it and a
// hard delete would make the record of what shipped incomplete.
func (s *Store) Delete(ctx context.Context, entryID, language, actor string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE knowledge_entry SET deleted = true, updated_at = now(), updated_by = $3
		WHERE entry_id = $1 AND language = $2 AND NOT deleted`, entryID, language, actor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Publish embeds the live drafts and activates them as a new corpus version.
//
// The version name is a timestamp rather than a counter: a counter needs a source of truth
// for "the last one", and two replicas publishing at once would each believe they had it.
func (s *Store) Publish(ctx context.Context, actor, note string, expectedRevision int) (string, error) {
	entries, err := s.List(ctx)
	if err != nil {
		return "", err
	}
	var docs []rag.Document
	var texts []string
	for _, e := range entries {
		if e.Deleted {
			continue
		}
		docs = append(docs, rag.Document{
			ID:       fmt.Sprintf("%s:%s", e.EntryID, e.Language),
			EntryID:  e.EntryID,
			Language: e.Language,
			Category: e.Category,
			Question: e.Question,
			Answer:   e.Answer,
			// The same shape the bundled corpus builds, so a published version and the
			// bundled one are the same kind of thing to the embedder and the retriever.
			Content: fmt.Sprintf("Q: %s\nA: %s", e.Question, e.Answer),
		})
		texts = append(texts, docs[len(docs)-1].Content)
	}
	if len(docs) == 0 {
		return "", ErrEmptyDraft
	}

	vectors, err := s.embedder.EmbedPassages(ctx, texts)
	if err != nil {
		return "", fmt.Errorf("embed the knowledge base: %w", err)
	}

	// A timestamp for a human plus randomness for uniqueness.
	//
	// The timestamp alone is not a name: two publications inside the same second produce
	// the same one and collide on the document primary key with a raw constraint
	// violation. That is not hypothetical -- it is a double-clicked Publish button, and
	// the first version of this hit it in a test within a minute of being written.
	// Relying on the clock's resolution for uniqueness is the same mistake as relying on
	// a process-local counter, which this repository has already made once.
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	version := fmt.Sprintf("%s-%s",
		time.Now().UTC().Format("2006-01-02T15-04-05Z"), hex.EncodeToString(suffix))
	if err := s.corpus.Publish(ctx, version, docs, vectors, actor, note, expectedRevision); err != nil {
		return "", err
	}
	return version, nil
}

// Versions and Activate are pass-throughs to internal/rag.
//
// They exist so the admin surface talks to one package about knowledge rather than two,
// and so nothing in internal/admin has to know that a corpus version is a rag concept.
func (s *Store) Versions(ctx context.Context) ([]rag.Version, error) {
	return s.corpus.Versions(ctx)
}

func (s *Store) Activate(ctx context.Context, version, actor string, expectedRevision int) error {
	return s.corpus.Activate(ctx, version, actor, expectedRevision)
}

// State is what the editor needs before it can publish: which version is live, and the
// revision to hand back so a stale page loses the race instead of winning it.
func (s *Store) State(ctx context.Context) (version string, revision int, err error) {
	version, revision, err = s.corpus.Active(ctx)
	if errors.Is(err, rag.ErrNoActiveVersion) {
		// Reachable before the first ingestion. The editor shows it rather than failing:
		// "no version is active" is a state an operator can act on.
		return "", 0, nil
	}
	return version, revision, err
}

// HasUnpublishedChanges reports whether any draft is newer than the active version.
//
// True after an ordinary edit, and also after a *rollback*, which is the case worth having
// it for: rolling back changes what customers are told and leaves the drafts untouched, so
// the editor shows text that is not live and nothing else would say so.
//
// A timestamp comparison rather than a content diff. It over-reports -- saving an entry
// unchanged sets updated_at -- and over-reporting is the safe direction: it says "publish
// to be sure", never "nothing to do" when there is.
func (s *Store) HasUnpublishedChanges(ctx context.Context) (bool, error) {
	var newer bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM knowledge_entry k
			WHERE k.updated_at > coalesce(
				(SELECT v.created_at FROM corpus_version v
				 JOIN corpus_active a ON a.version = v.version AND a.only_one),
				'-infinity'::timestamptz))`).Scan(&newer)
	return newer, err
}
