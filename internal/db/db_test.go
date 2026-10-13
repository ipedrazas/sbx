package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenAppliesMigrationsIdempotently(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dstudio.db")
	pool, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow("SELECT COUNT(*) FROM _migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(migrations) {
		t.Fatalf("applied %d of %d", n, len(migrations))
	}
	pool.Close()

	// Re-open: nothing new to apply, no errors from re-running ALTERs.
	pool, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, table := range []string{"datasources", "datasets", "agents", "actions", "audit_log", "agent_skill_installations"} {
		if _, err := pool.Exec("SELECT * FROM " + table + " LIMIT 1"); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
	var fk int
	if err := pool.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign keys not enabled: %d %v", fk, err)
	}
}

func TestOpenMemory(t *testing.T) {
	pool, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec("INSERT INTO datasources (name, ds_type) VALUES ('a', 'rest')"); err != nil {
		t.Fatal(err)
	}
	var sortable int
	if _, err := pool.Exec("INSERT INTO datasets (name, datasource_id) VALUES ('d', 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec("INSERT INTO dataset_fields (dataset_id, source_name, display_name) VALUES (1, 'x', 'x')"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow("SELECT sortable FROM dataset_fields").Scan(&sortable); err != nil || sortable != 1 {
		t.Fatalf("sortable default: %d %v", sortable, err)
	}
}
