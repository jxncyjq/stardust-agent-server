package tool

import "github.com/stardust/legion-agent/internal/domain"

type ExecutionPolicyConfig struct {
	AutoAllowTools []string
}

type ExecutionPolicy struct {
	autoAllow map[string]bool
}

func NewExecutionPolicy(cfg ExecutionPolicyConfig) ExecutionPolicy {
	autoAllow := make(map[string]bool, len(cfg.AutoAllowTools))
	for _, name := range cfg.AutoAllowTools {
		autoAllow[name] = true
	}
	return ExecutionPolicy{autoAllow: autoAllow}
}

func (p ExecutionPolicy) Decide(_ domain.Agent, call domain.ToolCall) Decision {
	if p.autoAllow[call.Name] {
		return DecisionAllow
	}
	switch call.RiskLevel {
	case "high", "critical":
		return DecisionDeny
	default:
		return DecisionAllow
	}
}
