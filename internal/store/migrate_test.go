package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"github.com/lai3d/ai-customer-service-go/internal/store"
)

// files builds a migration set in memory. The runner takes an fs.FS precisely so these
// tests can drive it with migrations that do not exist in the repository: a mechanism
// tested only against its own one real file is a mechanism whose second migration is
// still unproven.
func files(pairs ...string) fstest.MapFS {
	out := fstest.MapFS{}
	for i := 0; i < len(pairs); i += 2 {
		out["migrations/"+pairs[i]] = &fstest.MapFile{Data: []byte(pairs[i+1])}
	}
	return out
}

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func ledger(t *testing.T, conn *pgx.Conn) map[int]string {
	t.Helper()
	rows, err := conn.Query(context.Background(),
		`SELECT version, name FROM schema_migration ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var v int
		var name string
		if err := rows.Scan(&v, &name); err != nil {
			t.Fatal(err)
		}
		out[v] = name
	}
	return out
}

// The baseline is adopted by a database that already has the schema and no ledger.
//
// That is the whole of this item, and the first version of this test missed it: it called
// Open twice, and the second call skipped the baseline because the first had recorded it.
// It passed with the IF NOT EXISTS removed from the baseline -- proving that a restart is
// a no-op, which nobody doubted, and saying nothing about adoption.
//
// The case that matters is the one every running deployment is in: the schema exists
// because the previous start-up path created it, and schema_migration does not exist at
// all. The baseline is idempotent by construction, so it is simply applied -- no marker
// row, no --baseline flag, nothing to get wrong -- and the rows that were already there
// are still there afterwards.
func TestADatabaseWithTheSchemaAndNoLedgerAdoptsTheBaseline(t *testing.T) {
	ctx := context.Background()
	url := coldDatabase(t)
	conn := connect(t, url)

	// The state of a service that predates this change: the schema applied straight from
	// the file, exactly as the old start-up path applied the constant.
	embedded, err := store.ReadMigrations(store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	baseline := strings.ReplaceAll(embedded[0].SQL, ":dimensions", "384")
	if _, err := conn.Exec(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO chat_memory (conversation_id, role, content) VALUES ('c1','user','hello')`); err != nil {
		t.Fatal(err)
	}

	ran, err := store.Migrate(ctx, conn, store.Migrations, 384)
	if err != nil {
		t.Fatalf("adopting an existing schema failed: %v", err)
	}
	if ran != len(embedded) {
		t.Errorf("%d migrations ran against an existing schema, want %d", ran, len(embedded))
	}

	var rows int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM chat_memory WHERE conversation_id = 'c1'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("the row written before the migration ran is gone: %d rows", rows)
	}
	if applied := ledger(t, conn); applied[1] != "baseline" {
		t.Errorf("version 1 is recorded as %q: %v", applied[1], applied)
	}

	// And the restart after that does nothing at all.
	if ran, err := store.Migrate(ctx, conn, store.Migrations, 384); err != nil || ran != 0 {
		t.Errorf("a restart applied %d migrations (%v)", ran, err)
	}
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM schema_migration`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(embedded) {
		t.Errorf("%d ledger rows for %d migrations: one ran twice", count, len(embedded))
	}
}

// Open is the path a replica actually takes, and it has to survive being run twice
// against the same database -- which is every restart.
func TestOpeningTwiceKeepsTheDataAndTheLedger(t *testing.T) {
	ctx := context.Background()
	url := coldDatabase(t)

	pool, err := store.Open(ctx, url, 2, 384)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO chat_memory (conversation_id, role, content) VALUES ('c1','user','hello')`); err != nil {
		t.Fatal(err)
	}
	pool.Close()

	pool, err = store.Open(ctx, url, 2, 384)
	if err != nil {
		t.Fatalf("a second start against an existing schema failed: %v", err)
	}
	defer pool.Close()

	var rows, recorded int
	if err := pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM chat_memory WHERE conversation_id = 'c1'),
			(SELECT count(*) FROM schema_migration)`).Scan(&rows, &recorded); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("the row from the first start is gone: %d rows", rows)
	}
	if recorded != 1 {
		t.Errorf("%d ledger rows after two starts", recorded)
	}
}

// The checksum is the reason the ledger holds one. A version number records that a file
// with that number ran; only the checksum records that it was this file -- and editing an
// applied migration is what somebody does when writing another one feels like overkill.
func TestEditingAnAppliedMigrationStopsTheService(t *testing.T) {
	ctx := context.Background()
	conn := connect(t, coldDatabase(t))

	first := files("0001_baseline.sql", `CREATE TABLE widget (id INT PRIMARY KEY)`)
	if ran, err := store.Migrate(ctx, conn, first, 384); err != nil || ran != 1 {
		t.Fatalf("first run: %d applied, %v", ran, err)
	}

	edited := files("0001_baseline.sql",
		`CREATE TABLE widget (id INT PRIMARY KEY, colour TEXT)`)
	_, err := store.Migrate(ctx, conn, edited, 384)
	if !errors.Is(err, store.ErrChecksumChanged) {
		t.Fatalf("an edited migration returned %v", err)
	}
	// The message has to name the file, because the person reading it is deciding
	// between reverting an edit and writing a new migration.
	if !strings.Contains(err.Error(), "0001_baseline") {
		t.Errorf("the error does not say which migration: %v", err)
	}
}

// Two branches both adding 0007 and merging is the ordinary way this happens. The
// database that ran one of them would silently skip the other; a developer machine that
// had seen neither would run both. Both are defensible, which is the problem.
func TestAMigrationNumberedBelowOneAlreadyAppliedIsRefused(t *testing.T) {
	ctx := context.Background()
	conn := connect(t, coldDatabase(t))

	ahead := files(
		"0001_baseline.sql", `CREATE TABLE widget (id INT PRIMARY KEY)`,
		"0003_later.sql", `ALTER TABLE widget ADD COLUMN colour TEXT`)
	if ran, err := store.Migrate(ctx, conn, ahead, 384); err != nil || ran != 2 {
		t.Fatalf("setup: %d applied, %v", ran, err)
	}

	late := files(
		"0001_baseline.sql", `CREATE TABLE widget (id INT PRIMARY KEY)`,
		"0002_arrived_late.sql", `ALTER TABLE widget ADD COLUMN size TEXT`,
		"0003_later.sql", `ALTER TABLE widget ADD COLUMN colour TEXT`)
	_, err := store.Migrate(ctx, conn, late, 384)
	if !errors.Is(err, store.ErrOutOfOrder) {
		t.Fatalf("a migration numbered below one already applied returned %v", err)
	}

	var size int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'widget' AND column_name = 'size'`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if size != 0 {
		t.Error("the out-of-order migration was applied before being refused")
	}
}

