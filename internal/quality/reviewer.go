package quality

import (
	"context"
	"strings"

	"github.com/stardust/legion-agent/internal/domain"
)

type RiskLevel string

const (
	RiskLow  RiskLevel = "low"
	RiskHigh RiskLevel = "high"
)

type ReviewResult struct {
	Approved  bool
	RiskLevel RiskLevel
	Reason    string
}

type AegisReviewer struct{}

func NewAegisReviewer() AegisReviewer {
	return AegisReviewer{}
}

// Review gates a task result. This is a placeholder safety reviewer pending a
// real content-review backend: it only rejects an explicit unsafe *directive*
// (the sentinel prefix "unsafe:"), not the incidental word "unsafe". The bare
// substring match previously failed any legitimate answer that merely mentioned
// the word — e.g. one describing a security scanner's "unsafe_shell" rule — so
// good, safe answers were marked failed. Matching the directive sentinel keeps
// the gate meaningful without those false rejections.
func (r AegisReviewer) Review(ctx context.Context, run domain.TaskRun) (ReviewResult, error) {
	if err := ctx.Err(); err != nil {
		return ReviewResult{}, err
	}
	if strings.Contains(strings.ToLower(run.Result), "unsafe:") {
		return ReviewResult{Approved: false, RiskLevel: RiskHigh, Reason: "unsafe output directive detected"}, nil
	}
	return ReviewResult{Approved: true, RiskLevel: RiskLow}, nil
}
