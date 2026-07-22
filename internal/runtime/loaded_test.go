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

func TestAppendLoadedRejectsWhenSoleSurvivorStillExceedsBudget(t *testing.T) {
	t.Parallel()
	// detail(990) 单独 <= maxChars(1000),入口校验放行;但 name(30)+detail(990)=1020
	// 才是驱逐循环真正比较的 loadedSize 口径。驱逐到只剩这一条时循环因
	// len(kept)>1 为假而停止,若函数此时返回 nil error,就是把一个明知超预算
	// 的区块当作正常结果静默交还给调用方 —— 违反 fail-loud。
	name := "a-fairly-long-capability-name"
	detail := strings.Repeat("x", 990)
	_, _, err := appendLoaded(nil, name, detail, 1000)
	if err == nil {
		t.Fatal("appendLoaded() error = nil, want an error: name+detail exceeds maxChars even though detail alone does not")
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
	const maxPromptChars = 300 // headLen = maxPromptChars/3 = 100 inside boundPrompt

	// base must be long enough on its own to run past boundPrompt's headLen
	// (100 runes), so that under a merged single-budget boundPrompt(base+
	// loadedBlock+toolCtx, maxPromptChars) call, the loaded block that follows
	// it starts *after* the kept head window. The marker text itself sits at
	// the very front of base (within the head window), so it always survives
	// regardless of implementation -- it is the loaded block's marker below
	// that has to discriminate the two implementations.
	base := "BASE-PROMPT" + strings.Repeat("z", 200) // 211 runes > headLen(100)
	loaded := []loadedEntry{{name: "read_file", detail: "LOADED-SCHEMA-MARKER"}}
	// toolCtx must be large enough that it alone spans the merged boundPrompt's
	// tail window (tailLen = maxPromptChars-headLen = 200 runes), so the tail
	// kept by a merged boundPrompt call is carved entirely out of toolCtx and
	// never reaches back into the loaded block that precedes it.
	toolCtx := []toolEntry{{key: "k", text: strings.Repeat("t", 5000)}}

	got := composePrompt(base, loaded, toolCtx, maxPromptChars)

	if !strings.Contains(got, "BASE-PROMPT") {
		t.Error("base prompt was trimmed: the task framing must survive")
	}
	// This is the assertion with discriminating power: under a merged single-
	// budget boundPrompt(base+loadedBlock+toolCtx, maxPromptChars) call, the
	// loaded block falls in the dropped middle (base alone already exceeds
	// headLen, and toolCtx alone already exceeds tailLen), so the marker is
	// lost. Only a true three-part composePrompt -- which never trims base or
	// the loaded block -- keeps it.
	if !strings.Contains(got, "LOADED-SCHEMA-MARKER") {
		t.Error("loaded block was trimmed: a schema that silently vanishes leaves the model calling from memory")
	}
	if len([]rune(got)) > maxPromptChars+len([]rune(base))+len([]rune(renderLoaded(loaded))) {
		t.Errorf("composed prompt is %d runes, larger than the budget allows", len([]rune(got)))
	}
}