// A migration that cannot run leaves nothing behind.
//
// **What this actually demonstrates is Postgres's implicit transaction**, not the explicit
// one in applyOne: a multi-statement simple query is wrapped by the server, so the first
// CREATE TABLE rolls back when the second fails whether or not this code opens a
// transaction at all. Verified by removing the transaction and watching this test stay
// green, which is why it says so instead of claiming otherwise. It is kept because the
// property is worth pinning -- the next start-up is a retry rather than a repair -- and
// the test below covers the half this one cannot reach.
func TestAFailingMigrationLeavesNeitherTheChangeNorTheRecord(t *testing.T) {
	ctx := context.Background()
	conn := connect(t, coldDatabase(t))

	broken := files("0001_half.sql", `
		CREATE TABLE widget (id INT PRIMARY KEY);
		CREATE TABLE widget (id INT PRIMARY KEY);`)
	if _, err := store.Migrate(ctx, conn, broken, 384); err == nil {
		t.Fatal("a migration that cannot run reported success")
	}

	var tables, recorded int
	if err := conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM information_schema.tables WHERE table_name = 'widget'),
			(SELECT count(*) FROM schema_migration)`).Scan(&tables, &recorded); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Error("the first half of a failed migration is still in the database")
	}
	if recorded != 0 {
		t.Errorf("%d ledger rows for a migration that failed", recorded)
	}

	// And the fixed version applies cleanly, which is what makes the rollback worth
	// having: the next start-up is a retry rather than a repair.
	fixed := files("0001_half.sql", `CREATE TABLE widget (id INT PRIMARY KEY)`)
	if ran, err := store.Migrate(ctx, conn, fixed, 384); err != nil || ran != 1 {
		t.Fatalf("the corrected migration: %d applied, %v", ran, err)
	}
}

// The change and its ledger row commit together, and this is the half the server's own
// transaction does not cover: the migration succeeds and the *record* of it fails.
//
// Without one transaction around both, the table exists and nothing says so -- and the
// next start-up runs the same migration again, against a database it has already changed.
// A CREATE TABLE would then fail and the pod would restart-loop on a migration that
// worked. Forced by a CHECK constraint on the ledger rather than by timing, so it is a
// fact rather than a race.
func TestAMigrationThatCannotBeRecordedIsRolledBackWithItsRecord(t *testing.T) {
	ctx := context.Background()
	conn := connect(t, coldDatabase(t))

	first := files("0001_start.sql", `CREATE TABLE widget (id INT PRIMARY KEY)`)
	if _, err := store.Migrate(ctx, conn, first, 384); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx,
		`ALTER TABLE schema_migration ADD CONSTRAINT refuse_this CHECK (name <> 'unrecordable')`); err != nil {
		t.Fatal(err)
	}

	second := files(
		"0001_start.sql", `CREATE TABLE widget (id INT PRIMARY KEY)`,
		"0002_unrecordable.sql", `CREATE TABLE gadget (id INT PRIMARY KEY)`)
	if _, err := store.Migrate(ctx, conn, second, 384); err == nil {
		t.Fatal("a migration whose ledger row was refused reported success")
	}

	var gadget, recorded int
	if err := conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM information_schema.tables WHERE table_name = 'gadget'),
			(SELECT count(*) FROM schema_migration WHERE version = 2)`).
		Scan(&gadget, &recorded); err != nil {
		t.Fatal(err)
	}
	if gadget != 0 {
		t.Error("the migration is in the database and not in the ledger: the next " +
			"start-up will run it again against a schema it has already changed")
	}
	if recorded != 0 {
		t.Errorf("%d ledger rows for a migration that was refused", recorded)
	}
}

