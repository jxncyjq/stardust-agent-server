package cli

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	agentruntime "github.com/stardust/legion-agent/internal/runtime"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

// fakeMaas is a minimal port.MaasInferenceClient test double that records the
// prompt it received and returns a scripted response or error.
type fakeMaas struct {
	text string
	err  error
	got  string // prompt actually received
}

func (f *fakeMaas) Generate(_ context.Context, req port.InferenceRequest) (port.InferenceResponse, error) {
	f.got = req.Prompt
	if f.err != nil {
		return port.InferenceResponse{}, f.err
	}
	return port.InferenceResponse{Text: f.text}, nil
}

// fakeEpisodicStore is a minimal memory.EpisodicStore test double that records
// every entry added.
type fakeEpisodicStore struct {
	added []domain.MemoryEntry
}

func (s *fakeEpisodicStore) Add(_ context.Context, agent domain.Agent, task domain.Task, content string) (domain.MemoryEntry, error) {
	e := domain.MemoryEntry{ID: "x", AgentID: agent.ID, TaskID: task.ID, Content: content}
	s.added = append(s.added, e)
	return e, nil
}

func (s *fakeEpisodicStore) Search(context.Context, string, int) ([]domain.MemoryEntry, error) {
	return nil, nil
}

func TestEpisodeRecorderDistillsAndStores(t *testing.T) {
	maas := &fakeMaas{text: "  团体保险保全服务办理成功  "}
	store := &fakeEpisodicStore{}
	rec := newEpisodeRecorder(maas, store, slog.Default())

	if err := rec.record(context.Background(), domain.Agent{ID: "a1"}, domain.Task{ID: "t1", Input: "查保全"}, "success", "很长的原始结果文本..."); err != nil {
		t.Fatalf("record: %v", err)
	}
	if len(store.added) != 1 || store.added[0].Content != "团体保险保全服务办理成功" {
		t.Fatalf("expected distilled trimmed content, got %+v", store.added)
	}
	if !strings.Contains(maas.got, "success") {
		t.Fatalf("distill prompt missing outcome, got %q", maas.got)
	}
}

func TestEpisodeRecorderFallsBackOnDistillError(t *testing.T) {
	maas := &fakeMaas{err: errors.New("boom")}
	store := &fakeEpisodicStore{}
	rec := newEpisodeRecorder(maas, store, slog.Default())

	if err := rec.record(context.Background(), domain.Agent{ID: "a1"}, domain.Task{ID: "t1"}, "failure:tool", "raw failure detail"); err != nil {
		t.Fatalf("record must not error on distill failure (falls back): %v", err)
	}
	if len(store.added) != 1 || store.added[0].Content == "" {
		t.Fatalf("expected raw fallback stored, got %+v", store.added)
	}
	if store.added[0].Content != "raw failure detail" {
		t.Fatalf("expected raw content fallback, got %q", store.added[0].Content)
	}
}

func TestEpisodeRecorderFallsBackOnEmptyDistillResult(t *testing.T) {
	maas := &fakeMaas{text: "   "}
	store := &fakeEpisodicStore{}
	rec := newEpisodeRecorder(maas, store, slog.Default())

	if err := rec.record(context.Background(), domain.Agent{ID: "a1"}, domain.Task{ID: "t1"}, "success", "raw content here"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if len(store.added) != 1 || store.added[0].Content != "raw content here" {
		t.Fatalf("expected raw fallback on empty distill result, got %+v", store.added)
	}
}

func TestEpisodeRecorderReturnsErrorOnStoreFailure(t *testing.T) {
	maas := &fakeMaas{text: "distilled"}
	store := &failingEpisodicStore{err: errors.New("store down")}
	rec := newEpisodeRecorder(maas, store, slog.Default())

	if err := rec.record(context.Background(), domain.Agent{ID: "a1"}, domain.Task{ID: "t1"}, "success", "content"); err == nil {
		t.Fatal("expected error when store.Add fails")
	}
}

type failingEpisodicStore struct {
	err error
}

func (s *failingEpisodicStore) Add(context.Context, domain.Agent, domain.Task, string) (domain.MemoryEntry, error) {
	return domain.MemoryEntry{}, s.err
}

func (s *failingEpisodicStore) Search(context.Context, string, int) ([]domain.MemoryEntry, error) {
	return nil, nil
}

// Compile-time assertion: *episodeRecorder satisfies runtime.EpisodeRecorder.
var _ agentruntime.EpisodeRecorder = (*episodeRecorder)(nil)
