package tool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/port"
)

var (
	ErrPermissionDenied = errors.New("permission denied")
	ErrToolNotFound     = errors.New("tool not found")
	// ErrInvalidInput reports that a call's arguments failed the tool's own
	// declared input schema. It is distinct from ErrPermissionDenied: the caller
	// was authorized to reach the tool, but what it sent could not be understood
	// against the schema (a required argument missing, a malformed schema).
	ErrInvalidInput = errors.New("invalid tool input")
)

type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
	// DecisionAsk requires a human to approve this call before it runs. Only a
	// Decider produces it (see decider.go): the host's own Policy answers allow
	// or deny, and the Sensitive-tool approval it drives is decided a layer
	// above, at the round boundary.
	DecisionAsk Decision = "ask"
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
	// parent is nil for a root registry. A derived view holds a REFERENCE to
	// its parent and resolves at call time, never a copy of its handlers: a
	// copy keeps answering after the parent revokes the tool.
	parent *Registry
	filter *filter

	policy    Policy
	enforcer  PermissionEnforcer
	guards    Guardrails
	audit     port.AuditLog
	sanitizer port.OutputSanitizer

	// mu guards handlers and describes. Registration used to happen only during
	// assembly; plugins register and revoke while the agent is running, so reads
	// on the execution path now race writes without it.
	mu        sync.RWMutex
	handlers  map[string]Handler
	describes map[string]Descriptor
	// observers are notified after a completed call (see observer.go). They
	// live under the same lock as the handler maps because registration and
	// revocation race the same way, but they are always COPIED before being
	// called — see notifyObservers for why holding the lock across a
	// notification would deadlock a plugin that unloads itself.
	observers []*registeredObserver
	// deciders are consulted BEFORE dispatch and may refuse a call. They live
	// beside observers, under the same lock and with the same copy-then-call
	// discipline, because they have the same reentrancy hazard — but they are
	// the opposite kind of seam: an observer cannot change anything, and a
	// decider exists precisely to.
	deciders []*registeredDecider
	// askArbiter answers, at dispatch time, whether a call a decider wants
	// approved HAS been approved. It is nil in a deployment with no approval
	// machinery, and an ask is then a refusal — see resolveVerdict.
	askArbiter AskArbiter
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

// Register adds a tool under name and returns its revoke function. See
// RegisterDescriptor for the duplicate-name contract.
func (r *Registry) Register(name string, handler Handler) func() {
	return r.RegisterDescriptor(Descriptor{Name: name}, handler)
}

// RegisterDescriptor adds one tool and returns the function that removes it.
// The revoke function is idempotent and frees the name for reuse.
//
// Registering a name that is already registered panics. Two contributors
// fighting over one model-facing name is never a valid state: silently
// overwriting turns which implementation the model reaches into a load-order
// lottery, and the loser's registration would never be revocable. Deliberate
// override goes through Replace.
func (r *Registry) RegisterDescriptor(descriptor Descriptor, handler Handler) func() {
	r.mu.Lock()
	if _, exists := r.handlers[descriptor.Name]; exists {
		r.mu.Unlock()
		panic(fmt.Sprintf("tool: duplicate registration for %q", descriptor.Name))
	}
	r.handlers[descriptor.Name] = handler
	r.describes[descriptor.Name] = descriptor
	r.mu.Unlock()
	return r.revokeFunc(descriptor.Name)
}

// Replace installs handler under descriptor.Name whether or not the name is
// taken, and returns the function that removes it. It does NOT restore the
// previous registration: reinstating a handler whose owner may already be gone
// would resurrect exactly the stale implementation this design removes.
func (r *Registry) Replace(descriptor Descriptor, handler Handler) func() {
	r.mu.Lock()
	r.handlers[descriptor.Name] = handler
	r.describes[descriptor.Name] = descriptor
	r.mu.Unlock()
	return r.revokeFunc(descriptor.Name)
}

// revokeFunc returns an idempotent remover for name.
func (r *Registry) revokeFunc(name string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.handlers, name)
			delete(r.describes, name)
			r.mu.Unlock()
		})
	}
}

// filter is one scope's view over what it inherits. allow == nil means no
// allow-list, so unlisted inherited tools stay visible; deny always removes.
type filter struct {
	allow map[string]bool
	deny  map[string]bool
}

// admits reports whether an inherited name survives this filter.
func (f *filter) admits(name string) bool {
	if f == nil {
		return true
	}
	if f.deny[name] {
		return false
	}
	if f.allow != nil && !f.allow[name] {
		return false
	}
	return true
}

// Subset returns a VIEW over this registry exposing only the named tools plus
// whatever the view registers itself. It shares this registry's policy,
// enforcer, guardrails, audit log and sanitizer. Names with no matching handler
// are ignored, and a tool registered on the parent later is visible only if it
// was named here. It backs delegated sub-agents that must run with a narrowed
// tool set.
//
// The view resolves through its parent on every call, so revoking a tool on the
// parent removes it from every derived view at once.
func (r *Registry) Subset(names ...string) *Registry {
	allow := make(map[string]bool, len(names))
	for _, name := range names {
		allow[name] = true
	}
	return r.view(&filter{allow: allow})
}

