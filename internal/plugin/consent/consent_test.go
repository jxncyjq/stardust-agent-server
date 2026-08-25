package consent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/plugin/manifest"
)

// testActor is the sentinel actor value every test below passes, and every
// error-path assertion below requires the error to contain: it is the
// caller-supplied identity ("plugins grant", "POST /v1/plugins/{name}/grant")
// this package's whole reason for existing is to keep in the error message,
// so an endpoint can report its own name instead of a hardcoded CLI phrase.
const testActor = "test-actor 42"

// requireErrorContainsActor asserts err is non-nil, names testActor, and
// names every other want string -- failing loudly (with the actual error
// text) when any is missing.
func requireErrorContainsActor(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error containing %q and %v, got nil", testActor, want)
	}
	if !strings.Contains(err.Error(), testActor) {
		t.Fatalf("error %q does not contain actor %q", err.Error(), testActor)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("error %q does not contain %q", err.Error(), w)
		}
	}
}

func pluginManifest(capabilities []string, allowedHosts, allowedPaths []string) manifest.PluginManifest {
	return manifest.PluginManifest{
		Name:         "consent-test-plugin",
		Capabilities: capabilities,
		Network:      manifest.Network{AllowedHosts: allowedHosts},
		Filesystem:   manifest.Filesystem{AllowedPaths: allowedPaths},
	}
}

// --- ResolveCapabilities -------------------------------------------------

func TestResolveCapabilitiesRefusesAStrictSubsetAndNamesTheMissingOne(t *testing.T) {
	pm := pluginManifest([]string{"log", "http"}, nil, nil)

	_, err := ResolveCapabilities(testActor, []string{"log"}, pm)

	requireErrorContainsActor(t, err, "http", pm.Name)
}

func TestResolveCapabilitiesRefusesAnUndeclaredCapabilityAndNamesIt(t *testing.T) {
	pm := pluginManifest([]string{"log"}, nil, nil)

	_, err := ResolveCapabilities(testActor, []string{"log", "http"}, pm)

	requireErrorContainsActor(t, err, "http", pm.Name)
}

func TestResolveCapabilitiesAcceptsTheExactDeclaredSet(t *testing.T) {
	pm := pluginManifest([]string{"log", "http"}, nil, nil)

	got, err := ResolveCapabilities(testActor, []string{"log", "http"}, pm)
	if err != nil {
		t.Fatalf("ResolveCapabilities: unexpected error: %v", err)
	}
	if want := []string{"log", "http"}; !slices.Equal(got, want) {
		t.Errorf("ResolveCapabilities = %v, want %v", got, want)
	}
}

func TestResolveCapabilitiesAcceptsAnEmptyGrantForAPluginDeclaringNone(t *testing.T) {
	pm := pluginManifest(nil, nil, nil)

	got, err := ResolveCapabilities(testActor, nil, pm)
	if err != nil {
		t.Fatalf("ResolveCapabilities: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ResolveCapabilities = %v, want empty", got)
	}
}

// --- ResolveAllowedHosts --------------------------------------------------

func TestResolveAllowedHostsAcceptsAStrictSubsetOfWhatIsDeclared(t *testing.T) {
	pm := pluginManifest([]string{"http"}, []string{"api.example.com", "cdn.example.com"}, nil)

	got, err := ResolveAllowedHosts(testActor, []string{"api.example.com"}, pm)
	if err != nil {
		t.Fatalf("ResolveAllowedHosts: unexpected error: %v", err)
	}
	if want := []string{"api.example.com"}; !slices.Equal(got, want) {
		t.Errorf("ResolveAllowedHosts = %v, want %v", got, want)
	}
}

func TestResolveAllowedHostsRefusesAnUndeclaredHostAndNamesIt(t *testing.T) {
	pm := pluginManifest([]string{"http"}, []string{"api.example.com"}, nil)

	_, err := ResolveAllowedHosts(testActor, []string{"evil.example.com"}, pm)

	requireErrorContainsActor(t, err, "evil.example.com", pm.Name)
}

func TestResolveAllowedHostsComparisonIsCaseInsensitive(t *testing.T) {
	pm := pluginManifest([]string{"http"}, []string{"API.Example.COM"}, nil)

	got, err := ResolveAllowedHosts(testActor, []string{"api.example.com"}, pm)
	if err != nil {
		t.Fatalf("ResolveAllowedHosts: unexpected error: %v", err)
	}
	if want := []string{"api.example.com"}; !slices.Equal(got, want) {
		t.Errorf("ResolveAllowedHosts = %v, want %v", got, want)
	}
}

func TestResolveAllowedHostsAcceptsEmptyWhenNoneAreDeclared(t *testing.T) {
	pm := pluginManifest([]string{"http"}, nil, nil)

	got, err := ResolveAllowedHosts(testActor, nil, pm)
	if err != nil {
		t.Fatalf("ResolveAllowedHosts: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ResolveAllowedHosts = %v, want empty", got)
	}
}

// --- ResolveAllowedPaths ---------------------------------------------------

func TestResolveAllowedPathsAcceptsAStrictSubsetOfWhatIsDeclared(t *testing.T) {
	pm := pluginManifest([]string{"fs"}, nil, []string{"/data", "/cache"})

	got, err := ResolveAllowedPaths(testActor, []string{"/data"}, pm)
	if err != nil {
		t.Fatalf("ResolveAllowedPaths: unexpected error: %v", err)
	}
	if want := []string{"/data"}; !slices.Equal(got, want) {
		t.Errorf("ResolveAllowedPaths = %v, want %v", got, want)
	}
}

