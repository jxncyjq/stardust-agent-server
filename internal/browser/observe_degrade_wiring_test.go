package browser

import (
	"context"
	"strings"
	"testing"
)

func TestDegrade_ThresholdZero_NoChange(t *testing.T) {
	full := strings.Repeat("[e1] <link> x\n", 500)
	got, err := DegradeObservation(context.Background(), Observation{Text: full}, "t", "/root",
		DegradeDeps{}, 0)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Text != full || got.Truncated {
		t.Fatalf("threshold 0 should not degrade")
	}
}