// The embedded set, checked without a database. Every one of these has a failure that
// looks like nothing at run time: a file the glob does not match is a migration silently
// skipped, and a stray printf verb in a future migration would corrupt the statement
// beside it rather than fail.
func TestTheEmbeddedMigrationsAreWellFormed(t *testing.T) {
	found, err := store.ReadMigrations(store.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("no migrations are embedded; the go:embed pattern has stopped matching")
	}
	if found[0].Version != 1 || found[0].Name != "baseline" {
		t.Errorf("the first migration is %04d_%s", found[0].Version, found[0].Name)
	}

	var dimensioned int
	for _, m := range found {
		if strings.Contains(m.SQL, ":dimensions") {
			dimensioned++
		}
		// A SQL file is not a format string. The substitution is a plain replace, so a
		// verb here means somebody wrote a migration expecting Sprintf -- and the first
		// LIKE '%x%' in a later file would then eat its neighbour.
		for _, verb := range []string{"%d", "%s", "%v", "%w"} {
			if strings.Contains(m.SQL, verb) {
				t.Errorf("%04d_%s contains %s; migrations are not format strings",
					m.Version, m.Name, verb)
			}
		}
	}
	if dimensioned != 1 {
		t.Errorf("%d migrations carry :dimensions, want exactly the baseline", dimensioned)
	}
}
