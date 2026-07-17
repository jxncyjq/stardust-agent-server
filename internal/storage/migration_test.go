package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSchemaMigrationRecordsCurrentVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})

	version, err := repo.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion() error = %v, want nil", err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", version, CurrentSchemaVersion)
	}
	if err := repo.migrate(ctx); err != nil {
		t.Fatalf("migrate() second run error = %v, want nil", err)
	}
	version, err = repo.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion() after second migrate error = %v, want nil", err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() after second migrate = %d, want %d", version, CurrentSchemaVersion)
	}
}
