package toolauth

import (
	"fmt"
	"sync"
	"testing"
)

func TestContributedToolBecomesGateable(t *testing.T) {
	if IsGateable("jira_search") {
		t.Fatal("precondition: jira_search must not be a builtin gateable tool")
	}
	revoke := Contribute(GateableTool{Name: "jira_search", Description: "按 JQL 检索 Jira issue"})
	defer revoke()

	if !IsGateable("jira_search") {
		t.Fatal("a contributed tool must be gateable, otherwise disabled_tools cannot reach it")
	}
	if !GateableToolNames()["jira_search"] {
		t.Fatal("contributed tool missing from GateableToolNames")
	}
	found := false
	for _, tool := range GateableTools() {
		if tool.Name == "jira_search" {
			found = true
			if tool.Description == "" {
				t.Fatal("contributed tool must keep its description for the config UI")
			}
		}
	}
	if !found {
		t.Fatal("contributed tool missing from GateableTools")
	}
}

func TestRevokeRemovesContributedTool(t *testing.T) {
	revoke := Contribute(GateableTool{Name: "jira_search", Description: "x"})
	revoke()

	if IsGateable("jira_search") {
		t.Fatal("a revoked contribution must not stay gateable")
	}
	revoke() // idempotent
	Contribute(GateableTool{Name: "jira_search", Description: "x"})()
}

func TestGateableToolsStaySortedAcrossContributions(t *testing.T) {
	defer Contribute(GateableTool{Name: "aaa_first", Description: "x"})()
	defer Contribute(GateableTool{Name: "zzz_last", Description: "x"})()

	tools := GateableTools()
	for i := 1; i < len(tools); i++ {
		if tools[i-1].Name > tools[i].Name {
			t.Fatalf("GateableTools must stay sorted, got %q before %q",
				tools[i-1].Name, tools[i].Name)
		}
	}
}

// Two contributors claiming one gateable name is never a valid state: the
// config UI would show one entry that silently governs a different tool.
func TestDuplicateContributionPanics(t *testing.T) {
	revoke := Contribute(GateableTool{Name: "jira_search", Description: "x"})
	defer revoke()

	defer func() {
		if recover() == nil {
			t.Fatal("want panic on duplicate contribution")
		}
	}()
	Contribute(GateableTool{Name: "jira_search", Description: "y"})
}

// Shadowing a builtin would let a contributor redefine what an existing
// disabled_tools entry governs.
func TestContributionShadowingBuiltinPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic when a contribution shadows a builtin gateable tool")
		}
	}()
	Contribute(GateableTool{Name: "read_file", Description: "impostor"})
}

func TestEmptyContributionNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic on empty contribution name")
		}
	}()
	Contribute(GateableTool{Description: "nameless"})
}

func TestConcurrentContributeAndRead(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			Contribute(GateableTool{Name: fmt.Sprintf("churn_%d", i), Description: "x"})()
		}(i)
		go func() {
			defer wg.Done()
			_ = GateableTools()
			_ = IsGateable("read_file")
		}()
	}
	wg.Wait()
}
