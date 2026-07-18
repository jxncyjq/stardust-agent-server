package tool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

var (
	ErrPermissionDenied = errors.New("permission denied")
	ErrToolNotFound     = errors.New("tool not found")
)

type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
)

type Policy interface {
	Decide(agent domain.Agent, call domain.ToolCall) Decision
}

type PermissionEnforcer interface {
	Check(agent domain.Agent, call domain.ToolCall) error
}

type Guardrails interface {
	Before(ctx context.Context, call domain.ToolCall) error
	After(ctx context.Context, call domain.ToolCall, result domain.ToolResult) error
}

type Handler interface {
	Execute(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error)
}

type HandlerFunc func(context.Context, domain.ToolCall) (domain.ToolResult, error)

func (f HandlerFunc) Execute(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
	return f(ctx, call)
}

type Registry struct {
	policy    Policy
	enforcer  PermissionEnforcer
	guards    Guardrails
	audit     port.AuditLog
	sanitizer port.OutputSanitizer
	handlers  map[string]Handler
	describes map[string]Descriptor
}

func NewRegistry(policy Policy, enforcer PermissionEnforcer, guards Guardrails) *Registry {
	return &Registry{
		policy:    policy,
		enforcer:  enforcer,
		guards:    guards,
		handlers:  make(map[string]Handler),
		describes: make(map[string]Descriptor),
	}
}

func (r *Registry) Register(name string, handler Handler) {
	r.RegisterDescriptor(Descriptor{Name: name}, handler)
}

func (r *Registry) RegisterDescriptor(descriptor Descriptor, handler Handler) {
	r.handlers[descriptor.Name] = handler
	r.describes[descriptor.Name] = descriptor
}

// Subset returns a new registry that shares this registry's policy, enforcer,
// guardrails, audit log, and sanitizer but exposes only the named tools. Names
// with no matching handler are ignored. It backs delegated sub-agents that must
// run with a narrowed tool set instead of inheriting the full parent registry.
func (r *Registry) Subset(names ...string) *Registry {
	sub := NewRegistry(r.policy, r.enforcer, r.guards)
	sub.audit = r.audit
	sub.sanitizer = r.sanitizer
	allow := make(map[string]bool, len(names))
	for _, name := range names {
		allow[name] = true
	}
	for name, handler := range r.handlers {
		if allow[name] {
			sub.handlers[name] = handler
			sub.describes[name] = r.describes[name]
		}
	}
	return sub
}

func (r *Registry) Descriptors() []Descriptor {
	descriptors := make([]Descriptor, 0, len(r.describes))
	for _, descriptor := range r.describes {
		descriptors = append(descriptors, descriptor)
	}
	return descriptors
}

