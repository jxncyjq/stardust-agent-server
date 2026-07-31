package cli

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/memory"
	"github.com/stardust/legion-agent/internal/port"
)

// maxDistilledEpisodeChars caps the stored episode content (distilled or raw
// fallback) so a runaway LLM response or an oversized raw task result never
// blows up the episodic store.
const maxDistilledEpisodeChars = 2000

// maxDistillInputChars caps the content fed into the distillation LLM prompt,
// separately from maxDistilledEpisodeChars (the output/fallback cap). It
// bounds the cost/latency of the distill call itself against an oversized raw
// task result, independent of how the output is later truncated.
const maxDistillInputChars = 4000

// defaultEpisodeRecordTimeout bounds how long the background distill+store
// work is allowed to run before it's abandoned. It must never block the task
// it is recording, so this is generous but finite.
const defaultEpisodeRecordTimeout = 30 * time.Second

// episodeRecorder implements runtime.EpisodeRecorder: it distills a finished
// task's outcome into a short memory via the Maas LLM client and stores it in
// the episodic store, entirely off the task's critical path.
//
// RecordEpisode (the interface method) is fire-and-forget: it starts a
// goroutine bounded by a timeout and returns immediately, so a slow or failing
// LLM/store never blocks or fails the task that just completed. The
// synchronous work lives in record, which is the unit tested directly —
// testing through the goroutine would be flaky.
type episodeRecorder struct {
	summarizer port.MaasInferenceClient
	store      memory.EpisodicStore
	logger     *slog.Logger
	timeout    time.Duration
}

// newEpisodeRecorder builds an episodeRecorder. summarizer may be nil, in
// which case record always falls back to storing (truncated) raw content
// without attempting distillation. logger falls back to slog.Default() when
// nil — never to silence, consistent with the runtime package's own Logger
// fallback (see runtime.Config.Logger).
func newEpisodeRecorder(summarizer port.MaasInferenceClient, store memory.EpisodicStore, logger *slog.Logger) *episodeRecorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &episodeRecorder{
		summarizer: summarizer,
		store:      store,
		logger:     logger,
		timeout:    defaultEpisodeRecordTimeout,
	}
}

// RecordEpisode implements runtime.EpisodeRecorder. It never blocks the
// caller and never panics out of the goroutine: a top-level recover logs any
// panic (fail-loud, not fail-silent) instead of crashing the process.
func (r *episodeRecorder) RecordEpisode(agent domain.Agent, task domain.Task, outcome string, content string) {
	go func() {
		defer func() {
			if p := recover(); p != nil {
				r.logger.Error("episode recorder panicked",
					"task_id", task.ID,
					"outcome", outcome,
					"panic", p,
				)
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
		defer cancel()

		if err := r.record(ctx, agent, task, outcome, content); err != nil {
			r.logger.Warn("episode recording failed",
				"task_id", task.ID,
				"outcome", outcome,
				"error", err,
			)
		}
	}()
}

// record does the synchronous distill+store work and is the unit-tested
// entry point. It falls back to storing truncated raw content whenever
// distillation is unavailable, errors, or returns an empty result — the
// episode must never be silently lost, only ever degraded.
func (r *episodeRecorder) record(ctx context.Context, agent domain.Agent, task domain.Task, outcome string, content string) error {
	distilled := content

	if r.summarizer != nil {
		// Pre-truncate the input independently of the maxDistilledEpisodeChars
		// output cap below: content is often st.resp.Text from a large task
		// result, and sending it unbounded into the distill prompt inflates this
		// call's own cost/latency before the output-side truncation ever runs.
		promptContent := truncateEpisodeContent(content, maxDistillInputChars)
		resp, err := r.summarizer.Generate(ctx, port.InferenceRequest{
			RequestID: "episode-distill:" + task.ID,
			Prompt:    distillInstruction(outcome) + "\n\n" + promptContent,
		})
		if err != nil {
			r.logger.Warn("episode distillation failed, falling back to raw content",
				"task_id", task.ID,
				"outcome", outcome,
				"error", err,
			)
		} else if text := strings.TrimSpace(resp.Text); text != "" {
			distilled = text
		} else {
			r.logger.Warn("episode distillation returned empty result, falling back to raw content",
				"task_id", task.ID,
				"outcome", outcome,
			)
		}
	}

	distilled = truncateEpisodeContent(distilled, maxDistilledEpisodeChars)

	if _, err := r.store.Add(ctx, agent, task, distilled); err != nil {
		return fmt.Errorf("add episodic entry: %w", err)
	}
	return nil
}

// distillInstruction builds the LLM prompt instruction for compressing a
// finished task's process and result into a short, retrieval-friendly memory.
func distillInstruction(outcome string) string {
	return "把下面这次任务的过程与结果蒸馏成一条简洁的长期记忆（1-3 句，" +
		"保留关键事实/结论；若为失败，写明失败原因），供未来相似任务检索参考。" +
		"只输出记忆正文，不要解释。任务结果状态：" + outcome
}

// truncateEpisodeContent trims value to at most max runes, appending a marker
// when truncation occurred. Rune-based so multi-byte (CJK) content is never
// cut mid-character.
func truncateEpisodeContent(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max]) + "\n[truncated]"
}
