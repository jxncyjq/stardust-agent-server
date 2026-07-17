package cognitive

import (
	"context"
	"fmt"
	"strings"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/memory"
	"github.com/stardust/legion-agent/internal/skill"
)

type Request struct {
	Agent             domain.Agent
	Task              domain.Task
	Tools             []string
	ConversationTurns []domain.ConversationTurn
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

func (c *Core) WithSkills(skills SkillProvider) *Core {
	c.skills = skills
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
	skillBlock, err := c.skillBlock(ctx, req)
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
	if skillBlock != "" {
		prompt += skillBlock
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

func (c *Core) skillBlock(ctx context.Context, req Request) (string, error) {
	if c.skills == nil {
		return "", nil
	}
	injections, err := c.skills.SelectForTask(ctx, req.Task, 3)
	if err != nil {
		return "", fmt.Errorf("select task skills: %w", err)
	}
	if len(injections) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("Mounted skills:\n")
	for _, injection := range injections {
		if !skill.IsInjectable(injection.Skill) {
			continue
		}
		b.WriteString("- ")
		b.WriteString(injection.Skill.ID)
		b.WriteString(" (")
		b.WriteString(injection.Skill.Name)
		b.WriteString("): ")
		b.WriteString(firstNonEmpty(injection.Skill.Summary, injection.Skill.Content))
		b.WriteString("\n")
	}
	return b.String(), nil
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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