// Without returns a VIEW exposing every inherited tool except the named ones,
// including tools registered on the parent after this call. Names with no
// matching tool are ignored: disabling a tool an agent never had is a
// legitimate no-op, not an error. It never mutates the receiver.
func (r *Registry) Without(names ...string) *Registry {
	deny := make(map[string]bool, len(names))
	for _, name := range names {
		deny[name] = true
	}
	return r.view(&filter{deny: deny})
}

// view builds a child registry that inherits through f.
func (r *Registry) view(f *filter) *Registry {
	return &Registry{
		parent:    r,
		filter:    f,
		policy:    r.policy,
		enforcer:  r.enforcer,
		guards:    r.guards,
		audit:     r.audit,
		sanitizer: r.sanitizer,
		handlers:  make(map[string]Handler),
		describes: make(map[string]Descriptor),
	}
}

// resolve returns the handler and descriptor this registry currently exposes
// for name. Own registrations win and bypass the filter — a delegated scope
// keeps the tools it answers itself — while inherited ones must pass it.
func (r *Registry) resolve(name string) (Handler, Descriptor, bool) {
	r.mu.RLock()
	handler, ok := r.handlers[name]
	descriptor := r.describes[name]
	r.mu.RUnlock()
	if ok {
		return handler, descriptor, true
	}
	if r.parent == nil || !r.filter.admits(name) {
		return nil, Descriptor{}, false
	}
	return r.parent.resolve(name)
}

// visible collects every descriptor this registry exposes into out, inherited
// first so own registrations shadow same-named inherited ones.
func (r *Registry) visible(out map[string]Descriptor) {
	if r.parent != nil {
		inherited := make(map[string]Descriptor)
		r.parent.visible(inherited)
		for name, descriptor := range inherited {
			if r.filter.admits(name) {
				out[name] = descriptor
			}
		}
	}
	r.mu.RLock()
	for name, descriptor := range r.describes {
		out[name] = descriptor
	}
	r.mu.RUnlock()
}

func (r *Registry) Descriptors() []Descriptor {
	collected := make(map[string]Descriptor)
	r.visible(collected)
	descriptors := make([]Descriptor, 0, len(collected))
	for _, descriptor := range collected {
		descriptors = append(descriptors, descriptor)
	}
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].Name < descriptors[j].Name })
	return descriptors
}

// SafeToolNames 返回已注册工具中 NOT 敏感（且非 lazy 协议 meta 工具）的排序名。
// Plan 模式恰好提供这个集合，使规划运行无法触及有副作用工具。
func (r *Registry) SafeToolNames() []string {
	descriptors := r.Descriptors()
	names := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.Sensitive {
			continue
		}
		if descriptor.Name == "list_tools" || descriptor.Name == "call_tool" {
			continue
		}
		names = append(names, descriptor.Name)
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
	handler, descriptor, ok := r.resolve(call.Name)
	if !ok {
		return domain.ToolResult{}, fmt.Errorf("%w: %s", ErrToolNotFound, call.Name)
	}
	if call.RiskLevel == "" {
		call.RiskLevel = descriptor.RiskLevel
	}
	if err := validateInputSchema(descriptor.InputSchema, call.Arguments); err != nil {
		return domain.ToolResult{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if r.enforcer != nil {
		if err := r.enforcer.Check(agent, call); err != nil {
			return domain.ToolResult{}, err
		}
	}
	if r.policy != nil && r.policy.Decide(agent, call) == DecisionDeny {
		return domain.ToolResult{}, ErrPermissionDenied
	}
	// Deciders come AFTER the host's own enforcer and policy, and that order is
	// the enforcement: a call the host refused is never shown to a decider, so
	// no decider can widen one. They may only add refusals.
	if label, verdict := r.consultDeciders(ctx, descriptor, call); verdict.Decision != DecisionAllow {
		if err := r.resolveVerdict(ctx, label, verdict, call); err != nil {
			return domain.ToolResult{}, err
		}
	}
	if r.guards != nil {
		if err := r.guards.Before(ctx, call); err != nil {
			return domain.ToolResult{}, err
		}
	}
	// Hand the caller's identity down to the handler. Without it a tool cannot
	// scope its result to the requester, and tools that must (agent messaging)
	// would fall back to an unfiltered query.
	execCtx := WithCaller(ctx, agent)
	if descriptor.Timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, descriptor.Timeout)
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
	// Observers run last, on a result that is already final: nothing they do
	// can change what this function returns. See Observer for why the
	// exclusions above (permission/policy/guardrail refusals, handler errors)
	// are part of the contract rather than an oversight.
	r.notifyObservers(ctx, call, result)
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
		Origin:      CallOriginFrom(ctx),
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
