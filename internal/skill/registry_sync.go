package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/stardust/legion-agent/internal/port"
)

type RegistrySyncConfig struct {
	IndexURL    string
	InstallRoot string
	Repository  Repository
	Scanner     SkillSecurityScanner
	Audit       port.AuditLog
}

type RegistrySyncReport struct {
	Installed   int `json:"installed"`
	Quarantined int `json:"quarantined"`
	Failed      int `json:"failed"`
}

type RegistrySyncer struct {
	cfg RegistrySyncConfig
}

func NewRegistrySyncer(cfg RegistrySyncConfig) *RegistrySyncer {
	return &RegistrySyncer{cfg: cfg}
}

func (s *RegistrySyncer) Sync(ctx context.Context) (RegistrySyncReport, error) {
	if err := ctx.Err(); err != nil {
		return RegistrySyncReport{}, err
	}
	index, err := s.fetchIndex(ctx)
	if err != nil {
		return RegistrySyncReport{}, err
	}
	report := RegistrySyncReport{}
	for _, item := range index.Skills {
		manifestPath, cleanup, err := s.fetchManifest(ctx, item.ManifestURL)
		if err != nil {
			report.Failed++
			continue
		}
		installer := NewInstaller(InstallerConfig{
			InstallRoot: s.cfg.InstallRoot,
			Scanner:     s.cfg.Scanner,
			Repository:  s.cfg.Repository,
			Audit:       s.cfg.Audit,
		})
		_, err = installer.InstallFromManifest(ctx, manifestPath)
		cleanup()
		switch {
		case err == nil:
			report.Installed++
		case errors.Is(err, ErrSkillScanBlocked):
			report.Quarantined++
		default:
			report.Failed++
		}
	}
	return report, nil
}

type registryIndex struct {
	Skills []registryIndexItem `json:"skills"`
}

type registryIndexItem struct {
	ManifestURL string `json:"manifest_url"`
}

func (s *RegistrySyncer) fetchIndex(ctx context.Context) (registryIndex, error) {
	var index registryIndex
	if s.cfg.IndexURL == "" {
		return index, fmt.Errorf("registry index url is required")
	}
	body, err := fetchRegistryBytes(ctx, s.cfg.IndexURL)
	if err != nil {
		return index, fmt.Errorf("fetch registry index: %w", err)
	}
	if err := json.Unmarshal(body, &index); err != nil {
		return index, fmt.Errorf("decode registry index: %w", err)
	}
	return index, nil
}

func (s *RegistrySyncer) fetchManifest(ctx context.Context, manifestURL string) (string, func(), error) {
	body, err := fetchRegistryBytes(ctx, manifestURL)
	if err != nil {
		return "", func() {}, fmt.Errorf("fetch registry manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return "", func() {}, fmt.Errorf("decode registry manifest: %w", err)
	}
	dir, err := os.MkdirTemp("", "legion-skill-manifest-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create registry manifest temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	content, err := fetchRegistryBytes(ctx, manifest.ContentPath)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("fetch registry content: %w", err)
	}
	contentPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(contentPath, content, 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("write registry skill content: %w", err)
	}
	manifest.ContentPath = contentPath
	localManifest, err := json.Marshal(manifest)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("marshal local registry manifest: %w", err)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, localManifest, 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("write registry manifest: %w", err)
	}
	return manifestPath, cleanup, nil
}

func fetchRegistryBytes(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse registry url %q: %w", rawURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported registry url scheme %q", parsed.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create registry request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read registry response: %w", err)
	}
	return body, nil
}
