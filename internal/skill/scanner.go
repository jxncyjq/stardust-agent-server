package skill

import (
	"context"
	"strings"
)

type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

const (
	RulePromptOverride     = "prompt_override"
	RuleSecretExfiltration = "secret_exfiltration"
	RuleSSRF               = "ssrf"
	RulePathTraversal      = "path_traversal"
	RuleUnsafeShell        = "unsafe_shell"
	RuleWildcardDelete     = "wildcard_delete"
	RulePolicyBypass       = "policy_bypass"
	RuleLicenseMissing     = "license_missing"
)

type SkillSecurityScanner interface {
	Scan(ctx context.Context, skill SkillPackage) (SkillScanReport, error)
}

type SkillPackage struct {
	Skill   Skill
	Content string
}

type SkillScanReport struct {
	SkillID   string
	RiskLevel RiskLevel
	Findings  []SkillScanFinding
}

type SkillScanFinding struct {
	SkillID  string
	RuleID   string
	Severity Severity
	Message  string
	Location string
}

type SecurityScanner struct{}

func NewSecurityScanner() SecurityScanner {
	return SecurityScanner{}
}

func (SecurityScanner) Scan(ctx context.Context, pkg SkillPackage) (SkillScanReport, error) {
	if err := ctx.Err(); err != nil {
		return SkillScanReport{}, err
	}
	content := strings.ToLower(pkg.Content)
	report := SkillScanReport{
		SkillID:   pkg.Skill.ID,
		RiskLevel: RiskSafe,
	}
	rules := []scanRule{
		{RulePromptOverride, SeverityCritical, []string{"ignore all previous instructions", "ignore previous instructions", "forget all instructions"}, "prompt override instruction"},
		{RuleSecretExfiltration, SeverityCritical, []string{"print secrets", "leak secret", "exfiltrate", "id_rsa", "api key"}, "secret exfiltration request"},
		{RuleSSRF, SeverityCritical, []string{"169.254.169.254", "metadata.google.internal", "localhost:", "127.0.0.1"}, "ssrf target"},
		{RulePathTraversal, SeverityCritical, []string{"../", "..\\", "/etc/passwd", ".ssh"}, "path traversal or sensitive path"},
		{RulePolicyBypass, SeverityCritical, []string{"disable safety", "disable security", "bypass policy", "ignore policy"}, "policy bypass request"},
		{RuleWildcardDelete, SeverityWarning, []string{"rm -rf *", "remove-item *", "del /s *"}, "wildcard delete command"},
		{RuleUnsafeShell, SeverityWarning, []string{"sudo ", "powershell -encodedcommand", "curl | sh", "bash -c"}, "unsafe shell command"},
	}
	for _, rule := range rules {
		if matchedAny(content, rule.patterns) {
			report.Findings = append(report.Findings, SkillScanFinding{
				SkillID:  pkg.Skill.ID,
				RuleID:   rule.id,
				Severity: rule.severity,
				Message:  rule.message,
				Location: "SKILL.md",
			})
		}
	}
	if !strings.Contains(content, "license:") && !strings.Contains(content, "license ") {
		report.Findings = append(report.Findings, SkillScanFinding{
			SkillID:  pkg.Skill.ID,
			RuleID:   RuleLicenseMissing,
			Severity: SeverityInfo,
			Message:  "license metadata missing",
			Location: "SKILL.md",
		})
	}
	report.RiskLevel = riskFromFindings(report.Findings)
	return report, nil
}

type scanRule struct {
	id       string
	severity Severity
	patterns []string
	message  string
}

func matchedAny(content string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.Contains(content, pattern) {
			return true
		}
	}
	return false
}

func riskFromFindings(findings []SkillScanFinding) RiskLevel {
	var warning bool
	for _, finding := range findings {
		switch finding.Severity {
		case SeverityCritical:
			return RiskCritical
		case SeverityWarning:
			warning = true
		}
	}
	if warning {
		return RiskWarning
	}
	return RiskSafe
}
