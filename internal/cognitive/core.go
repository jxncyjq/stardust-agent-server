package cognitive

import (
	"context"
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/capability"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/memory"
	"github.com/stardust/legion-agent/internal/skill"
)

type Request struct {
	Agent             domain.Agent
	Task              domain.Task
	Tools             []string
	ConversationTurns []domain.ConversationTurn
	// Catalog, when non-nil, is the per-task capability catalog rendered into the
	// prompt. The runtime supplies one built from the run's effective (per-task,
	// Plan-scoped) tool registry so the prompt advertises exactly what that task
	// may load and dispatch. It takes precedence over the Core-level catalog set
	// by WithCatalog; when nil the Core-level catalog is used (standalone/tests).
	Catalog *capability.Catalog
}

type BuiltContext struct {
	Prompt     string
	Compressed bool
}

type MemoryProvider interface {
	SystemPromptBlock(ctx context.Context, agent domain.Agent) (string, error)
	Prefetch(ctx context.Context, task domain.Task) ([]domain.MemoryEntry, error)
}

type SkillProvider interface {
	SelectForTask(ctx context.Context, task domain.Task, maxSkills int) ([]skill.Injection, error)
}

type CapabilityMemoryProvider interface {
	SearchGenes(ctx context.Context, query memory.CapabilityQuery) ([]memory.GeneHit, error)
	SearchCapsules(ctx context.Context, query memory.CapabilityQuery) ([]memory.CapsuleHit, error)
}

type CompressionResult struct {
	Text       string
	Compressed bool
}

type Compressor interface {
	Compress(ctx context.Context, text string) (CompressionResult, error)
}

type Core struct {
	compressor       Compressor
	memory           MemoryProvider
	skills           SkillProvider
	catalog          *capability.Catalog
	capabilityMemory CapabilityMemoryProvider
	contextFiles     string
}

func NewCore(compressor Compressor) *Core {
	return &Core{compressor: compressor}
}

func (c *Core) WithMemory(memory MemoryProvider) *Core {
	c.memory = memory
	return c
}

// WithSkills attaches the skill selector.
//
// Deprecated for prompt building: skills now reach the model through the
// capability catalog (WithCatalog), which lists every skill rather than a
// keyword-matched top-N. SelectForTask is kept for the /skills query paths and
// for future catalog search scoring; it no longer injects anything into a
// prompt.
func (c *Core) WithSkills(skills SkillProvider) *Core {
	c.skills = skills
	return c
}

// WithCatalog attaches the capability catalog rendered into the prompt.
//
// The catalog lists every callable tool and loadable skill, one line each, so
// its rendering is the same for every task of the same agent -- unlike the
// previous keyword-matched skill selection, which changed the task framing per
// task and so missed the provider prompt cache on every task. The model pulls
// the full definition of whatever it needs with load_capabilities.
//
// Request.Catalog, when set, overrides this per task: the runtime passes a
// catalog scoped to the run's effective tool registry so the prompt matches
// exactly what that task may load and dispatch.
func (c *Core) WithCatalog(catalog *capability.Catalog) *Core {
	c.catalog = catalog
	return c
}

func (c *Core) WithCapabilityMemory(capabilityMemory CapabilityMemoryProvider) *Core {
	c.capabilityMemory = capabilityMemory
	return c
}

func (c *Core) WithContextFiles(contextFiles string) *Core {
	c.contextFiles = strings.TrimSpace(contextFiles)
	return c
}

func (c *Core) BuildContext(ctx context.Context, req Request) (BuiltContext, error) {
	if err := ctx.Err(); err != nil {
		return BuiltContext{}, err
	}
	memoryBlock, err := c.memoryBlock(ctx, req)
	if err != nil {
		return BuiltContext{}, err
	}
	capabilityBlock, err := c.capabilityBlock(ctx, req)
	if err != nil {
		return BuiltContext{}, err
	}
	catalogBlock, err := c.catalogBlock(ctx, req)
	if err != nil {
		return BuiltContext{}, err
	}
	prompt := fmt.Sprintf(
		"Agent: %s\nRole: %s\nTask: %s\nInput: %s\nTools: %s\n",
		req.Agent.ID,
		req.Agent.Role,
		req.Task.ID,
		req.Task.Input,
		strings.Join(req.Tools, ", "),
	)
	if memoryBlock != "" {
		prompt += memoryBlock
	}
	if conversationBlock := conversationBlock(req.ConversationTurns); conversationBlock != "" {
		prompt += conversationBlock
	}
	if c.contextFiles != "" {
		prompt += "Runtime context files:\n"
		prompt += c.contextFiles
		prompt += "\n"
	}
	if capabilityBlock != "" {
		prompt += capabilityBlock
	}
	if catalogBlock != "" {
		prompt += catalogBlock
	}
	result, err := c.compressor.Compress(ctx, prompt)
	if err != nil {
		return BuiltContext{}, fmt.Errorf("compress context: %w", err)
	}
	return BuiltContext{Prompt: result.Text, Compressed: result.Compressed}, nil
}

