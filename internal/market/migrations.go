package market

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jackc/pgx/v5"
)

// Migrate upgrades the original unversioned schema in place. The original 001
// remains idempotent; it is executed once and adopted into the version ledger.
func (s *Store) Migrate(ctx context.Context, directory string) error {
	files, err := filepath.Glob(filepath.Join(directory, "[0-9][0-9][0-9]_*.sql"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no migrations in %s", directory)
	}
	sort.Strings(files)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(743204)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	for _, file := range files {
		body, e := os.ReadFile(file)
		if e != nil {
			return e
		}
		body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
		version := filepath.Base(file)
		checksum := fmt.Sprintf("%x", sha256.Sum256(body))
		var prior string
		e = tx.QueryRow(ctx, "SELECT checksum FROM schema_migrations WHERE version=$1", version).Scan(&prior)
		if e == nil {
			if prior != checksum {
				return fmt.Errorf("applied migration %s changed; restore it and add a new migration", version)
			}
			continue
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		if _, e = tx.Exec(ctx, string(body)); e != nil {
			return fmt.Errorf("migration %s: %w", version, e)
		}
		if _, e = tx.Exec(ctx, "INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)", version, checksum); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
