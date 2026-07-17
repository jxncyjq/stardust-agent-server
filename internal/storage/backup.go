package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type BackupManifest struct {
	SourcePath string
	BackupPath string
	Checksum   string
}

type RestoreResult struct {
	BackupPath     string
	TargetPath     string
	PreRestorePath string
	BackupChecksum string
}

func BackupSQLite(ctx context.Context, sourcePath string, backupPath string) (BackupManifest, error) {
	if err := ctx.Err(); err != nil {
		return BackupManifest{}, err
	}
	if sourcePath == "" || backupPath == "" {
		return BackupManifest{}, fmt.Errorf("source and backup paths are required")
	}
	if err := copyFile(sourcePath, backupPath); err != nil {
		return BackupManifest{}, fmt.Errorf("copy sqlite backup: %w", err)
	}
	checksum, err := fileSHA256(backupPath)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("checksum sqlite backup: %w", err)
	}
	if err := os.WriteFile(backupPath+".sha256", []byte(checksum), 0o600); err != nil {
		return BackupManifest{}, fmt.Errorf("write backup checksum: %w", err)
	}
	return BackupManifest{SourcePath: sourcePath, BackupPath: backupPath, Checksum: checksum}, nil
}

func RestoreSQLite(ctx context.Context, backupPath string, targetPath string) (RestoreResult, error) {
	if err := ctx.Err(); err != nil {
		return RestoreResult{}, err
	}
	if backupPath == "" || targetPath == "" {
		return RestoreResult{}, fmt.Errorf("backup and target paths are required")
	}
	checksum, err := fileSHA256(backupPath)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("checksum sqlite backup: %w", err)
	}
	expected, err := os.ReadFile(backupPath + ".sha256")
	if err != nil {
		return RestoreResult{}, fmt.Errorf("read backup checksum: %w", err)
	}
	if strings.TrimSpace(string(expected)) != checksum {
		return RestoreResult{}, fmt.Errorf("backup checksum mismatch")
	}
	preRestorePath := targetPath + ".pre-restore-" + time.Now().UTC().Format("20060102150405")
	if _, err := os.Stat(targetPath); err == nil {
		if err := copyFile(targetPath, preRestorePath); err != nil {
			return RestoreResult{}, fmt.Errorf("create pre-restore backup: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return RestoreResult{}, fmt.Errorf("stat restore target: %w", err)
	}
	if err := copyFile(backupPath, targetPath); err != nil {
		return RestoreResult{}, fmt.Errorf("restore sqlite backup: %w", err)
	}
	return RestoreResult{
		BackupPath:     backupPath,
		TargetPath:     targetPath,
		PreRestorePath: preRestorePath,
		BackupChecksum: checksum,
	}, nil
}

func copyFile(sourcePath string, targetPath string) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer target.Close()
	if _, err := io.Copy(target, source); err != nil {
		return err
	}
	return target.Sync()
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