func TestResolveAllowedPathsRefusesAnUndeclaredPathAndNamesIt(t *testing.T) {
	pm := pluginManifest([]string{"fs"}, nil, []string{"/data"})

	_, err := ResolveAllowedPaths(testActor, []string{"/etc"}, pm)

	requireErrorContainsActor(t, err, "/etc", pm.Name)
}

// --- RefuseUnnamedAllowlist -------------------------------------------------

func TestRefuseUnnamedAllowlistRefusesHTTPWithNoHostsNamedWhenSomeAreDeclared(t *testing.T) {
	pm := pluginManifest([]string{"http"}, []string{"api.example.com"}, nil)

	err := RefuseUnnamedAllowlist(testActor, "--capabilities", []string{"http"}, pm, nil, nil,
		"name a host", "name a path")

	requireErrorContainsActor(t, err, "--capabilities", "http", pm.Name)
}

func TestRefuseUnnamedAllowlistAcceptsHTTPWhenTheDeclaredAllowlistIsEmpty(t *testing.T) {
	pm := pluginManifest([]string{"http"}, nil, nil)

	err := RefuseUnnamedAllowlist(testActor, "--capabilities", []string{"http"}, pm, nil, nil,
		"name a host", "name a path")
	if err != nil {
		t.Fatalf("RefuseUnnamedAllowlist: unexpected error: %v", err)
	}
}

func TestRefuseUnnamedAllowlistAcceptsHTTPWhenAHostIsNamed(t *testing.T) {
	pm := pluginManifest([]string{"http"}, []string{"api.example.com"}, nil)

	err := RefuseUnnamedAllowlist(testActor, "--capabilities", []string{"http"}, pm,
		[]string{"api.example.com"}, nil, "name a host", "name a path")
	if err != nil {
		t.Fatalf("RefuseUnnamedAllowlist: unexpected error: %v", err)
	}
}

func TestRefuseUnnamedAllowlistRefusesFSWithNoPathsNamedWhenSomeAreDeclared(t *testing.T) {
	pm := pluginManifest([]string{"fs"}, nil, []string{"/data"})

	err := RefuseUnnamedAllowlist(testActor, "--capabilities", []string{"fs"}, pm, nil, nil,
		"name a host", "name a path")

	requireErrorContainsActor(t, err, "--capabilities", "fs", pm.Name)
}

// --- FindEntry --------------------------------------------------------------

func TestFindEntryReturnsTheMatchingEntry(t *testing.T) {
	dep := manifest.Deployment{Plugins: []manifest.Entry{
		{Name: "alpha", Source: "./alpha"},
		{Name: "beta", Source: "./beta"},
	}}

	got, err := FindEntry(dep, "beta")
	if err != nil {
		t.Fatalf("FindEntry: unexpected error: %v", err)
	}
	if got.Name != "beta" {
		t.Errorf("FindEntry.Name = %q, want %q", got.Name, "beta")
	}
}

func TestFindEntryRefusesAnUnknownNameAndListsWhatExists(t *testing.T) {
	dep := manifest.Deployment{Plugins: []manifest.Entry{
		{Name: "alpha", Source: "./alpha"},
	}}

	_, err := FindEntry(dep, "gamma")
	if err == nil {
		t.Fatal("FindEntry: want an error for an unknown name, got nil")
	}
	if !strings.Contains(err.Error(), "gamma") {
		t.Errorf("FindEntry error = %v, want it to name the requested %q", err, "gamma")
	}
	if !strings.Contains(err.Error(), "alpha") {
		t.Errorf("FindEntry error = %v, want it to name the existing entry %q", err, "alpha")
	}
}

// --- ReadDeploymentWithSnapshot / RefuseDeploymentChanged -------------------

func TestReadDeploymentWithSnapshotReturnsTheParsedDeploymentAndRawBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.json")
	content := []byte(`{"plugins":[{"name":"alpha","source":"./alpha"}]}`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write plugins.json: %v", err)
	}

	dep, snapshot, err := ReadDeploymentWithSnapshot(path)
	if err != nil {
		t.Fatalf("ReadDeploymentWithSnapshot: unexpected error: %v", err)
	}
	if len(dep.Plugins) != 1 || dep.Plugins[0].Name != "alpha" {
		t.Errorf("ReadDeploymentWithSnapshot deployment = %+v, want one entry named alpha", dep)
	}
	if string(snapshot) != string(content) {
		t.Errorf("ReadDeploymentWithSnapshot snapshot = %q, want %q", snapshot, content)
	}
}

func TestReadDeploymentWithSnapshotFailsLoudlyWhenThePathDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.json")

	_, _, err := ReadDeploymentWithSnapshot(path)
	if err == nil {
		t.Fatal("ReadDeploymentWithSnapshot: want an error for a missing file, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("ReadDeploymentWithSnapshot error = %v, want it to name the path %q", err, path)
	}
}

func TestRefuseDeploymentChangedErrorsWhenTheBytesOnDiskChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.json")
	original := []byte(`{"plugins":[]}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("write plugins.json: %v", err)
	}
	snapshot := append([]byte(nil), original...)

	changed := []byte(`{"plugins":[{"name":"alpha","source":"./alpha"}]}`)
	if err := os.WriteFile(path, changed, 0o644); err != nil {
		t.Fatalf("rewrite plugins.json: %v", err)
	}

	err := RefuseDeploymentChanged(testActor, path, snapshot)

	requireErrorContainsActor(t, err, path)
}

func TestRefuseDeploymentChangedAcceptsAnUnchangedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugins.json")
	original := []byte(`{"plugins":[]}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("write plugins.json: %v", err)
	}
	snapshot := append([]byte(nil), original...)

	if err := RefuseDeploymentChanged(testActor, path, snapshot); err != nil {
		t.Fatalf("RefuseDeploymentChanged: unexpected error: %v", err)
	}
}
