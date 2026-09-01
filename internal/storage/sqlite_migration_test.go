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

// conversation_turns 的四个 token 列不在这张清单里：那张表连同它的写入方已在 P3
// Task 5 退役（事件日志是唯一真相源，spec §3 取舍 A2），token 用量走事件载荷的
// usage 字段、由 storage.projectTurns 累加还原。audit_events 一侧仍然落盘，所以
// 这条测试守的就只剩它。
func TestTokenColumnsExistAfterInit(t *testing.T) {
	r := openTestRepo(t)
	for _, tc := range []struct{ table, col string }{
		{"audit_events", "prompt_tokens"}, {"audit_events", "completion_tokens"},
		{"audit_events", "cached_tokens"}, {"audit_events", "total_tokens"},
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
