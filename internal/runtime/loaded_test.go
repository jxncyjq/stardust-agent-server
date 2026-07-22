package runtime

import (
	"strings"
	"testing"
)

func TestAppendLoadedIsIdempotent(t *testing.T) {
	t.Parallel()
	entries, evicted, err := appendLoaded(nil, "read_file", "SCHEMA-A", 1000)
	if err != nil {
		t.Fatalf("appendLoaded() error = %v, want nil", err)
	}
	entries, evicted, err = appendLoaded(entries, "read_file", "SCHEMA-A", 1000)
	if err != nil {
		t.Fatalf("second appendLoaded() error = %v, want nil", err)
	}
	if len(entries) != 1 {
		t.Errorf("len = %d, want 1: loading the same capability twice must replace, not accumulate", len(entries))
	}
	if len(evicted) != 0 {
		t.Errorf("evicted = %v, want none", evicted)
	}
}

func TestAppendLoadedEvictsOldestAndReportsIt(t *testing.T) {
	t.Parallel()
	entries, _, err := appendLoaded(nil, "first", strings.Repeat("a", 400), 1000)
	if err != nil {
		t.Fatalf("appendLoaded(first) error = %v", err)
	}
	entries, _, err = appendLoaded(entries, "second", strings.Repeat("b", 400), 1000)
	if err != nil {
		t.Fatalf("appendLoaded(second) error = %v", err)
	}

	entries, evicted, err := appendLoaded(entries, "third", strings.Repeat("c", 400), 1000)
	if err != nil {
		t.Fatalf("appendLoaded(third) error = %v", err)
	}
	if len(evicted) == 0 {
		t.Fatal("evicted = none, want the oldest entry to be reported")
	}
	if evicted[0] != "first" {
		t.Errorf("evicted[0] = %q, want %q (least recently loaded)", evicted[0], "first")
	}
	for _, e := range entries {
		if e.name == "first" {
			t.Error("evicted entry is still present")
		}
	}
}

func TestAppendLoadedRejectsOversizedDetail(t *testing.T) {
	t.Parallel()
	// 单个正文就超过整个区块上限时,驱逐再多也放不下。截断的 schema 是非法
	// JSON、截断的技能正文是残缺指令,两者都比明确失败更糟。
	_, _, err := appendLoaded(nil, "huge", strings.Repeat("x", 2000), 1000)
	if err == nil {
		t.Fatal("appendLoaded() error = nil, want an error naming the oversized capability")
	}
	if !strings.Contains(err.Error(), "huge") {
		t.Errorf("error = %q, want it to name the capability", err)
	}
}

func TestRenderLoadedStatesEvictions(t *testing.T) {
	t.Parallel()
	got := renderLoaded([]loadedEntry{{name: "read_file", detail: "SCHEMA"}})
	if !strings.Contains(got, "read_file") || !strings.Contains(got, "SCHEMA") {
		t.Errorf("renderLoaded() = %q, want it to carry the loaded detail", got)
	}
}

func TestComposePromptTrimsToolOutputNotLoadedBlock(t *testing.T) {
	t.Parallel()
	base := "BASE-PROMPT"
	loaded := []loadedEntry{{name: "read_file", detail: "LOADED-SCHEMA-MARKER"}}
	toolCtx := []toolEntry{{key: "k", text: strings.Repeat("t", 5000)}}

	got := composePrompt(base, loaded, toolCtx, 1000)

	if !strings.Contains(got, "BASE-PROMPT") {
		t.Error("base prompt was trimmed: the task framing must survive")
	}
	if !strings.Contains(got, "LOADED-SCHEMA-MARKER") {
		t.Error("loaded block was trimmed: a schema that silently vanishes leaves the model calling from memory")
	}
	if len([]rune(got)) > 1000+len([]rune(base))+len([]rune(renderLoaded(loaded))) {
		t.Errorf("composed prompt is %d runes, larger than the budget allows", len([]rune(got)))
	}
}
