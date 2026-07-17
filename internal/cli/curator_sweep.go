package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/stardust/legion-agent/internal/skill"
	"github.com/stardust/legion-agent/internal/task"
)

// newSkillCuratorSweepJob builds a background job that periodically runs the
// skill Curator's deterministic, zero-token lifecycle sweep. The sweep ages idle
// workspace skills through stale into archived and never deletes anything; a
// sweep failure is returned so the scheduler logs it rather than silently
// skipping maintenance. A nil curator makes the job a no-op (curator wiring is
// only present when a persistent skill repository is available).
func newSkillCuratorSweepJob(curator *skill.Curator, now func() time.Time) task.BackgroundJob {
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if curator == nil {
			return nil
		}
		if _, err := curator.Sweep(ctx, now()); err != nil {
			return fmt.Errorf("skill curator sweep: %w", err)
		}
		return nil
	}
}
