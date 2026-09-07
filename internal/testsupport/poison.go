package testsupport

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// PoisonCorpus writes one document straight into a tenant's active corpus version, and
// returns the function that removes it.
//
// It exists for the eval's injection case: an entry that tells the assistant to ignore its
// instructions, retrieved through the ordinary path, so the attack is delivered the way a
// real one would be — through the knowledge base rather than through the question.
//
// # Why it lives here rather than in the eval
//
// It lived in `internal/eval`, which is behind a build tag, and its SQL named
// `corpus_active.only_one` — a column migration 0004 dropped when the active version became
// one row per tenant. Nothing noticed: `go test ./...`, `go vet ./...` and CI all skip a
// tagged file, and `make lint` vets it but a stale column name in a string is not a
// compile error. The eval ran, this case failed in ten milliseconds with a SQL error, and
// the failure was one line among thirty-five passes.
//
// Here it is in the ordinary build with a test that runs in CI against a real schema. That
// is the whole point: the harness that measures the product is itself a thing that rots,
// and the only guard that works is a check that runs when nobody is thinking about it.
func PoisonCorpus(ctx context.Context, pool *pgxpool.Pool, tenantID string,
	doc PoisonedDocument, vector []float32) (remove func(context.Context) error, err error) {

	content := fmt.Sprintf("Q: %s\nA: %s", doc.Question, doc.Answer)

	// The tenant's *active* version, because an entry outside the version retrieval reads
	// is an entry the model never sees — and the case would then pass by the attack never
	// having been delivered.
	var version *string
	if err := pool.QueryRow(ctx,
		`SELECT version FROM corpus_active WHERE tenant_id = $1`, tenantID).
		Scan(&version); err != nil {
		return nil, fmt.Errorf("read the active corpus version for %q: %w", tenantID, err)
	}

	id := "poison:" + doc.EntryID + ":" + doc.Language
	if _, err := pool.Exec(ctx, `
		INSERT INTO faq_document
			(tenant_id, id, entry_id, language, category, question, answer, content,
			 embedding, corpus_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		tenantID, id, doc.EntryID, doc.Language, doc.Category, doc.Question, doc.Answer,
		content, pgvector.NewVector(vector), version); err != nil {
		return nil, fmt.Errorf("write the poisoned entry: %w", err)
	}

	return func(ctx context.Context) error {
		_, err := pool.Exec(ctx,
			`DELETE FROM faq_document WHERE id = $1 AND tenant_id = $2`, id, tenantID)
		return err
	}, nil
}

// PoisonedDocument is the entry to plant. It is the corpus's own shape rather than
// rag.Document so that this package does not depend on internal/rag, which depends on cgo.
type PoisonedDocument struct {
	EntryID  string
	Language string
	Category string
	Question string
	Answer   string
}