// SafeToolNames 返回已注册工具中 NOT 敏感（且非 lazy 协议 meta 工具）的排序名。
// Plan 模式恰好提供这个集合，使规划运行无法触及有副作用工具。
func (r *Registry) SafeToolNames() []string {
	names := make([]string, 0, len(r.describes))
	for name, descriptor := range r.describes {
		if descriptor.Sensitive {
			continue
		}
		if name == "list_tools" || name == "call_tool" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) WithAuditLog(audit port.AuditLog) *Registry {
	r.audit = audit
	return r
}

func (r *Registry) WithOutputSanitizer(sanitizer port.OutputSanitizer) *Registry {
	r.sanitizer = sanitizer
	return r
}

func (r *Registry) Execute(ctx context.Context, agent domain.Agent, call domain.ToolCall) (domain.ToolResult, error) {
	handler, ok := r.handlers[call.Name]
	if !ok {
		return domain.ToolResult{}, fmt.Errorf("%w: %s", ErrToolNotFound, call.Name)
	}
	descriptor := r.describes[call.Name]
	if call.RiskLevel == "" {
		call.RiskLevel = descriptor.RiskLevel
	}
	if err := validateInputSchema(descriptor.InputSchema, call.Arguments); err != nil {
		return domain.ToolResult{}, err
	}
	if r.enforcer != nil {
		if err := r.enforcer.Check(agent, call); err != nil {
			return domain.ToolResult{}, err
		}
	}
	if r.policy != nil && r.policy.Decide(agent, call) == DecisionDeny {
		return domain.ToolResult{}, ErrPermissionDenied
	}
	if r.guards != nil {
		if err := r.guards.Before(ctx, call); err != nil {
			return domain.ToolResult{}, err
		}
	}
	execCtx := ctx
	if descriptor.Timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, descriptor.Timeout)
		defer cancel()
	}
	result, err := handler.Execute(execCtx, call)
	if err != nil {
		if auditErr := r.appendAudit(ctx, agent, call, "tool_failed"); auditErr != nil {
			return domain.ToolResult{}, auditErr
		}
		return domain.ToolResult{}, err
	}
	if r.guards != nil {
		if err := r.guards.After(ctx, call, result); err != nil {
			if auditErr := r.appendAudit(ctx, agent, call, "tool_failed"); auditErr != nil {
				return domain.ToolResult{}, auditErr
			}
			return domain.ToolResult{}, err
		}
	}
	if r.sanitizer != nil {
		result.Output = r.sanitizer.MarkdownInline(result.Output)
		result.Error = r.sanitizer.MarkdownInline(result.Error)
	}
	if err := r.appendAudit(ctx, agent, call, "tool_executed"); err != nil {
		return domain.ToolResult{}, err
	}
	return result, nil
}

func (r *Registry) appendAudit(ctx context.Context, agent domain.Agent, call domain.ToolCall, action string) error {
	if r.audit == nil {
		return nil
	}
	callID := call.ID
	if callID == "" {
		callID = call.Name
	}
	if err := r.audit.Append(ctx, domain.AuditEvent{
		ID:          callID + ":" + action,
		RequestID:   callID,
		SubjectType: "tool",
		SubjectID:   call.Name,
		Action:      action,
		Hash:        agent.ID,
		CreatedAt:   time.Now(),
	}); err != nil {
		return fmt.Errorf("append %s audit event: %w", action, err)
	}
	return nil
}

type StaticPolicy struct {
	decision Decision
}

func NewStaticPolicy(decision Decision) StaticPolicy {
	return StaticPolicy{decision: decision}
}

func (p StaticPolicy) Decide(domain.Agent, domain.ToolCall) Decision {
	return p.decision
}

type RolePermissionEnforcer struct {
	allowed map[string]bool
}

func NewRolePermissionEnforcer(allowed map[string]bool) RolePermissionEnforcer {
	return RolePermissionEnforcer{allowed: allowed}
}

func (e RolePermissionEnforcer) Check(agent domain.Agent, call domain.ToolCall) error {
	if !e.allowed[agent.Role+":"+call.Name] {
		return ErrPermissionDenied
	}
	return nil
}

type NoopGuardrails struct{}

func (NoopGuardrails) Before(ctx context.Context, _ domain.ToolCall) error {
	return ctx.Err()
}

func (NoopGuardrails) After(ctx context.Context, _ domain.ToolCall, _ domain.ToolResult) error {
	return ctx.Err()
}

type PathGuard interface {
	Check(ctx context.Context, path string) (string, error)
}

type PathGuardrails struct {
	guard PathGuard
	keys  []string
}

func NewPathGuardrails(guard PathGuard, keys ...string) PathGuardrails {
	return PathGuardrails{guard: guard, keys: keys}
}

func (g PathGuardrails) Before(ctx context.Context, call domain.ToolCall) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, key := range g.keys {
		value, ok := call.Arguments[key]
		if !ok || value == "" {
			continue
		}
		if _, err := g.guard.Check(ctx, value); err != nil {
			return fmt.Errorf("check tool path %q: %w", key, err)
		}
	}
	return nil
}

func (g PathGuardrails) After(ctx context.Context, _ domain.ToolCall, _ domain.ToolResult) error {
	return ctx.Err()
}

var _ PathGuard = port.WorkspacePathGuard{}
