package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 往前迁移是自动的（CREATE IF NOT EXISTS + 幂等 ALTER），反方向不是：旧二进制打开
// 新库时，schema_migrations 里那个更高的版本号是唯一的线索。以前 migrate 只
// INSERT OR IGNORE、从不读回，于是进程照常启动，一路跑到某个查询撞上不存在的列才
// 炸——届时离原因很远，而期间的写入可能已经污染了库。
func TestMigrateRefusesADatabaseNewerThanTheBinary(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "future.db")

	// 先让本程序正常建库，再手工写进一个更高的版本号——这正是「回滚了二进制却没
	// 回滚数据」留下的形状。
	repo, err := OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		CurrentSchemaVersion+7, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed future version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	_, err = OpenSQLite(context.Background(), path)
	if err == nil {
		t.Fatalf("比本程序新 %d 个版本的库被照常打开了：进程会跑到某个查询撞上不存在的列"+
			"才炸，届时看不出是二进制回滚了；期间的写入还可能污染这个库", 7)
	}
	msg := err.Error()
	if !strings.Contains(msg, "newer than this binary") {
		t.Errorf("错误信息没说清是「库比程序新」，运维定位不到：%v", err)
	}
	// 两个版本号都要出现，否则运维不知道该升到哪一版。
	if !strings.Contains(msg, "18") || !strings.Contains(msg, "11") {
		t.Errorf("错误信息里没同时给出库的版本与本程序支持的版本：%v", err)
	}
}

// 同版本与旧版本都必须照常打开——守卫不能把正常升级路径也挡掉。
func TestMigrateAcceptsTheCurrentAndOlderVersions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		version int
	}{
		{"same", CurrentSchemaVersion},
		{"older", CurrentSchemaVersion - 3},
		{"ancient", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "db.sqlite")

			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatalf("sql.Open: %v", err)
			}
			if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
				version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
				t.Fatalf("create table: %v", err)
			}
			if _, err := db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
				tc.version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatalf("seed version: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			repo, err := OpenSQLite(context.Background(), path)
			if err != nil {
				t.Fatalf("版本 %d 的库被拒了，而它不比本程序新（%d）：正常的升级路径被挡住了：%v",
					tc.version, CurrentSchemaVersion, err)
			}
			t.Cleanup(func() { _ = repo.Close() })

			// 打开之后必须真的完成迁移：版本号被推到当前值，说明建表与列迁移都跑过了。
			got, err := repo.SchemaVersion(context.Background())
			if err != nil {
				t.Fatalf("SchemaVersion: %v", err)
			}
			if got != CurrentSchemaVersion {
				t.Errorf("迁移后版本 = %d，要 %d：旧库没被升上来", got, CurrentSchemaVersion)
			}
		})
	}
}
