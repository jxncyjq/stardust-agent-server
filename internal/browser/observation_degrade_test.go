package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeExtractor struct {
	out string
	err error
}

func (f fakeExtractor) Extract(_ context.Context, _, _ string) (string, error) {
	return f.out, f.err
}

type fakeArchive struct {
	rel      string
	err      error
	savedArg string
}

func (f *fakeArchive) Save(_, content string) (string, error) {
	f.savedArg = content
	return f.rel, f.err
}
func (f *fakeArchive) Cleanup(string, time.Duration) error { return nil }

func obsWithText(t string) Observation { return Observation{Text: t} }

func TestDegradeObservation_UnderThreshold_ReturnsAsIs(t *testing.T) {
	obs := obsWithText("short text")
	arch := &fakeArchive{rel: "x"}
	got, err := DegradeObservation(context.Background(), obs, "task", "/root",
		DegradeDeps{Archive: arch}, 100)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Text != "short text" || got.Truncated {
		t.Fatalf("got %+v, want unchanged untruncated", got)
	}
	if arch.savedArg != "" {
		t.Fatalf("archive.Save called under threshold; want not called")
	}
}

func TestDegradeObservation_NilExtractor_TruncatesWithPointer(t *testing.T) {
	full := strings.Repeat("[e1] <link> aaaaa\n", 50)
	arch := &fakeArchive{rel: ".legion/browser/snapshots/deadbeef.txt"}
	got, err := DegradeObservation(context.Background(), obsWithText(full), "task", "/root",
		DegradeDeps{Archive: arch}, 100)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !got.Truncated {
		t.Fatalf("Truncated = false, want true")
	}
	if !strings.Contains(got.Text, "read_file(path=\".legion/browser/snapshots/deadbeef.txt\")") {
		t.Fatalf("footer missing pointer, got:\n%s", got.Text)
	}
	if arch.savedArg != full {
		t.Fatalf("archive got wrong content")
	}
	if len([]rune(got.Text)) > 100+footerMaxRunes {
		t.Fatalf("truncated text still too long: %d runes", len([]rune(got.Text)))
	}
}

func TestDegradeObservation_Extractor_ReducesText(t *testing.T) {
	full := strings.Repeat("[e1] <link> noise\n", 50)
	arch := &fakeArchive{rel: "p.txt"}
	got, err := DegradeObservation(context.Background(), obsWithText(full), "buy milk", "/root",
		DegradeDeps{Extractor: fakeExtractor{out: "[e1] <button> Buy"}, Archive: arch}, 100)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !strings.HasPrefix(got.Text, "[e1] <button> Buy") {
		t.Fatalf("reduced text missing, got:\n%s", got.Text)
	}
}

func TestDegradeObservation_ExtractorError_HardFails(t *testing.T) {
	full := strings.Repeat("x\n", 200)
	arch := &fakeArchive{rel: "p.txt"}
	_, err := DegradeObservation(context.Background(), obsWithText(full), "task", "/root",
		DegradeDeps{Extractor: fakeExtractor{err: errors.New("boom")}, Archive: arch}, 100)
	if err == nil || !strings.Contains(err.Error(), "extract") {
		t.Fatalf("err = %v, want wrapped extract error", err)
	}
}

func TestDegradeObservation_ExtractorEmpty_HardFails(t *testing.T) {
	full := strings.Repeat("x\n", 200)
	arch := &fakeArchive{rel: "p.txt"}
	_, err := DegradeObservation(context.Background(), obsWithText(full), "task", "/root",
		DegradeDeps{Extractor: fakeExtractor{out: "   "}, Archive: arch}, 100)
	if err == nil {
		t.Fatalf("err = nil, want error on empty extraction")
	}
}

func TestDegradeObservation_ArchiveError_HardFails(t *testing.T) {
	full := strings.Repeat("x\n", 200)
	arch := &fakeArchive{err: errors.New("disk full")}
	_, err := DegradeObservation(context.Background(), obsWithText(full), "task", "/root",
		DegradeDeps{Archive: arch}, 100)
	if err == nil || !strings.Contains(err.Error(), "archive") {
		t.Fatalf("err = %v, want wrapped archive error", err)
	}
}

func TestTruncateByLine_KeepsWholeLines(t *testing.T) {
	in := "line-one\nline-two\nline-three\n"
	out := TruncateByLine(in, 12)
	if strings.Contains(out, "line-two") {
		t.Fatalf("cut mid-content, got %q", out)
	}
	if !strings.Contains(out, "line-one") {
		t.Fatalf("dropped first line, got %q", out)
	}
}