func (c *Core) memoryBlock(ctx context.Context, req Request) (string, error) {
	if c.memory == nil {
		return "", nil
	}
	systemBlock, err := c.memory.SystemPromptBlock(ctx, req.Agent)
	if err != nil {
		return "", fmt.Errorf("build memory system block: %w", err)
	}
	prefetched, err := c.memory.Prefetch(ctx, req.Task)
	if err != nil {
		return "", fmt.Errorf("prefetch memory: %w", err)
	}
	var b strings.Builder
	if systemBlock != "" {
		b.WriteString("\n")
		b.WriteString(systemBlock)
		b.WriteString("\n")
	}
	if len(prefetched) > 0 {
		b.WriteString("Prefetched memory:\n")
		for _, entry := range prefetched {
			b.WriteString("- ")
			b.WriteString(entry.Content)
			b.WriteString("\n")
		}
	}
	return b.String(), nil
}

func conversationBlock(turns []domain.ConversationTurn) string {
	if len(turns) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Recent conversation:\n")
	for _, turn := range turns {
		role := string(turn.Role)
		if role == "" {
			role = "unknown"
		}
		b.WriteString("- ")
		b.WriteString(role)
		if turn.Role == domain.ConversationRoleAssistant {
			meta := strings.TrimSpace(turn.AgentID)
			if strings.TrimSpace(turn.ModelProfile) != "" {
				if meta != "" {
					meta += "/"
				}
				meta += strings.TrimSpace(turn.ModelProfile)
			}
			if meta != "" {
				b.WriteString("(")
				b.WriteString(meta)
				b.WriteString(")")
			}
		}
		b.WriteString(": ")
		b.WriteString(strings.TrimSpace(turn.Content))
		b.WriteString("\n")
	}
	return b.String()
}

func (c *Core) capabilityBlock(ctx context.Context, req Request) (string, error) {
	if c.capabilityMemory == nil {
		return "", nil
	}
	query := memory.CapabilityQuery{
		Text: req.Task.Input,
		Tags: queryTags(req.Task.Input),
		TopK: 3,
	}
	genes, err := c.capabilityMemory.SearchGenes(ctx, query)
	if err != nil {
		return "", fmt.Errorf("search capability genes: %w", err)
	}
	capsules, err := c.capabilityMemory.SearchCapsules(ctx, query)
	if err != nil {
		return "", fmt.Errorf("search capability capsules: %w", err)
	}
	if len(genes) == 0 && len(capsules) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("Capability memory:\n")
	for _, hit := range genes {
		b.WriteString("- Gene ")
		b.WriteString(hit.Gene.ID)
		b.WriteString(": ")
		b.WriteString(hit.Gene.Plan)
		if hit.Gene.Avoid != "" {
			b.WriteString(" Avoid: ")
			b.WriteString(hit.Gene.Avoid)
		}
		b.WriteString("\n")
	}
	for _, hit := range capsules {
		b.WriteString("- Capsule ")
		b.WriteString(hit.Capsule.ID)
		b.WriteString(": ")
		b.WriteString(hit.Capsule.Outcome)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// catalogBlock renders the capability catalog into the prompt.
//
// It replaces the previous keyword-matched skill injection, which scored skills
// against task.Input, took the top three, and fell back to injecting a skill's
// entire body when it declared no summary. That made the task framing different
// for every task -- so the provider prompt cache missed on every task -- and let
// the system guess on the model's behalf. The catalog lists everything in one
// line each; the model pulls what it wants with load_capabilities.
//
// Request.Catalog (the per-task, effective-registry-scoped catalog the runtime
// supplies) takes precedence over the Core-level catalog set by WithCatalog.
func (c *Core) catalogBlock(ctx context.Context, req Request) (string, error) {
	catalog := req.Catalog
	if catalog == nil {
		catalog = c.catalog
	}
	if catalog == nil {
		return "", nil
	}
	entries, err := catalog.Entries(ctx)
	if err != nil {
		return "", fmt.Errorf("build capability catalog: %w", err)
	}
	return capability.Render(entries), nil
}

func queryTags(input string) []string {
	fields := strings.Fields(strings.ToLower(input))
	tags := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		field = strings.Trim(field, ".,;:!?()[]{}\"'")
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		tags = append(tags, field)
	}
	return tags
}

type NoopCompressor struct{}

func (NoopCompressor) Compress(ctx context.Context, text string) (CompressionResult, error) {
	if err := ctx.Err(); err != nil {
		return CompressionResult{}, err
	}
	return CompressionResult{Text: text}, nil
}

type ThresholdCompressor struct {
	limit int
}

func NewThresholdCompressor(limit int) ThresholdCompressor {
	return ThresholdCompressor{limit: limit}
}

func (c ThresholdCompressor) Compress(ctx context.Context, text string) (CompressionResult, error) {
	if err := ctx.Err(); err != nil {
		return CompressionResult{}, err
	}
	if c.limit <= 0 || len(text) <= c.limit {
		return CompressionResult{Text: text}, nil
	}
	summary := text
	if len(summary) > c.limit {
		summary = summary[:c.limit]
	}
	return CompressionResult{
		Text:       "compressed context: " + summary,
		Compressed: true,
	}, nil
}
