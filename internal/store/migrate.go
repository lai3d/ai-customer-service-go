package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Migrations are the versioned schema files, embedded so a container carries them and a
// deployment cannot half-ship them.
//
//go:embed migrations/*.sql
var Migrations embed.FS

// The ledger. Created before anything reads it, by the only statement in this package
// that is not itself a migration -- a bootstrap has to start somewhere, and this table's
// shape is the one thing that cannot be migrated by the thing it records.
//
// checksum is the point of the table. version alone records that a file with that number
// ran; the checksum records that it was *this* file, which is what catches the edit
// somebody makes to an applied migration because it was easier than writing another one.
const migrationLedger = `
CREATE TABLE IF NOT EXISTS schema_migration (
    version     INTEGER     NOT NULL PRIMARY KEY,
    name        TEXT        NOT NULL,
    checksum    TEXT        NOT NULL,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    duration_ms BIGINT      NOT NULL
)`

// A migration file: 0001_baseline.sql.
type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

var (
	// ErrChecksumChanged is a migration that has already run and is no longer the file
	// that ran. Startup fails: what the database contains and what the repository says
	// it contains have diverged, and every guess about which is right is worse than
	// stopping.
	ErrChecksumChanged = errors.New("an applied migration has been edited")
	// ErrOutOfOrder is a migration numbered below one that has already been applied.
	// It is what two branches adding 0007 and merging looks like from the database's
	// side: the second one would be skipped in production and applied on every developer
	// machine that had not seen the first.
	ErrOutOfOrder = errors.New("a migration is numbered below one already applied")
)

// migrate applies everything not yet applied, in version order, and returns how many ran.
//
// The caller holds the schema advisory lock. Nothing here takes it: a function that takes
// its own lock is a function that can be called without one, and this is the only place
// in the service that issues DDL.
func migrate(ctx context.Context, conn *pgx.Conn, fsys fs.FS, dimensions int) (int, error) {
	if _, err := conn.Exec(ctx, migrationLedger); err != nil {
		return 0, fmt.Errorf("create the migration ledger: %w", err)
	}

	files, err := readMigrations(fsys)
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		// A build that embedded nothing would otherwise report a healthy start against
		// an empty database, and the first query would be the error message.
		return 0, errors.New("no migrations found: the embed pattern has stopped matching")
	}

	applied, highest, err := appliedMigrations(ctx, conn)
	if err != nil {
		return 0, err
	}

	ran := 0
	for _, m := range files {
		if was, ok := applied[m.version]; ok {
			if was != m.checksum {
				return ran, fmt.Errorf("%w: %04d_%s (the database has %s, this build has %s)",
					ErrChecksumChanged, m.version, m.name, short(was), short(m.checksum))
			}
			continue
		}
		if m.version < highest {
			return ran, fmt.Errorf("%w: %04d_%s, and %04d has already run. Renumber it above "+
				"%04d rather than applying it by hand -- a database that skipped it and one "+
				"that ran it are both defensible, which is the problem",
				ErrOutOfOrder, m.version, m.name, highest, highest)
		}
		if err := applyOne(ctx, conn, m, dimensions); err != nil {
			return ran, err
		}
		ran++
	}
	return ran, nil
}

// applyOne runs one migration and records it in the same transaction.
//
// Together or not at all. A migration that ran and was not recorded runs again on the
// next start-up -- against a database it has already changed -- and one recorded without
// running is a lie that only shows up in the migration after it.
//
// The cost of that choice is stated rather than hidden: everything here runs inside a
// transaction, so CREATE INDEX CONCURRENTLY cannot. A migration that needs one needs a
// different mechanism, and inventing that mechanism before there is a table big enough to
// need it would be inventing the requirement too.
func applyOne(ctx context.Context, conn *pgx.Conn, m migration, dimensions int) error {
	started := time.Now()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin %04d_%s: %w", m.version, m.name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	sql := strings.ReplaceAll(m.sql, ":dimensions", strconv.Itoa(dimensions))
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("apply %04d_%s: %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO schema_migration (version, name, checksum, duration_ms)
		VALUES ($1,$2,$3,$4)`,
		m.version, m.name, m.checksum, time.Since(started).Milliseconds()); err != nil {
		return fmt.Errorf("record %04d_%s: %w", m.version, m.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %04d_%s: %w", m.version, m.name, err)
	}
	slog.Info("applied a schema migration",
		"version", m.version, "name", m.name, "millis", time.Since(started).Milliseconds())
	return nil
}

// readMigrations reads and sorts the files. A name that does not parse is an error rather
// than a file quietly skipped: a migration nobody notices is missing is the whole failure
// mode this package exists to prevent.
func readMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	out := make([]migration, 0, len(entries))
	seen := map[int]string{}
	for _, entry := range entries {
		base := strings.TrimSuffix(path.Base(entry), ".sql")
		number, name, found := strings.Cut(base, "_")
		if !found {
			return nil, fmt.Errorf("migration %q is not <version>_<name>.sql", entry)
		}
		version, err := strconv.Atoi(number)
		if err != nil {
			return nil, fmt.Errorf("migration %q does not start with a version number", entry)
		}
		if other, clash := seen[version]; clash {
			return nil, fmt.Errorf("migrations %q and %q share version %d", other, entry, version)
		}
		seen[version] = entry

		body, err := fs.ReadFile(fsys, entry)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			version:  version,
			name:     name,
			sql:      string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// appliedMigrations returns what the database says has run, and the highest version it
// has seen. The highest is not len(applied): a ledger with 1 and 3 in it has a hole, and
// the hole is what the out-of-order check is about.
func appliedMigrations(ctx context.Context, conn *pgx.Conn) (map[int]string, int, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migration`)
	if err != nil {
		return nil, 0, fmt.Errorf("read the migration ledger: %w", err)
	}
	defer rows.Close()

	applied := map[int]string{}
	highest := 0
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, 0, err
		}
		applied[version] = checksum
		if version > highest {
			highest = version
		}
	}
	return applied, highest, rows.Err()
}

func short(checksum string) string {
	if len(checksum) < 12 {
		return checksum
	}
	return checksum[:12]
}
