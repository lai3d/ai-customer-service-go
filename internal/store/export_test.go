package store

import (
	"context"
	"io/fs"

	"github.com/jackc/pgx/v5"
)

// The migration runner and the file reader, for the external test package. Exported here
// rather than in the package itself because nothing outside this package should be able
// to issue DDL: a migration function anybody can call is a migration that runs without
// the advisory lock.

func Migrate(ctx context.Context, conn *pgx.Conn, fsys fs.FS, dimensions int) (int, error) {
	return migrate(ctx, conn, fsys, dimensions)
}

type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

func ReadMigrations(fsys fs.FS) ([]Migration, error) {
	found, err := readMigrations(fsys)
	if err != nil {
		return nil, err
	}
	out := make([]Migration, 0, len(found))
	for _, m := range found {
		out = append(out, Migration{m.version, m.name, m.sql, m.checksum})
	}
	return out, nil
}
