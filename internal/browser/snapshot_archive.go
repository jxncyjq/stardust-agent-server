package browser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// fileSnapshotArchive 把全量观测文本按内容哈希落盘到 <root>/<dir>/<sha>.txt。
// 幂等去重（同内容命同名文件，存在即跳过写）。dir 用斜杠相对路径，返回值同样用斜杠，
// 供 read_file（接受相对工具根路径）翻页。
type fileSnapshotArchive struct {
	dir string // 相对工具根，如 ".legion/browser/snapshots"
}

// newFileSnapshotArchive 构造一个把快照写入相对工具根 dir 目录的归档器。
// dir 为空时回退到默认路径 ".legion/browser/snapshots"。
func newFileSnapshotArchive(dir string) *fileSnapshotArchive {
	if dir == "" {
		dir = ".legion/browser/snapshots"
	}
	return &fileSnapshotArchive{dir: dir}
}

// Save 把 content 按内容 SHA-256 哈希命名落盘到 <root>/<dir>/<sha>.txt，
// 返回相对 root 的斜杠路径（read_file 契约）。同内容已存在则跳过写并返回同路径（幂等去重）。
func (a *fileSnapshotArchive) Save(root, content string) (string, error) {
	sum := sha256.Sum256([]byte(content))
	name := hex.EncodeToString(sum[:]) + ".txt"
	rel := a.dir + "/" + name // 斜杠相对路径（read_file 契约）
	abs := filepath.Join(root, filepath.FromSlash(a.dir), name)
	if _, err := os.Stat(abs); err == nil {
		return rel, nil // 去重：同内容已存在
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("mkdir snapshot dir: %w", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write snapshot %s: %w", name, err)
	}
	return rel, nil
}

// Cleanup 删除 <root>/<dir> 下修改时间早于 now-ttl 的旧快照文件，best-effort。
// ttl<=0 视为不清理；目录尚未创建时无需清理直接返回。
func (a *fileSnapshotArchive) Cleanup(root string, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	base := filepath.Join(root, filepath.FromSlash(a.dir))
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 目录还没建，无需清理
		}
		return fmt.Errorf("read snapshot dir: %w", err)
	}
	cutoff := time.Now().Add(-ttl)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(base, e.Name())) // best-effort
		}
	}
	return nil
}
