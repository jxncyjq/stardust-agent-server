package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/domain"
)

type stubProvider struct{ details map[string]string }

func (p stubProvider) Entries(context.Context) ([]capability.Entry, error) {
	entries := make([]capability.Entry, 0, len(p.details))
	for name := range p.details {
		entries = append(entries, capability.Entry{
			Name: name, Group: "files", Summary: "一行说明", Kind: capability.KindTool,
		})
	}
	return entries, nil
}

func (p stubProvider) Detail(_ context.Context, name string) (string, error) {
	detail, ok := p.details[name]
	if !ok {
		return "", capability.ErrUnknownCapability
	}
	return detail, nil
}

func loadCall(names ...string) domain.ToolCall {
	return domain.ToolCall{
		ID:        "c1",
		Name:      metaToolLoadCapabilities,
		Arguments: map[string]string{"names": strings.Join(names, ",")},
	}
}

func TestLoadCapabilitiesPutsDetailInLoadedBlock(t *testing.T) {
	t.Parallel()
	rt := NewRuntime(Config{})
	catalog := capability.NewCatalog(stubProvider{details: map[string]string{"read_file": "SCHEMA-MARKER"}})
	st := &loopState{}

	result, err := rt.dispatchLoadCapabilities(context.Background(), st, loadCall("read_file"), catalog)
	if err != nil {
		t.Fatalf("dispatchLoadCapabilities() error = %v, want nil", err)
	}
	if !result.Success {
		t.Fatalf("result.Success = false, error = %q", result.Error)
	}
	if len(st.loaded) != 1 || st.loaded[0].detail != "SCHEMA-MARKER" {
		t.Fatalf("loaded = %+v, want the detail pinned in the loaded block", st.loaded)
	}
	if strings.Contains(result.Output, "SCHEMA-MARKER") {
		t.Error("the detail went into the tool result: it would then be subject to the 4000-char truncation and to mid-prompt dropping")
	}
}

func TestLoadCapabilitiesRejectsUnknownName(t *testing.T) {
	t.Parallel()
	rt := NewRuntime(Config{})
	catalog := capability.NewCatalog(stubProvider{details: map[string]string{"read_file": "S"}})
	st := &loopState{}

	// 作用域检查落在这里:目录由调用方的 registry 建,Plan 模式过滤掉的
	// 工具不在目录里,因此也 load 不到。
	result, err := rt.dispatchLoadCapabilities(context.Background(), st, loadCall("write_file"), catalog)
	if err != nil {
		t.Fatalf("dispatchLoadCapabilities() error = %v, want nil (failures go back to the model)", err)
	}
	if result.Success {
		t.Fatal("result.Success = true, want a failed result for an out-of-catalog name")
	}
	if !strings.Contains(result.Error, "write_file") {
		t.Errorf("error = %q, want it to name the rejected capability", result.Error)
	}
	if len(st.loaded) != 0 {
		t.Error("a rejected load still modified the loaded block")
	}
}

func TestLoadCapabilitiesRejectsEmptyAndOversizedBatch(t *testing.T) {
	t.Parallel()
	rt := NewRuntime(Config{})
	details := map[string]string{}
	names := make([]string, 0, maxLoadBatch+1)
	for i := 0; i <= maxLoadBatch; i++ {
		name := string(rune('a' + i))
		details[name] = "S"
		names = append(names, name)
	}
	catalog := capability.NewCatalog(stubProvider{details: details})

	empty, err := rt.dispatchLoadCapabilities(context.Background(), &loopState{}, loadCall(), catalog)
	if err != nil {
		t.Fatalf("empty batch error = %v, want nil", err)
	}
	if empty.Success {
		t.Error("empty names list was accepted")
	}

	over, err := rt.dispatchLoadCapabilities(context.Background(), &loopState{}, loadCall(names...), catalog)
	if err != nil {
		t.Fatalf("oversized batch error = %v, want nil", err)
	}
	if over.Success {
		t.Errorf("batch of %d was accepted, limit is %d", len(names), maxLoadBatch)
	}
}

type recordingUsage struct{ touched []string }

func (u *recordingUsage) Touch(id string, _ time.Time) { u.touched = append(u.touched, id) }

func TestLoadCapabilitiesTouchesSkillUsage(t *testing.T) {
	t.Parallel()
	usage := &recordingUsage{}
	rt := NewRuntime(Config{SkillUsage: usage})
	catalog := capability.NewCatalog(stubProvider{details: map[string]string{"go-testing": "BODY"}})

	if _, err := rt.dispatchLoadCapabilities(context.Background(), &loopState{}, loadCall("go-testing"), catalog); err != nil {
		t.Fatalf("dispatchLoadCapabilities() error = %v", err)
	}
	// Curator 靠使用记录做老化清理,而「无使用记录的技能不会被动」
	// (skill/curator.go:153)。不 Touch 就等于 Curator 停摆且无人察觉。
	if len(usage.touched) != 1 || usage.touched[0] != "go-testing" {
		t.Errorf("touched = %v, want [go-testing]", usage.touched)
	}
}
