package compat

import (
	"encoding/json"
	"os"
	"testing"
)

type componentParityManifest struct {
	Components []componentParityEntry `json:"components"`
}

type componentParityEntry struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Status        string   `json:"status"`
	Packages      []string `json:"packages"`
	RequiredTests []string `json:"required_tests"`
	Degradation   string   `json:"degradation"`
}

func TestComponentParityManifest(t *testing.T) {
	data, err := os.ReadFile("testdata/component-parity.json")
	if err != nil {
		t.Fatalf("read component parity manifest: %v", err)
	}

	var manifest componentParityManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode component parity manifest: %v", err)
	}
	if len(manifest.Components) == 0 {
		t.Fatal("component parity manifest must contain at least one component")
	}

	allowedStatus := map[string]bool{
		"done":     true,
		"partial":  true,
		"planned":  true,
		"deferred": true,
	}
	requiredComponents := []string{
		"A00",
		"A01",
		"A03",
		"A20",
		"A21",
		"A22",
		"A23",
		"A30",
		"A31",
		"A32",
		"A52",
		"A53",
		"A54",
		"A60",
		"A61",
		"A63",
		"A64",
		"A70",
	}

	seen := make(map[string]componentParityEntry, len(manifest.Components))
	for _, component := range manifest.Components {
		if component.ID == "" {
			t.Error("component id is required")
		}
		if component.Name == "" {
			t.Errorf("%s: component name is required", component.ID)
		}
		if !allowedStatus[component.Status] {
			t.Errorf("%s: unsupported component status %q", component.ID, component.Status)
		}
		if len(component.Packages) == 0 {
			t.Errorf("%s: packages are required", component.ID)
		}
		if component.Status != "deferred" && len(component.RequiredTests) == 0 {
			t.Errorf("%s: non-deferred components must list required tests", component.ID)
		}
		if component.Status != "deferred" && component.Degradation == "" {
			t.Errorf("%s: non-deferred components must document degradation behavior", component.ID)
		}
		if _, ok := seen[component.ID]; ok {
			t.Errorf("%s: duplicate component entry", component.ID)
		}
		seen[component.ID] = component
	}

	for _, id := range requiredComponents {
		if _, ok := seen[id]; !ok {
			t.Errorf("required P10 component %s is missing from component parity manifest", id)
		}
	}
}
