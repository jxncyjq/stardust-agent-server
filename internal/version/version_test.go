package version

import "testing"

func TestInfoReturnsBuildVariables(t *testing.T) {
	oldVersion := Version
	oldCommit := Commit
	oldBuildTime := BuildTime
	t.Cleanup(func() {
		Version = oldVersion
		Commit = oldCommit
		BuildTime = oldBuildTime
	})
	Version = "0.1.0-test"
	Commit = "abc123"
	BuildTime = "2026-05-15T12:00:00Z"

	info := Info()
	if info.Version != "0.1.0-test" || info.Commit != "abc123" || info.BuildTime != "2026-05-15T12:00:00Z" {
		t.Fatalf("Info() = %#v, want injected build variables", info)
	}
}
