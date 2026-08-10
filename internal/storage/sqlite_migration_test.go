package storage

import (
	"context"
	"testing"
)

func columnExists(t *testing.T, r *SQLiteRepository, table, col string) bool {
	t.Helper()
	rows, err := r.db.QueryContext(context.Background(), "PRAGMA table_info("+table+")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == col {
			return true
		}
	}
	return false
}

func TestTokenColumnsExistAfterInit(t *testing.T) {
	r := openTestRepo(t)
	for _, tc := range []struct{ table, col string }{
		{"audit_events", "prompt_tokens"}, {"audit_events", "completion_tokens"},
		{"audit_events", "cached_tokens"}, {"audit_events", "total_tokens"},
		{"conversation_turns", "prompt_tokens"}, {"conversation_turns", "completion_tokens"},
		{"conversation_turns", "cached_tokens"}, {"conversation_turns", "total_tokens"},
	} {
		if !columnExists(t, r, tc.table, tc.col) {
			t.Errorf("missing column %s.%s", tc.table, tc.col)
		}
	}
}

func TestApplyColumnMigrationsIdempotent(t *testing.T) {
	r := openTestRepo(t)
	if err := r.applyColumnMigrations(context.Background()); err != nil {
		t.Fatalf("re-running migrations must be idempotent: %v", err)
	}
}
