package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "github.com/uptrace/bun/driver/sqliteshim"
)

// openRaw opens a plain SQLite database to seed pre-migration schema states.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	return db
}

func tableNames(t *testing.T, path string) map[string]bool {
	t.Helper()
	db := openRaw(t, path)
	defer db.Close()
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	names := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names[name] = true
	}
	return names
}

func countRows(t *testing.T, path, table string) int {
	t.Helper()
	db := openRaw(t, path)
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// A fresh database must start. Before the table name was pinned, bun created
// "reasoning_cache_entries" and the index step failed with
// "no such table: main.reasoning_cache", so the proxy refused to boot.
func TestFreshDatabaseStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.sqlite3")
	s, err := NewReasoningStore(context.Background(), path, 3600, 100)
	if err != nil {
		t.Fatalf("NewReasoningStore on a fresh database: %v", err)
	}
	defer s.Close(context.Background())

	if !tableNames(t, path)["reasoning_cache"] {
		t.Fatalf("reasoning_cache table missing: %v", tableNames(t, path))
	}
	if tableNames(t, path)["reasoning_cache_entries"] {
		t.Fatalf("pluralized table should not exist on a fresh database")
	}
}

// Rows written by older builds into "reasoning_cache_entries" must survive the
// upgrade: they are the live cache, and renaming avoids copying gigabytes.
func TestMigratesLegacyEntriesTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.sqlite3")
	raw := openRaw(t, path)
	if _, err := raw.Exec(`CREATE TABLE reasoning_cache_entries (
		key TEXT PRIMARY KEY, reasoning TEXT NOT NULL,
		message_json TEXT NOT NULL, created_at TIMESTAMP NOT NULL)`); err != nil {
		t.Fatalf("seed legacy table: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := raw.Exec(
			"INSERT INTO reasoning_cache_entries (key, reasoning, message_json, created_at) VALUES (?, ?, ?, datetime('now'))",
			fmt.Sprintf("k%d", i), "why", "{}",
		); err != nil {
			t.Fatalf("seed row: %v", err)
		}
	}
	raw.Close()

	s, err := NewReasoningStore(context.Background(), path, 86400, 100)
	if err != nil {
		t.Fatalf("NewReasoningStore during migration: %v", err)
	}
	defer s.Close(context.Background())

	names := tableNames(t, path)
	if names["reasoning_cache_entries"] {
		t.Fatalf("pluralized table should be renamed away: %v", names)
	}
	if got := countRows(t, path, "reasoning_cache"); got != 3 {
		t.Fatalf("migrated rows = %d, want 3", got)
	}
}

// Databases from the Python version already have "reasoning_cache" in a
// different row format. It must be archived, not overwritten, and the live
// rows from the pluralized table must win.
func TestMigrationArchivesExistingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.sqlite3")
	raw := openRaw(t, path)
	if _, err := raw.Exec(`CREATE TABLE reasoning_cache (
		key TEXT PRIMARY KEY, reasoning TEXT, message_json TEXT, created_at TIMESTAMP)`); err != nil {
		t.Fatalf("seed python table: %v", err)
	}
	if _, err := raw.Exec(
		"INSERT INTO reasoning_cache (key, reasoning, message_json, created_at) VALUES ('old', 'r', '{}', 1778060684)",
	); err != nil {
		t.Fatalf("seed python row: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE reasoning_cache_entries (
		key TEXT PRIMARY KEY, reasoning TEXT NOT NULL,
		message_json TEXT NOT NULL, created_at TIMESTAMP NOT NULL)`); err != nil {
		t.Fatalf("seed pluralized table: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := raw.Exec(
			"INSERT INTO reasoning_cache_entries (key, reasoning, message_json, created_at) VALUES (?, ?, ?, datetime('now'))",
			fmt.Sprintf("new%d", i), "why", "{}",
		); err != nil {
			t.Fatalf("seed live row: %v", err)
		}
	}
	raw.Close()

	s, err := NewReasoningStore(context.Background(), path, 86400, 100)
	if err != nil {
		t.Fatalf("NewReasoningStore during migration: %v", err)
	}
	defer s.Close(context.Background())

	if got := countRows(t, path, "reasoning_cache"); got != 2 {
		t.Fatalf("active table rows = %d, want the 2 live rows", got)
	}

	archived := 0
	for name := range tableNames(t, path) {
		if len(name) > len("reasoning_cache_legacy_") && name[:len("reasoning_cache_legacy_")] == "reasoning_cache_legacy_" {
			archived++
			if got := countRows(t, path, name); got != 1 {
				t.Fatalf("archived rows in %s = %d, want 1", name, got)
			}
		}
	}
	if archived != 1 {
		t.Fatalf("archived legacy tables = %d, want 1: %v", archived, tableNames(t, path))
	}
}

// The row cap must actually be enforced. It silently did nothing while it
// targeted a table the ORM never wrote to.
func TestRowCapPruneEnforced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.sqlite3")
	s, err := NewReasoningStore(context.Background(), path, 86400, 5)
	if err != nil {
		t.Fatalf("NewReasoningStore: %v", err)
	}
	defer s.Close(context.Background())

	for i := 0; i < 12; i++ {
		if err := s.putDirect(context.Background(), fmt.Sprintf("k%02d", i), "why", "{}"); err != nil {
			t.Fatalf("putDirect: %v", err)
		}
	}
	if err := s.Prune(context.Background()); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if got := countRows(t, path, "reasoning_cache"); got > 5 {
		t.Fatalf("rows after prune = %d, want <= 5 (cap not enforced)", got)
	}
}

// Migration must be idempotent: a second start has nothing left to move.
func TestMigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.sqlite3")
	raw := openRaw(t, path)
	if _, err := raw.Exec(`CREATE TABLE reasoning_cache_entries (
		key TEXT PRIMARY KEY, reasoning TEXT NOT NULL,
		message_json TEXT NOT NULL, created_at TIMESTAMP NOT NULL)`); err != nil {
		t.Fatalf("seed legacy table: %v", err)
	}
	raw.Close()

	for i := 0; i < 2; i++ {
		s, err := NewReasoningStore(context.Background(), path, 86400, 100)
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		s.Close(context.Background())
	}

	if names := tableNames(t, path); names["reasoning_cache_entries"] {
		t.Fatalf("pluralized table reappeared: %v", names)
	}
}
