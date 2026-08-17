package host

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stardust/legion-agent/internal/adapter"
	"github.com/stardust/legion-agent/internal/domain"
	"github.com/stardust/legion-agent/internal/plugin/abi"
	"github.com/stardust/legion-agent/internal/plugin/perm"
	"github.com/stardust/legion-agent/internal/port"
	"github.com/stardust/legion-agent/internal/tool"
)

// Ops 70-79 are test-only fixture ops implemented by
// testdata/guest-hostcall-rust/src/lib.rs; see testdata/README.md for the full
// op table. Each one calls exactly one host function and returns its packed
// result verbatim.
const (
	opCallLog         int32 = 70 // call log(1, body)
	opCallConfigGet   int32 = 71 // call config_get()
	opCallKVGet       int32 = 72 // call kv_get(body)
	opCallKVPut       int32 = 73 // call kv_put(key, value) from a "<key>\n<value>" body
	opCallHTTPRequest int32 = 74 // call http_request(body)
	opCallReadFile    int32 = 75 // call read_file(body)
	opCallCallTool    int32 = 76 // call call_tool(body)
	opArmAllocFailure int32 = 77 // make the next plugin_alloc return 0
)

// hostcallWasm loads the compiled host-call test guest once per test binary
// run. Unlike testdata/plugin.wasm it imports every legion host function, so
// it only instantiates against a fully granted host module.
var hostcallWasm = sync.OnceValues(func() ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", "hostcall.wasm"))
})

// testPluginName is the plugin identity every fixture host module is built
// with: it shows up in the kv namespace and in the call_tool origin.
const testPluginName = "fixture-plugin"

// memKV is an in-memory KVStore. It records the keys it is handed verbatim so
// tests can prove the host namespaced them.
type memKV struct {
	mu   sync.Mutex
	data map[string]string
	err  error
}

func newMemKV() *memKV { return &memKV{data: map[string]string{}} }

func (s *memKV) Get(ctx context.Context, key string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", false, s.err
	}
	v, ok := s.data[key]
	return v, ok, nil
}

func (s *memKV) Put(ctx context.Context, key, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.data[key] = value
	return nil
}

func (s *memKV) snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}

// fullGrant authorizes every capability. Allowlists stay empty: a test that
// needs http or fs to succeed must say which host or path it allows, so no
// test passes because an allowlist was permissive by default.
func fullGrant() perm.Grant {
	return perm.Grant{Log: true, Config: true, KV: true, HTTP: true, FS: true, Tool: true}
}

// testEnv is the set of fakes behind one fixture host module, kept so a test
// can assert on what the host functions did (logged, published, stored,
// executed).
type testEnv struct {
	deps    Deps
	logs    *bytes.Buffer
	events  *adapter.MemoryEventBus
	kv      *memKV
	root    string
	origins []string
	mu      sync.Mutex
}

// newTestEnv wires every dependency BuildHostModule can ask for: a capturing
// logger, an in-memory event bus, an in-memory kv store, a real http client, a
// workspace path guard rooted at a fresh temp dir, and a tool registry holding
// one echo tool that records the call origin it was reached with.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp workspace root: %v", err)
	}
	env := &testEnv{
		logs:   &bytes.Buffer{},
		events: adapter.NewMemoryEventBus(),
		kv:     newMemKV(),
		root:   root,
	}
	registry := tool.NewRegistry(nil, nil, nil)
	registry.Register("echo_tool", tool.HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		env.mu.Lock()
		env.origins = append(env.origins, tool.CallOriginFrom(ctx))
		env.mu.Unlock()
		return domain.ToolResult{CallID: call.ID, Success: true, Output: "echo:" + call.Arguments["text"]}, nil
	}))
	env.deps = Deps{
		PluginName: testPluginName,
		Logger:     slog.New(slog.NewTextHandler(env.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Config:     json.RawMessage(`{"endpoint":"https://api.example.com","retries":3}`),
		KV:         env.kv,
		HTTP:       http.DefaultClient,
		FS:         port.NewWorkspacePathGuard(root),
		Events:     env.events,
		Tools:      registry,
		Agent:      domain.Agent{ID: "agent-1", CompanyID: "co-1", Role: "developer", Status: domain.AgentActive},
	}
	return env
}

func (e *testEnv) recordedOrigins() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.origins...)
}

