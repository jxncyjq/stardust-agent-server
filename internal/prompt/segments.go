// Package prompt holds the plugin-contributed blocks of the system prompt.
//
// It is its own package for one structural reason: the plugin host WRITES
// segments (at activation) and the cognitive core READS them (when it builds a
// prompt). Putting the store in either of those packages would make the other
// import it, and internal/cognitive importing internal/plugin/host — or the
// reverse — is a dependency neither of them should have.
package prompt

import (
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/stardust/legion-agent/internal/lifecycle"
)

// MaxSegmentRunes and MaxTotalRunes bound what plugins may add to every
// prompt this deployment sends.
//
// They are counted in RUNES rather than bytes because the budget being
// protected is the model's context, and a CJK deployment would otherwise get a
// third of the segment an ASCII one gets for the same limit.
//
// The caps exist because this text is paid for on EVERY inference, forever: a
// plugin that writes two pages does not cost two pages once, it costs them per
// task for as long as it stays mounted. Overrunning is not an error though —
// the segment is truncated (visibly) or refused (loudly), and the plugin keeps
// working. A plugin unloaded over prompt length would be a deployment broken
// by a formatting mistake.
const (
	MaxSegmentRunes = 2048
	MaxTotalRunes   = 8192
)

// truncationMarker is appended to a segment that did not fit. It is IN THE
// TEXT, not only in the log, because the two are read by different people: the
// operator reads the log, and the plugin author reading a rendered prompt has
// to be able to see that their block was cut rather than that the model
// ignored it.
const truncationMarker = "\n…(truncated by the host: this plugin's prompt segment exceeded its size limit)"

// segment is one plugin's contribution.
type segment struct {
	plugin string
	text   string
}

// Segments is the set of plugin-contributed prompt blocks currently mounted.
//
// It is safe for concurrent use: activation adds and removes segments while
// tasks render them.
type Segments struct {
	mu     sync.RWMutex
	items  []*segment
	logger *slog.Logger
}

// NewSegments returns an empty set. A nil logger discards the warnings about
// truncated and refused segments — valid for a test, and never what a
// deployment wants.
func NewSegments(logger *slog.Logger) *Segments {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Segments{logger: logger}
}

// Add registers pluginName's segment and returns the function that removes it.
//
// An empty (or whitespace-only) text registers nothing and returns a no-op:
// rendering an empty fence would spend tokens on every inference to say that a
// plugin had nothing to say.
//
// Text longer than MaxSegmentRunes is truncated, visibly and with a warning.
// The revoke function is idempotent, matching the other plugin seams.
func (s *Segments) Add(pluginName, text string) func() {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return func() {}
	}
	if runes := []rune(trimmed); len(runes) > MaxSegmentRunes {
		s.logger.Warn("plugin prompt segment truncated",
			"component", "prompt",
			"plugin", pluginName,
			"runes", len(runes),
			"limit", MaxSegmentRunes,
			"consequence", "the model sees the beginning of this segment and a truncation marker, not the whole of it")
		trimmed = string(runes[:MaxSegmentRunes]) + truncationMarker
	}

	entry := &segment{plugin: pluginName, text: trimmed}
	s.mu.Lock()
	s.items = append(s.items, entry)
	s.mu.Unlock()

	var once bool
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if once {
			return
		}
		once = true
		for i, candidate := range s.items {
			if candidate == entry {
				s.items = append(s.items[:i], s.items[i+1:]...)
				return
			}
		}
	}
}

// Render returns the block that goes into the system prompt, or "" when no
// plugin contributed anything.
//
// Three properties this function exists to guarantee:
//
//  1. Every segment is FENCED with markers naming its plugin and saying it is
//     untrusted. This text was written by a deployment-installed plugin and
//     reaches the model as part of the system prompt; without the fence, a
//     plugin writing "ignore the previous instructions" is indistinguishable
//     from the host saying it.
//  2. The order is by PLUGIN NAME, not registration order. This block lives in
//     the cache-stable prefix, so two runs of the same deployment must render
//     byte-identically — and mount order is not something a deployment
//     controls.
//  3. The total is bounded. Segments are admitted in the same sorted order
//     until MaxTotalRunes is reached; whatever does not fit is REFUSED with a
//     warning naming it, never quietly dropped.
func (s *Segments) Render() string {
	s.mu.RLock()
	items := make([]*segment, len(s.items))
	copy(items, s.items)
	s.mu.RUnlock()

	if len(items) == 0 {
		return ""
	}
	sort.Slice(items, func(i, j int) bool { return items[i].plugin < items[j].plugin })

	var out strings.Builder
	used := 0
	for _, item := range items {
		cost := len([]rune(item.text))
		if used+cost > MaxTotalRunes {
			s.logger.Warn("plugin prompt segment did not fit the total budget",
				"component", "prompt",
				"plugin", item.plugin,
				"runes", cost,
				"used", used,
				"limit", MaxTotalRunes,
				"consequence", "this plugin's prompt segment is not in the system prompt at all")
			continue
		}
		used += cost
		out.WriteString(openingFence(item.plugin))
		out.WriteString(item.text)
		if !strings.HasSuffix(item.text, "\n") {
			out.WriteString("\n")
		}
		out.WriteString(closingFence(item.plugin))
	}
	return out.String()
}

// openingFence and closingFence spell the boundary markers in one place. The
// wording is deliberate: it names the plugin (so a reader can attribute the
// instruction) and says untrusted (so the model can weigh it against the
// host's own instructions).
func openingFence(pluginName string) string {
	return "--- plugin \"" + pluginName + "\" (untrusted, provided by a deployment-installed plugin) ---\n"
}

func closingFence(pluginName string) string {
	return "--- end plugin \"" + pluginName + "\" ---\n"
}

// ContributeOwned adds a segment and files its removal in the ledger under
// owner, so disposing the owner takes the text out of the prompt.
//
// It is to Add what tool.RegisterOwned is to Register: a plugin whose tools
// were withdrawn but whose text still steers the model is a plugin the
// deployment believes it has disabled.
func ContributeOwned(
	ledger *lifecycle.Ledger,
	owner lifecycle.Owner,
	segments *Segments,
	pluginName string,
	text string,
) func() error {
	revoke := segments.Add(pluginName, text)
	return ledger.Add(owner, "prompt:"+pluginName, func() error {
		revoke()
		return nil
	})
}
