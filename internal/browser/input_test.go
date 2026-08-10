package browser

import (
	"math"
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/input"
)

func TestValidateInputEventsOK(t *testing.T) {
	err := validateInputEvents([]InputEvent{
		{Type: "mousemove", X: 0.5, Y: 0.5},
		{Type: "click", X: 0, Y: 1, Button: "left"},
		{Type: "wheel", X: 0.2, Y: 0.3, DeltaY: 120},
		{Type: "keydown", Key: "Enter"},
		{Type: "char", Text: "hello"},
	})
	if err != nil {
		t.Fatalf("valid batch rejected: %v", err)
	}
}

func TestValidateInputEventsRejects(t *testing.T) {
	cases := map[string]InputEvent{
		"unknown type":  {Type: "drag", X: 0.1, Y: 0.1},
		"x below range": {Type: "mousemove", X: -0.01, Y: 0.5},
		"y above range": {Type: "mousemove", X: 0.5, Y: 1.01},
		"bad button":    {Type: "mousedown", X: 0.5, Y: 0.5, Button: "sideways"},
		"key too long":  {Type: "keydown", Key: strings.Repeat("a", maxKeyLen+1)},
		"text too long": {Type: "char", Text: strings.Repeat("z", maxTextLen+1)},
	}
	for name, ev := range cases {
		if err := validateInputEvents([]InputEvent{ev}); err == nil {
			t.Errorf("%s: expected rejection, got nil", name)
		}
	}
}

func TestValidateInputEventsRejectsUnmappableKey(t *testing.T) {
	if err := validateInputEvents([]InputEvent{
		{Type: "keydown", Key: "F13Nonsense"},
	}); err == nil {
		t.Fatal("unmappable key should be rejected before injection")
	}
}

func TestValidateInputEventsRejectsMixedBatchOnUnmappableKey(t *testing.T) {
	// Documents whole-batch-reject: the click must never be applied because
	// validation runs (and fails) before any event is injected.
	if err := validateInputEvents([]InputEvent{
		{Type: "click", X: 0.5, Y: 0.5},
		{Type: "keydown", Key: "NotAKey"},
	}); err == nil {
		t.Fatal("batch with unmappable key should be rejected as a whole")
	}
}

func TestValidateInputEventsRejectsNonFiniteWheelDelta(t *testing.T) {
	if err := validateInputEvents([]InputEvent{
		{Type: "wheel", X: 0.5, Y: 0.5, DeltaY: math.Inf(1)},
	}); err == nil {
		t.Fatal("wheel with +Inf DeltaY should be rejected")
	}
	if err := validateInputEvents([]InputEvent{
		{Type: "wheel", X: 0.5, Y: 0.5, DeltaX: math.NaN()},
	}); err == nil {
		t.Fatal("wheel with NaN DeltaX should be rejected")
	}
}

func TestValidateInputEventsRejectsEmptyCharText(t *testing.T) {
	if err := validateInputEvents([]InputEvent{
		{Type: "char", Text: ""},
	}); err == nil {
		t.Fatal("char with empty text should be rejected")
	}
}

func TestValidateInputEventsBatchCap(t *testing.T) {
	big := make([]InputEvent, maxInputBatch+1)
	for i := range big {
		big[i] = InputEvent{Type: "mousemove", X: 0.5, Y: 0.5}
	}
	if err := validateInputEvents(big); err == nil {
		t.Fatal("expected batch-size rejection")
	}
}

func TestValidateInputEventsEmpty(t *testing.T) {
	if err := validateInputEvents(nil); err == nil {
		t.Fatal("empty batch should be rejected (nothing to inject)")
	}
}

func TestKeyToInputKey(t *testing.T) {
	if k, err := keyToInputKey("Enter"); err != nil || k != input.Enter {
		t.Fatalf("Enter map: %v %v", k, err)
	}
	if k, err := keyToInputKey("a"); err != nil || k != input.Key('a') {
		t.Fatalf("printable map: %v %v", k, err)
	}
	if _, err := keyToInputKey("F13Nonsense"); err == nil {
		t.Fatal("unknown key should error")
	}
	if _, err := keyToInputKey(""); err == nil {
		t.Fatal("empty key should error")
	}
}