// newHostcallInstance builds a host module from g and deps, then compiles and
// instantiates the host-call fixture guest against it.
func newHostcallInstance(t *testing.T, g perm.Grant, deps Deps) *Instance {
	t.Helper()

	ctx := context.Background()
	rt := NewRuntime(ctx, testMemoryPages)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	if _, err := BuildHostModule(ctx, rt, g, deps); err != nil {
		t.Fatalf("BuildHostModule: %v", err)
	}
	wasmBytes, err := hostcallWasm()
	if err != nil {
		t.Fatalf("read hostcall fixture wasm: %v", err)
	}
	compiled, err := Compile(ctx, rt, wasmBytes)
	if err != nil {
		t.Fatalf("Compile(hostcall fixture): %v", err)
	}
	inst, err := NewInstance(ctx, rt, compiled)
	if err != nil {
		t.Fatalf("NewInstance(hostcall fixture): %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst
}

// hostError is the JSON error envelope every host function returns instead of
// a result body when it refuses or fails.
type hostError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// decodeHostError decodes out as an error envelope, failing the test when it
// carries no code — a body with no code is a success body, and reading one as
// a denial is how a "denied" assertion passes vacuously.
func decodeHostError(t *testing.T, out []byte) hostError {
	t.Helper()
	var got hostError
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode host error envelope from %s: %v", out, err)
	}
	if got.Code == "" {
		t.Fatalf("body %s carries no error code, want an error envelope", out)
	}
	return got
}

// deniedEvents returns the plugin/call_failed events whose category is
// "denied".
func deniedEvents(t *testing.T, env *testEnv) []domain.RuntimeEvent {
	t.Helper()
	all, err := env.events.Events()
	if err != nil {
		t.Fatalf("read published events: %v", err)
	}
	var denied []domain.RuntimeEvent
	for _, ev := range all {
		if ev.Type == RuntimeEventCallFailed && strings.Contains(ev.Message, "category=denied") {
			denied = append(denied, ev)
		}
	}
	return denied
}

// TestBuildHostModuleRegistersOnlyGrantedCapabilities is the core invariant:
// an ungranted capability is absent from the module, not present-and-refusing.
func TestBuildHostModuleRegistersOnlyGrantedCapabilities(t *testing.T) {
	env := newTestEnv(t)

	cases := []struct {
		name  string
		grant perm.Grant
		want  []string
	}{
		{name: "log only", grant: perm.Grant{Log: true}, want: []string{"log"}},
		{name: "config only", grant: perm.Grant{Config: true}, want: []string{"config_get"}},
		{name: "kv only", grant: perm.Grant{KV: true}, want: []string{"kv_get", "kv_put"}},
		{name: "http only", grant: perm.Grant{HTTP: true, AllowedHosts: []string{"example.com"}}, want: []string{"http_request"}},
		{name: "fs only", grant: perm.Grant{FS: true, AllowedPaths: []string{"/tmp"}}, want: []string{"read_file"}},
		{name: "tool only", grant: perm.Grant{Tool: true}, want: []string{"call_tool"}},
		{name: "nothing", grant: perm.Grant{}, want: nil},
		{
			name:  "everything",
			grant: fullGrant(),
			want:  []string{"call_tool", "config_get", "http_request", "kv_get", "kv_put", "log", "read_file"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rt := NewRuntime(ctx, testMemoryPages)
			t.Cleanup(func() { _ = rt.Close(context.Background()) })

			mod, err := BuildHostModule(ctx, rt, tc.grant, env.deps)
			if err != nil {
				t.Fatalf("BuildHostModule: %v", err)
			}
			got := make([]string, 0, len(mod.ExportedFunctionDefinitions()))
			for name := range mod.ExportedFunctionDefinitions() {
				got = append(got, name)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("host module exports %v, want exactly %v", got, tc.want)
			}
		})
	}
}

// TestUngrantedCapabilityIsAbsentAtLinkTime is the mutation target: with only
// log granted, a guest importing http_request must fail to INSTANTIATE. If the
// implementation ever registers ungranted functions and denies at run time,
// instantiation succeeds and this test fails.
func TestUngrantedCapabilityIsAbsentAtLinkTime(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	wasmBytes, err := hostcallWasm()
	if err != nil {
		t.Fatalf("read hostcall fixture wasm: %v", err)
	}

	rt := NewRuntime(ctx, testMemoryPages)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	if _, err := BuildHostModule(ctx, rt, perm.Grant{Log: true}, env.deps); err != nil {
		t.Fatalf("BuildHostModule(log only): %v", err)
	}
	compiled, err := Compile(ctx, rt, wasmBytes)
	if err != nil {
		t.Fatalf("Compile(hostcall fixture): %v", err)
	}
	if _, err := NewInstance(ctx, rt, compiled); err == nil {
		t.Fatal("NewInstance succeeded against a log-only host module; the ungranted " +
			"capabilities must be absent from the module (link-time failure), not " +
			"registered functions that refuse at run time")
	}

	// The same fixture against a full grant must instantiate, so the failure
	// above is the missing grant and not a broken fixture.
	full := NewRuntime(ctx, testMemoryPages)
	t.Cleanup(func() { _ = full.Close(context.Background()) })
	if _, err := BuildHostModule(ctx, full, fullGrant(), env.deps); err != nil {
		t.Fatalf("BuildHostModule(full grant): %v", err)
	}
	fullCompiled, err := Compile(ctx, full, wasmBytes)
	if err != nil {
		t.Fatalf("Compile(hostcall fixture, full runtime): %v", err)
	}
	inst, err := NewInstance(ctx, full, fullCompiled)
	if err != nil {
		t.Fatalf("NewInstance against a fully granted host module: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
}

// TestCheckImportsNamesTheMissingFunctions covers the human-readable
// pre-check: the host compares the module's imports against the grant and says
// which function is missing and which capability would provide it, instead of
// leaving the user with wazero's raw link error.
func TestCheckImportsNamesTheMissingFunctions(t *testing.T) {
	ctx := context.Background()
	rt := NewRuntime(ctx, testMemoryPages)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	wasmBytes, err := hostcallWasm()
	if err != nil {
		t.Fatalf("read hostcall fixture wasm: %v", err)
	}
	compiled, err := Compile(ctx, rt, wasmBytes)
	if err != nil {
		t.Fatalf("Compile(hostcall fixture): %v", err)
	}

	err = CheckImports(compiled, perm.Grant{Log: true})
	if err == nil {
		t.Fatal("CheckImports(log only) = nil, want an error naming the ungranted imports")
	}
	msg := err.Error()
	for _, want := range []string{"http_request", "read_file", "call_tool", "config_get", "kv_get", "kv_put"} {
		if !strings.Contains(msg, want) {
			t.Errorf("CheckImports error %q does not name the missing function %q", msg, want)
		}
	}
	// The capability names are what a deployment actually has to grant, so the
	// message must carry them too.
	for _, want := range []string{"http", "fs", "tool"} {
		if !strings.Contains(msg, want) {
			t.Errorf("CheckImports error %q does not name the capability %q that would grant it", msg, want)
		}
	}
	// log IS granted here, so it must not be reported among the missing ones.
	if strings.Contains(msg, `"log"`) {
		t.Errorf("CheckImports error %q reports the granted function log as missing", msg)
	}

	if err := CheckImports(compiled, fullGrant()); err != nil {
		t.Errorf("CheckImports(full grant) = %v, want nil", err)
	}
}

// TestCheckImportNamesRejectsUnknownHostImport covers the import the host has
// no idea about: it must be reported as unresolvable rather than silently
// ignored (and then failing later as a raw wazero link error).
func TestCheckImportNamesRejectsUnknownHostImport(t *testing.T) {
	err := checkImportNames([]string{"log", "definitely_not_a_host_function"}, perm.Grant{Log: true})
	if err == nil {
		t.Fatal("checkImportNames(unknown import) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "definitely_not_a_host_function") {
		t.Errorf("error %q does not name the unknown import", err)
	}
}

// TestBuildHostModuleFailsWhenGrantedCapabilityLacksDependency covers the
// assembly-time invariant: registering a granted capability whose dependency is
// missing would produce a host function that cannot do its job, so the build
// must fail loudly instead.
func TestBuildHostModuleFailsWhenGrantedCapabilityLacksDependency(t *testing.T) {
	cases := []struct {
		name     string
		grant    perm.Grant
		mutate   func(*Deps)
		wantWord string
	}{
		{
			name:     "log without a logger",
			grant:    perm.Grant{Log: true},
			mutate:   func(d *Deps) { d.Logger = nil },
			wantWord: "Logger",
		},
		{
			name:     "config without config json",
			grant:    perm.Grant{Config: true},
			mutate:   func(d *Deps) { d.Config = nil },
			wantWord: "Config",
		},
		{
			name:     "config with invalid json",
			grant:    perm.Grant{Config: true},
			mutate:   func(d *Deps) { d.Config = json.RawMessage(`{"broken":`) },
			wantWord: "Config",
		},
		{
			name:     "kv without a store",
			grant:    perm.Grant{KV: true},
			mutate:   func(d *Deps) { d.KV = nil },
			wantWord: "KV",
		},
		{
			name:     "http without a client",
			grant:    perm.Grant{HTTP: true, AllowedHosts: []string{"example.com"}},
			mutate:   func(d *Deps) { d.HTTP = nil },
			wantWord: "HTTP",
		},
		{
			name:     "http without an event bus",
			grant:    perm.Grant{HTTP: true, AllowedHosts: []string{"example.com"}},
			mutate:   func(d *Deps) { d.Events = nil },
			wantWord: "Events",
		},
		{
			name:     "fs without a path guard",
			grant:    perm.Grant{FS: true, AllowedPaths: []string{"/tmp"}},
			mutate:   func(d *Deps) { d.FS = port.WorkspacePathGuard{} },
			wantWord: "FS",
		},
		{
			name:     "fs without an event bus",
			grant:    perm.Grant{FS: true, AllowedPaths: []string{"/tmp"}},
			mutate:   func(d *Deps) { d.Events = nil },
			wantWord: "Events",
		},
		{
			name:     "tool without a registry",
			grant:    perm.Grant{Tool: true},
			mutate:   func(d *Deps) { d.Tools = nil },
			wantWord: "Tools",
		},
		{
			name:     "tool without an agent identity",
			grant:    perm.Grant{Tool: true},
			mutate:   func(d *Deps) { d.Agent = domain.Agent{} },
			wantWord: "Agent",
		},
		{
			name:     "any capability without a plugin name",
			grant:    perm.Grant{Log: true},
			mutate:   func(d *Deps) { d.PluginName = "" },
			wantWord: "PluginName",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			deps := env.deps
			tc.mutate(&deps)

			ctx := context.Background()
			rt := NewRuntime(ctx, testMemoryPages)
			t.Cleanup(func() { _ = rt.Close(context.Background()) })

			mod, err := BuildHostModule(ctx, rt, tc.grant, deps)
			if err == nil {
				t.Fatalf("BuildHostModule returned a module (%v) with a missing dependency, want an error", mod != nil)
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("error %q does not name the missing dependency %q", err, tc.wantWord)
			}
		})
	}
}

// TestBuildHostModuleIgnoresDependenciesOfUngrantedCapabilities is the other
// half of the invariant above: a nil dependency for a capability nobody
// granted is not a problem, because nothing will be registered for it.
func TestBuildHostModuleIgnoresDependenciesOfUngrantedCapabilities(t *testing.T) {
	ctx := context.Background()
	rt := NewRuntime(ctx, testMemoryPages)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	logs := &bytes.Buffer{}
	deps := Deps{
		PluginName: testPluginName,
		Logger:     slog.New(slog.NewTextHandler(logs, nil)),
	}
	if _, err := BuildHostModule(ctx, rt, perm.Grant{Log: true}, deps); err != nil {
		t.Fatalf("BuildHostModule(log only, only a logger provided) = %v, want nil", err)
	}
}

func TestLogWritesToTheHostLogger(t *testing.T) {
	env := newTestEnv(t)
	inst := newHostcallInstance(t, fullGrant(), env.deps)

	out, err := inst.Invoke(context.Background(), opCallLog, []byte("plugin says hello"))
	if err != nil {
		t.Fatalf("Invoke(log): %v", err)
	}
	if len(out) != 0 {
		t.Errorf("Invoke(log) returned body %s, want none (log has no return value)", out)
	}
	if got := env.logs.String(); !strings.Contains(got, "plugin says hello") || !strings.Contains(got, testPluginName) {
		t.Errorf("host log = %q, want it to carry the guest message and the plugin name", got)
	}
}

func TestConfigGetReturnsTheConfigVerbatim(t *testing.T) {
	env := newTestEnv(t)
	inst := newHostcallInstance(t, fullGrant(), env.deps)

	out, err := inst.Invoke(context.Background(), opCallConfigGet, nil)
	if err != nil {
		t.Fatalf("Invoke(config_get): %v", err)
	}
	if string(out) != string(env.deps.Config) {
		t.Errorf("config_get returned %s, want the deployment config verbatim %s", out, env.deps.Config)
	}
}

func TestKVRoundTripsInThePluginNamespace(t *testing.T) {
	env := newTestEnv(t)
	inst := newHostcallInstance(t, fullGrant(), env.deps)
	ctx := context.Background()

	putOut, err := inst.Invoke(ctx, opCallKVPut, []byte("cursor\nrow-42"))
	if err != nil {
		t.Fatalf("Invoke(kv_put): %v", err)
	}
	var put struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(putOut, &put); err != nil {
		t.Fatalf("decode kv_put response %s: %v", putOut, err)
	}
	if !put.OK {
		t.Errorf("kv_put response = %s, want ok=true", putOut)
	}

	// The guest asked for "cursor"; the host must have stored it under the
	// plugin's own namespace so two plugins cannot read each other's keys.
	stored := env.kv.snapshot()
	if _, unqualified := stored["cursor"]; unqualified {
		t.Errorf("kv store holds the unqualified key %q; keys must be namespaced per plugin: %v", "cursor", stored)
	}
	found := false
	for key := range stored {
		if strings.Contains(key, testPluginName) && strings.Contains(key, "cursor") {
			found = true
		}
	}
	if !found {
		t.Errorf("kv store = %v, want a key namespaced with the plugin name %q", stored, testPluginName)
	}

	getOut, err := inst.Invoke(ctx, opCallKVGet, []byte("cursor"))
	if err != nil {
		t.Fatalf("Invoke(kv_get): %v", err)
	}
	var got struct {
		Found bool   `json:"found"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(getOut, &got); err != nil {
		t.Fatalf("decode kv_get response %s: %v", getOut, err)
	}
	if !got.Found || got.Value != "row-42" {
		t.Errorf("kv_get response = %s, want found=true value=row-42", getOut)
	}

	missOut, err := inst.Invoke(ctx, opCallKVGet, []byte("never-written"))
	if err != nil {
		t.Fatalf("Invoke(kv_get miss): %v", err)
	}
	if err := json.Unmarshal(missOut, &got); err != nil {
		t.Fatalf("decode kv_get miss response %s: %v", missOut, err)
	}
	if got.Found {
		t.Errorf("kv_get(never-written) = %s, want found=false", missOut)
	}
}

func TestHTTPRequestReachesAnAllowedHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-Method", r.Method)
		_, _ = w.Write([]byte("upstream body"))
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}

	env := newTestEnv(t)
	grant := fullGrant()
	grant.AllowedHosts = []string{parsed.Hostname()}
	inst := newHostcallInstance(t, grant, env.deps)

	req, err := json.Marshal(map[string]any{"method": "GET", "url": server.URL})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := inst.Invoke(context.Background(), opCallHTTPRequest, req)
	if err != nil {
		t.Fatalf("Invoke(http_request): %v", err)
	}
	var resp struct {
		Status  int                 `json:"status"`
		Headers map[string][]string `json:"headers"`
		Body    string              `json:"body"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode http_request response %s: %v", out, err)
	}
	if resp.Status != http.StatusOK || resp.Body != "upstream body" {
		t.Errorf("http_request response = %s, want status 200 and the upstream body", out)
	}
	if len(deniedEvents(t, env)) != 0 {
		t.Errorf("an allowed request published a denial event: %v", deniedEvents(t, env))
	}
}

// TestHTTPRequestDeniesAHostOutsideTheAllowlist is the second check: the HTTP
// capability being granted says nothing about which hosts are reachable.
func TestHTTPRequestDeniesAHostOutsideTheAllowlist(t *testing.T) {
	var reached bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer server.Close()

	env := newTestEnv(t)
	grant := fullGrant()
	grant.AllowedHosts = []string{"allowed.example"}
	inst := newHostcallInstance(t, grant, env.deps)

	req, err := json.Marshal(map[string]any{"method": "GET", "url": server.URL})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := inst.Invoke(context.Background(), opCallHTTPRequest, req)
	if err != nil {
		t.Fatalf("Invoke(http_request): %v", err)
	}
	got := decodeHostError(t, out)
	if got.Code != CodeDenied {
		t.Errorf("http_request to an unlisted host returned code %q, want %q (body %s)", got.Code, CodeDenied, out)
	}
	if reached {
		t.Error("the denied request still reached the upstream server")
	}
	denied := deniedEvents(t, env)
	if len(denied) != 1 {
		t.Fatalf("published %d plugin/call_failed{denied} events, want exactly 1 (all events: %v)", len(denied), denied)
	}
	if !strings.Contains(denied[0].Message, "http_request") || !strings.Contains(denied[0].Message, testPluginName) {
		t.Errorf("denial event message %q must name the host function and the plugin", denied[0].Message)
	}
}

func TestHTTPRequestDeniesANonHTTPScheme(t *testing.T) {
	env := newTestEnv(t)
	grant := fullGrant()
	grant.AllowedHosts = []string{"allowed.example"}
	inst := newHostcallInstance(t, grant, env.deps)

	req, err := json.Marshal(map[string]any{"method": "GET", "url": "file:///etc/passwd"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := inst.Invoke(context.Background(), opCallHTTPRequest, req)
	if err != nil {
		t.Fatalf("Invoke(http_request): %v", err)
	}
	if got := decodeHostError(t, out); got.Code != CodeDenied {
		t.Errorf("http_request with a file:// URL returned code %q, want %q (body %s)", got.Code, CodeDenied, out)
	}
	if len(deniedEvents(t, env)) != 1 {
		t.Errorf("published %d denial events, want 1", len(deniedEvents(t, env)))
	}
}

func TestReadFileReadsAnAllowedPath(t *testing.T) {
	env := newTestEnv(t)
	allowedDir := filepath.Join(env.root, "allowed")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("create allowed dir: %v", err)
	}
	target := filepath.Join(allowedDir, "ok.txt")
	if err := os.WriteFile(target, []byte("plugin readable"), 0o644); err != nil {
		t.Fatalf("write allowed file: %v", err)
	}

	grant := fullGrant()
	grant.AllowedPaths = []string{allowedDir}
	inst := newHostcallInstance(t, grant, env.deps)

	req, err := json.Marshal(map[string]string{"path": target})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := inst.Invoke(context.Background(), opCallReadFile, req)
	if err != nil {
		t.Fatalf("Invoke(read_file): %v", err)
	}
	var resp struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode read_file response %s: %v", out, err)
	}
	if resp.Content != "plugin readable" {
		t.Errorf("read_file response = %s, want the file content", out)
	}
	if len(deniedEvents(t, env)) != 0 {
		t.Errorf("an allowed read published a denial event: %v", deniedEvents(t, env))
	}
}

// TestReadFileDeniesPathsOutsideTheAllowlist covers both fs checks: the
// workspace guard (a path escaping the workspace root, a symlink pointing out
// of it) and the allowlist (a path inside the workspace but outside
// allowed_paths).
func TestReadFileDeniesPathsOutsideTheAllowlist(t *testing.T) {
	env := newTestEnv(t)
	allowedDir := filepath.Join(env.root, "allowed")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("create allowed dir: %v", err)
	}
	secret := filepath.Join(env.root, "secret.txt")
	if err := os.WriteFile(secret, []byte("workspace secret"), 0o644); err != nil {
		t.Fatalf("write workspace secret: %v", err)
	}
	outside := filepath.Join(filepath.Dir(env.root), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside the workspace"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	grant := fullGrant()
	grant.AllowedPaths = []string{allowedDir}
	inst := newHostcallInstance(t, grant, env.deps)

	cases := []struct {
		name string
		path string
	}{
		{name: "inside the workspace but outside allowed_paths", path: secret},
		{name: "outside the workspace root", path: outside},
		{name: "a traversal spelling", path: filepath.Join(allowedDir, "..", "secret.txt")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := json.Marshal(map[string]string{"path": tc.path})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			out, err := inst.Invoke(context.Background(), opCallReadFile, req)
			if err != nil {
				t.Fatalf("Invoke(read_file): %v", err)
			}
			got := decodeHostError(t, out)
			if got.Code != CodeDenied {
				t.Errorf("read_file(%s) returned code %q, want %q (body %s)", tc.path, got.Code, CodeDenied, out)
			}
			if strings.Contains(got.Message, "workspace secret") || strings.Contains(got.Message, "outside the workspace") {
				t.Errorf("the denial message leaked file content: %q", got.Message)
			}
		})
	}
	if len(deniedEvents(t, env)) != len(cases) {
		t.Errorf("published %d denial events, want %d (one per refused read)", len(deniedEvents(t, env)), len(cases))
	}
}

// TestReadFileDeniesASymlinkEscape proves the fs check really goes through
// port.WorkspacePathGuard: a symlink inside the allowlist pointing out of the
// workspace is spelled entirely inside it, so only the guard's
// symlink-resolved comparison catches it.
func TestReadFileDeniesASymlinkEscape(t *testing.T) {
	env := newTestEnv(t)
	allowedDir := filepath.Join(env.root, "allowed")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("create allowed dir: %v", err)
	}
	outside := filepath.Join(filepath.Dir(env.root), "escape-target.txt")
	if err := os.WriteFile(outside, []byte("outside the workspace"), 0o644); err != nil {
		t.Fatalf("write escape target: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	link := filepath.Join(allowedDir, "escape.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}

	grant := fullGrant()
	grant.AllowedPaths = []string{allowedDir}
	inst := newHostcallInstance(t, grant, env.deps)

	req, err := json.Marshal(map[string]string{"path": link})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := inst.Invoke(context.Background(), opCallReadFile, req)
	if err != nil {
		t.Fatalf("Invoke(read_file): %v", err)
	}
	if got := decodeHostError(t, out); got.Code != CodeDenied {
		t.Errorf("read_file(symlink escape) returned code %q, want %q (body %s)", got.Code, CodeDenied, out)
	}
}

// TestReadFileDeniesASymlinkIntoTheWorkspaceOutsideTheAllowlist covers the
// escape a lexical allowlist test would miss: a link INSIDE allowed_paths whose
// target is a workspace file OUTSIDE them. The workspace guard is happy (the
// target is in the workspace) and the spelling is inside the allowlist, so only
// resolving the allowlist containment itself refuses it.
func TestReadFileDeniesASymlinkIntoTheWorkspaceOutsideTheAllowlist(t *testing.T) {
	env := newTestEnv(t)
	allowedDir := filepath.Join(env.root, "allowed")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatalf("create allowed dir: %v", err)
	}
	secret := filepath.Join(env.root, "secret.txt")
	if err := os.WriteFile(secret, []byte("workspace secret"), 0o644); err != nil {
		t.Fatalf("write workspace secret: %v", err)
	}
	link := filepath.Join(allowedDir, "shortcut.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}

	grant := fullGrant()
	grant.AllowedPaths = []string{allowedDir}
	inst := newHostcallInstance(t, grant, env.deps)

	req, err := json.Marshal(map[string]string{"path": link})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := inst.Invoke(context.Background(), opCallReadFile, req)
	if err != nil {
		t.Fatalf("Invoke(read_file): %v", err)
	}
	if strings.Contains(string(out), "workspace secret") {
		t.Fatalf("read_file followed a symlink out of allowed_paths and returned the target's content: %s", out)
	}
	if got := decodeHostError(t, out); got.Code != CodeDenied {
		t.Errorf("read_file(symlink inside allowed_paths -> outside) returned code %q, want %q (body %s)",
			got.Code, CodeDenied, out)
	}
}

// TestCheckAllowedPathIsFailClosed covers the allowlist containment rules
// directly, including the two malformed-allowlist cases that must not widen a
// grant: no entries at all, and an empty entry (filepath.Clean("") == ".").
func TestCheckAllowedPathIsFailClosed(t *testing.T) {
	root := t.TempDir()
	allowedDir := filepath.Join(root, "allowed")
	if err := os.MkdirAll(filepath.Join(allowedDir, "nested"), 0o755); err != nil {
		t.Fatalf("create allowed dir: %v", err)
	}
	inside := filepath.Join(allowedDir, "nested", "file.txt")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	outside := filepath.Join(root, "elsewhere.txt")

	cases := []struct {
		name    string
		grant   perm.Grant
		path    string
		wantErr bool
	}{
		{
			name:  "a file inside an allowed directory",
			grant: perm.Grant{FS: true, AllowedPaths: []string{allowedDir}},
			path:  inside,
		},
		{
			name:  "the allowed directory itself",
			grant: perm.Grant{FS: true, AllowedPaths: []string{allowedDir}},
			path:  allowedDir,
		},
		{
			name:    "a file outside every allowed directory",
			grant:   perm.Grant{FS: true, AllowedPaths: []string{allowedDir}},
			path:    outside,
			wantErr: true,
		},
		{
			name:    "an empty allowlist",
			grant:   perm.Grant{FS: true},
			path:    inside,
			wantErr: true,
		},
		{
			name:    "an empty allowlist entry",
			grant:   perm.Grant{FS: true, AllowedPaths: []string{""}},
			path:    inside,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAllowedPath(context.Background(), tc.grant, tc.path)
			if tc.wantErr && err == nil {
				t.Fatalf("checkAllowedPath(%s) = nil, want an error", tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkAllowedPath(%s) = %v, want nil", tc.path, err)
			}
		})
	}
}

func TestCallToolGoesThroughTheRegistryWithAPluginOrigin(t *testing.T) {
	env := newTestEnv(t)
	inst := newHostcallInstance(t, fullGrant(), env.deps)

	req, err := json.Marshal(map[string]any{
		"call_id":   "call-1",
		"tool":      "echo_tool",
		"arguments": map[string]string{"text": "hi"},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := inst.Invoke(context.Background(), opCallCallTool, req)
	if err != nil {
		t.Fatalf("Invoke(call_tool): %v", err)
	}
	var result domain.ToolResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("decode call_tool response %s: %v", out, err)
	}
	if !result.Success || result.Output != "echo:hi" || result.CallID != "call-1" {
		t.Errorf("call_tool response = %s, want the echo tool's successful result", out)
	}
	origins := env.recordedOrigins()
	if len(origins) != 1 || origins[0] != "plugin:"+testPluginName {
		t.Errorf("tool saw call origins %v, want exactly [plugin:%s]", origins, testPluginName)
	}
}

func TestCallToolReportsARegistryFailure(t *testing.T) {
	env := newTestEnv(t)
	inst := newHostcallInstance(t, fullGrant(), env.deps)

	req, err := json.Marshal(map[string]any{"tool": "no_such_tool"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	out, err := inst.Invoke(context.Background(), opCallCallTool, req)
	if err != nil {
		t.Fatalf("Invoke(call_tool): %v", err)
	}
	got := decodeHostError(t, out)
	if got.Code != CodeHostError {
		t.Errorf("call_tool(unknown tool) returned code %q, want %q (body %s)", got.Code, CodeHostError, out)
	}
	if !strings.Contains(got.Message, "no_such_tool") {
		t.Errorf("call_tool failure message %q does not name the tool", got.Message)
	}
}

// TestHostFunctionsRejectMalformedRequests covers the decode error paths: a
// body the host cannot parse must come back as an explicit error envelope, not
// as an empty success body.
func TestHostFunctionsRejectMalformedRequests(t *testing.T) {
	env := newTestEnv(t)
	grant := fullGrant()
	grant.AllowedHosts = []string{"allowed.example"}
	grant.AllowedPaths = []string{env.root}
	inst := newHostcallInstance(t, grant, env.deps)

	cases := []struct {
		name string
		op   int32
		body []byte
	}{
		{name: "read_file with broken json", op: opCallReadFile, body: []byte(`{"path":`)},
		{name: "read_file with an empty path", op: opCallReadFile, body: []byte(`{"path":""}`)},
		{name: "read_file with no body", op: opCallReadFile, body: nil},
		{name: "http_request with broken json", op: opCallHTTPRequest, body: []byte(`not json`)},
		{name: "http_request with no url", op: opCallHTTPRequest, body: []byte(`{"method":"GET"}`)},
		{name: "http_request with no method", op: opCallHTTPRequest, body: []byte(`{"url":"https://allowed.example/"}`)},
		{name: "call_tool with broken json", op: opCallCallTool, body: []byte(`{`)},
		{name: "call_tool with no tool name", op: opCallCallTool, body: []byte(`{"arguments":{}}`)},
		{name: "kv_get with an empty key", op: opCallKVGet, body: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := inst.Invoke(context.Background(), tc.op, tc.body)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if got := decodeHostError(t, out); got.Code != CodeInvalidRequest {
				t.Errorf("returned code %q, want %q (body %s)", got.Code, CodeInvalidRequest, out)
			}
		})
	}
}

// TestCallToolIgnoresAGuestSuppliedRiskLevel pins a deliberate omission: the
// guest cannot describe its own call's risk, because Registry.Execute only
// fills the descriptor's risk level in when the call carries none — a guest
// that could send risk_level="low" would be able to talk its way past the
// policy gates.
func TestCallToolIgnoresAGuestSuppliedRiskLevel(t *testing.T) {
	env := newTestEnv(t)
	var seen []string
	registry := tool.NewRegistry(nil, nil, nil)
	registry.RegisterDescriptor(
		tool.Descriptor{Name: "risky_tool", RiskLevel: "high"},
		tool.HandlerFunc(func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			seen = append(seen, call.RiskLevel)
			return domain.ToolResult{CallID: call.ID, Success: true}, nil
		}),
	)
	deps := env.deps
	deps.Tools = registry
	inst := newHostcallInstance(t, fullGrant(), deps)

	req, err := json.Marshal(map[string]any{"tool": "risky_tool", "risk_level": "low"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if _, err := inst.Invoke(context.Background(), opCallCallTool, req); err != nil {
		t.Fatalf("Invoke(call_tool): %v", err)
	}
	if len(seen) != 1 || seen[0] != "high" {
		t.Errorf("tool saw risk levels %v, want exactly [high] from its own descriptor", seen)
	}
}

// TestResultWriteBackFailureFailsLoudly covers the host's last error path: when
// the guest's allocator refuses, the host cannot deliver the result body. It
// must fail the call loudly (a trap the guest's caller sees) rather than return
// PackResult(0, 0), which the guest would read as "the call succeeded with no
// data".
func TestResultWriteBackFailureFailsLoudly(t *testing.T) {
	env := newTestEnv(t)
	inst := newHostcallInstance(t, fullGrant(), env.deps)
	ctx := context.Background()

	// Arm with a nil body: an input body would consume the arming on the
	// input allocation Invoke does itself.
	if _, err := inst.Invoke(ctx, opArmAllocFailure, nil); err != nil {
		t.Fatalf("Invoke(arm alloc failure): %v", err)
	}
	out, err := inst.Invoke(ctx, opCallConfigGet, nil)
	if err == nil {
		t.Fatalf("Invoke(config_get) with a failing guest allocator returned %s and no error, "+
			"want a loud failure", out)
	}
	// Pin the reason, so a future failure of this call for some unrelated
	// reason cannot keep the test green.
	if !strings.Contains(err.Error(), abi.ExportAlloc) || !strings.Contains(err.Error(), "null pointer") {
		t.Errorf("Invoke error = %v, want it to name the guest's refused %s", err, abi.ExportAlloc)
	}
	if !inst.Dead() {
		t.Error("Instance.Dead() = false after a trapped call, want true")
	}
}

// TestHostcallFixtureContractIsPinned pins the second fixture's import/export
// set: a rebuild that stops importing one of the host functions would quietly
// turn the link-time absence tests vacuous.
func TestHostcallFixtureContractIsPinned(t *testing.T) {
	ctx := context.Background()
	rt := NewRuntime(ctx, testMemoryPages)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	wasmBytes, err := hostcallWasm()
	if err != nil {
		t.Fatalf("read hostcall fixture wasm: %v", err)
	}
	compiled, err := Compile(ctx, rt, wasmBytes)
	if err != nil {
		t.Fatalf("Compile(hostcall fixture): %v", err)
	}

	gotExports := make([]string, 0, len(compiled.ExportedFunctions()))
	for name := range compiled.ExportedFunctions() {
		gotExports = append(gotExports, name)
	}
	sort.Strings(gotExports)
	wantExports := []string{"_initialize", abi.ExportAlloc, abi.ExportFree, abi.ExportInvoke}
	sort.Strings(wantExports)
	if strings.Join(gotExports, ",") != strings.Join(wantExports, ",") {
		t.Errorf("hostcall fixture exports %v, want exactly %v", gotExports, wantExports)
	}

	var hostImports []string
	for _, def := range compiled.ImportedFunctions() {
		module, name, _ := def.Import()
		switch module {
		case abi.HostModuleName:
			hostImports = append(hostImports, name)
		case "wasi_snapshot_preview1":
		default:
			t.Errorf("hostcall fixture imports %s.%s from an unexpected module", module, name)
		}
	}
	sort.Strings(hostImports)
	wantImports := []string{"call_tool", "config_get", "http_request", "kv_get", "kv_put", "log", "read_file"}
	if strings.Join(hostImports, ",") != strings.Join(wantImports, ",") {
		t.Errorf("hostcall fixture imports %v from %q, want exactly %v",
			hostImports, abi.HostModuleName, wantImports)
	}
}

// compile-time assertion that the fixture kv store satisfies the host's
// contract, so a signature drift fails here rather than at the wiring site.
var _ KVStore = (*memKV)(nil)
