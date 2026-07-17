package quality

import (
	"context"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

func TestAegisReviewerRejectsUnsafeResult(t *testing.T) {
	t.Parallel()

	reviewer := NewAegisReviewer()
	result, err := reviewer.Review(context.Background(), domain.TaskRun{
		Result: "unsafe: delete workspace",
	})
	if err != nil {
		t.Fatalf("Review() error = %v, want nil", err)
	}
	if result.Approved {
		t.Errorf("Review() approved = true, want false")
	}
	if result.RiskLevel != RiskHigh {
		t.Errorf("Review() risk = %q, want %q", result.RiskLevel, RiskHigh)
	}
}

// TestAegisReviewerApprovesIncidentalUnsafeMention guards against the previous
// false-positive: a legitimate answer that merely mentions the word "unsafe"
// (e.g. describing a security scanner's "unsafe_shell" rule) must be approved,
// not rejected.
func TestAegisReviewerApprovesIncidentalUnsafeMention(t *testing.T) {
	t.Parallel()

	reviewer := NewAegisReviewer()
	for _, result := range []string{
		"安全扫描规则 unsafe_shell 检测 sudo、curl | sh 等不安全 shell 命令",
		"The scanner flags unsafe shell usage as a warning-level finding.",
	} {
		got, err := reviewer.Review(context.Background(), domain.TaskRun{Result: result})
		if err != nil {
			t.Fatalf("Review() error = %v, want nil", err)
		}
		if !got.Approved {
			t.Errorf("Review(%q) approved = false, want true (incidental mention must not be rejected)", result)
		}
	}
}
