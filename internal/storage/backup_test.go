package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestSQLiteBackupRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agent.db")
	backupPath := filepath.Join(dir, "agent.db.bak")
	repo, err := OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) error = %v, want nil", dbPath, err)
	}
	if err := repo.SaveTask(ctx, domain.Task{
		ID:        "backup-task",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Status:    domain.TaskDone,
		Input:     "before backup",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveTask(backup-task) error = %v, want nil", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close(source repo) error = %v, want nil", err)
	}

	manifest, err := BackupSQLite(ctx, dbPath, backupPath)
	if err != nil {
		t.Fatalf("BackupSQLite(%q, %q) error = %v, want nil", dbPath, backupPath, err)
	}
	if manifest.Checksum == "" {
		t.Fatalf("BackupSQLite().Checksum = empty, want sha256")
	}
	if _, err := os.Stat(backupPath + ".sha256"); err != nil {
		t.Fatalf("Stat(%q) error = %v, want checksum sidecar", backupPath+".sha256", err)
	}

	repo, err = OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) after backup error = %v, want nil", dbPath, err)
	}
	if err := repo.SaveTask(ctx, domain.Task{
		ID:        "backup-task",
		CompanyID: "company-1",
		AgentID:   "agent-1",
		Status:    domain.TaskDone,
		Input:     "after backup",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveTask(modified backup-task) error = %v, want nil", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close(modified repo) error = %v, want nil", err)
	}

	restore, err := RestoreSQLite(ctx, backupPath, dbPath)
	if err != nil {
		t.Fatalf("RestoreSQLite(%q, %q) error = %v, want nil", backupPath, dbPath, err)
	}
	if restore.PreRestorePath == "" {
		t.Fatalf("RestoreSQLite().PreRestorePath = empty, want pre-restore backup")
	}
	if _, err := os.Stat(restore.PreRestorePath); err != nil {
		t.Fatalf("Stat(%q) error = %v, want pre-restore backup", restore.PreRestorePath, err)
	}
	repo, err = OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) after restore error = %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("Close(restored repo) error = %v, want nil", err)
		}
	})
	task, ok, err := repo.GetTask(ctx, "backup-task")
	if err != nil {
		t.Fatalf("GetTask(backup-task) error = %v, want nil", err)
	}
	if !ok || task.Input != "before backup" {
		t.Fatalf("GetTask(backup-task) = %#v, %t, want restored input before backup", task, ok)
	}
}

func TestSQLiteRestoreRejectsChecksumMismatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agent.db")
	backupPath := filepath.Join(dir, "agent.db.bak")
	repo, err := OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite(%q) error = %v, want nil", dbPath, err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if _, err := BackupSQLite(ctx, dbPath, backupPath); err != nil {
		t.Fatalf("BackupSQLite(%q, %q) error = %v, want nil", dbPath, backupPath, err)
	}
	if err := os.WriteFile(backupPath+".sha256", []byte("bad-checksum"), 0o600); err != nil {
		t.Fatalf("WriteFile(checksum) error = %v, want nil", err)
	}

	if _, err := RestoreSQLite(ctx, backupPath, dbPath); err == nil {
		t.Fatalf("RestoreSQLite(checksum mismatch) error = nil, want error")
	}
}
