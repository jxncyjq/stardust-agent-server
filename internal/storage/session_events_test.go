package storage

import (
	"context"
	"path/filepath"
	"testing"
)

// 建表这一条不是形式主义：表名/主键写错的症状是「一切正常，直到两条事件撞了 seq」，
// 而那时错误信息指向的是 UNIQUE 约束，不是这次改动。
func TestTheSessionEventsTableExistsWithACompositeKey(t *testing.T) {
	ctx := context.Background()
	repo, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer repo.Close()

	var count int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='session_events'`,
	).Scan(&count); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	if count != 1 {
		t.Fatalf("session_events 表不存在")
	}

	// 主键必须是 (session_id, seq) 两列：只按 seq 建键会让不同会话互相挤号。
	rows, err := repo.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('session_events') WHERE pk > 0 ORDER BY pk`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer rows.Close()
	var pk []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan pk column: %v", err)
		}
		pk = append(pk, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pk columns: %v", err)
	}
	if len(pk) != 2 || pk[0] != "session_id" || pk[1] != "seq" {
		t.Errorf("主键 = %v, want [session_id seq]", pk)
	}
}
