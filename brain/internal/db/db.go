// Package db opens the collector's SQLite file and maps its rows to Go values.
// The collector owns the schema (collector/schema.sql); testschema.sql is a copy for tests.
//
// Open and Create pin the pool to one connection (SetMaxOpenConns(1)). While a *sql.Tx is
// open, any call on the *sql.DB itself (GetMeta, ReadSamples, ...) waits for that one
// connection and deadlocks, since the Tx holds it until Commit/Rollback. Inside a
// transaction use only the *sql.Tx helpers (InsertIntervals, DeleteIntervals, SetMetaTx)
// or tx.Query/tx.Exec directly.
package db

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed testschema.sql
var testSchema string

// SchemaVersion is the only meta.schema_version this brain understands.
const SchemaVersion = "4"

// ErrBeforeHorizon: a requested time precedes the oldest retained sample (exit code 2).
var ErrBeforeHorizon = errors.New("before the oldest retained sample (raw retention horizon)")

// ErrSchema is wrapped by CheckSchema on a version mismatch.
var ErrSchema = errors.New("schema version mismatch")

// DefaultPath is the collector's database location.
func DefaultPath(home string) string {
	return filepath.Join(home, "Library", "Application Support", "stint", "stint.db")
}

// Open opens an existing database read-write with a 5 s busy timeout. It never creates one.
func Open(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("database %s: %w (is the collector installed?)", path, err)
	}
	sq, err := sql.Open("sqlite3", dsn(path, "mode=rw"))
	if err != nil {
		return nil, err
	}
	sq.SetMaxOpenConns(1)
	return sq, nil
}

// dsn builds a file: URI; the path is percent-encoded so "Application Support" works.
func dsn(path, mode string) string {
	u := url.URL{Scheme: "file", Path: path, RawQuery: mode + "&_busy_timeout=5000"}
	return u.String()
}

// Create makes a new database at path (parent directories included) with the embedded
// test schema. Tests only.
func Create(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	sq, err := sql.Open("sqlite3", dsn(path, "mode=rwc"))
	if err != nil {
		return nil, err
	}
	sq.SetMaxOpenConns(1)
	if _, err := sq.Exec(testSchema); err != nil {
		sq.Close()
		return nil, err
	}
	return sq, nil
}

// CheckSchema fails unless meta.schema_version == SchemaVersion.
func CheckSchema(sq *sql.DB) error {
	v, ok, err := GetMeta(sq, "schema_version")
	if err != nil {
		return fmt.Errorf("reading meta.schema_version: %w", err)
	}
	if !ok || v != SchemaVersion {
		return fmt.Errorf("%w: got %q, want %q", ErrSchema, v, SchemaVersion)
	}
	return nil
}

// GetMeta reads meta[key]; ok is false when the key is absent.
func GetMeta(sq *sql.DB, key string) (value string, ok bool, err error) {
	err = sq.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

const metaUpsert = `INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`

// SetMeta upserts meta[key].
func SetMeta(sq *sql.DB, key, value string) error {
	_, err := sq.Exec(metaUpsert, key, value)
	return err
}

// SetMetaTx upserts meta[key] inside tx; the write lands with the transaction.
func SetMetaTx(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec(metaUpsert, key, value)
	return err
}

// DeleteMeta removes meta[key]; deleting an absent key is not an error.
func DeleteMeta(sq *sql.DB, key string) error {
	_, err := sq.Exec(`DELETE FROM meta WHERE key = ?`, key)
	return err
}
