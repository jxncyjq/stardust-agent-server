package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stardust/legion-agent/internal/domain"
)

// fileTestSessionStore is a minimal SessionStore stub that returns a fixed
// GetAgentSession result, so GET /v1/files tests can control exactly what
// session (and working directory) the handler resolves without pulling in a
// real storage backend.
type fileTestSessionStore struct {
	session domain.AgentSession
	found   bool
	err     error
}

func (f *fileTestSessionStore) ListAgentSessions(ctx context.Context, companyID string, agentID string) ([]domain.AgentSession, error) {
	return nil, nil
}

func (f *fileTestSessionStore) ListConversationTurns(ctx context.Context, sessionID string, limit int) ([]domain.ConversationTurn, error) {
	return nil, nil
}

func (f *fileTestSessionStore) GetAgentSession(ctx context.Context, sessionID string) (domain.AgentSession, bool, error) {
	return f.session, f.found, f.err
}

func (f *fileTestSessionStore) SaveAgentSession(ctx context.Context, session domain.AgentSession) error {
	return nil
}

func (f *fileTestSessionStore) DeleteAgentSession(ctx context.Context, sessionID string) error {
	return nil
}

// filesRouteURL builds a GET /v1/files request URL from query parameters,
// mirroring how the fileURL builder under test assembles the same route.
func filesRouteURL(sessionID, relPath string, download bool) string {
	v := url.Values{}
	if sessionID != "" {
		v.Set("session_id", sessionID)
	}
	if relPath != "" {
		v.Set("path", relPath)
	}
	if download {
		v.Set("download", "1")
	}
	return "/v1/files?" + v.Encode()
}

// TestHTTPServerServeFileReturnsFileContent guards the happy path: a valid
// session whose WorkingDir contains the requested file streams back with a
// 200, a Content-Type derived from the file extension, and the file bytes as
// the body.
func TestHTTPServerServeFileReturnsFileContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := "hello from generated file\n"
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte(want), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v, want nil", err)
	}
	sessions := &fileTestSessionStore{
		session: domain.AgentSession{ID: "session-1", WorkingDir: dir},
		found:   true,
	}
	srv := NewHTTPServer(Config{AdminToken: "token", Sessions: sessions})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, filesRouteURL("session-1", "report.txt", false), nil)
	req.Header.Set("Authorization", "Bearer token")
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/files status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != want {
		t.Fatalf("GET /v1/files body = %q, want %q", rec.Body.String(), want)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("GET /v1/files Content-Type = %q, want text/plain (for .txt)", ct)
	}
}

// TestHTTPServerServeFileDownloadSetsContentDisposition guards that
// ?download=1 marks the response as an attachment so browsers save it
// instead of rendering it inline.
func TestHTTPServerServeFileDownloadSetsContentDisposition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.csv"), []byte("a,b,c\n"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v, want nil", err)
	}
	sessions := &fileTestSessionStore{
		session: domain.AgentSession{ID: "session-1", WorkingDir: dir},
		found:   true,
	}
	srv := NewHTTPServer(Config{AdminToken: "token", Sessions: sessions})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, filesRouteURL("session-1", "report.csv", true), nil)
	req.Header.Set("Authorization", "Bearer token")
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/files?download=1 status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("GET /v1/files?download=1 Content-Disposition = %q, want attachment prefix", cd)
	}
	if !strings.Contains(cd, "report.csv") {
		t.Fatalf("GET /v1/files?download=1 Content-Disposition = %q, want filename report.csv", cd)
	}
}

// TestHTTPServerServeFilePathTraversalForbidden guards the workspace-root
// confinement: a path escaping the session's WorkingDir must be refused
// rather than served, regardless of whether a file exists there.
func TestHTTPServerServeFilePathTraversalForbidden(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	secret := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v, want nil", err)
	}
	workDir := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatalf("Mkdir error = %v, want nil", err)
	}
	sessions := &fileTestSessionStore{
		session: domain.AgentSession{ID: "session-1", WorkingDir: workDir},
		found:   true,
	}
	srv := NewHTTPServer(Config{AdminToken: "token", Sessions: sessions})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, filesRouteURL("session-1", "../secret.txt", false), nil)
	req.Header.Set("Authorization", "Bearer token")
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /v1/files (traversal) status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

// TestHTTPServerServeFileMissingFileReturnsNotFound guards that a
// well-formed request for a file that does not exist under the session's
// WorkingDir reports 404 rather than a lower-level OS error.
func TestHTTPServerServeFileMissingFileReturnsNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sessions := &fileTestSessionStore{
		session: domain.AgentSession{ID: "session-1", WorkingDir: dir},
		found:   true,
	}
	srv := NewHTTPServer(Config{AdminToken: "token", Sessions: sessions})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, filesRouteURL("session-1", "does-not-exist.txt", false), nil)
	req.Header.Set("Authorization", "Bearer token")
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/files (missing file) status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

// TestHTTPServerServeFileSessionWithoutWorkingDirReturnsNotFound guards that a
// session with no bound working directory cannot serve files (there is no
// root to confine the read to).
func TestHTTPServerServeFileSessionWithoutWorkingDirReturnsNotFound(t *testing.T) {
	t.Parallel()
	sessions := &fileTestSessionStore{
		session: domain.AgentSession{ID: "session-1", WorkingDir: ""},
		found:   true,
	}
	srv := NewHTTPServer(Config{AdminToken: "token", Sessions: sessions})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, filesRouteURL("session-1", "report.txt", false), nil)
	req.Header.Set("Authorization", "Bearer token")
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/files (no working dir) status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

// TestHTTPServerServeFileUnknownSessionReturnsNotFound guards that a
// session_id the store does not recognize is reported as 404, not served or
// mistaken for an empty-working-dir session.
func TestHTTPServerServeFileUnknownSessionReturnsNotFound(t *testing.T) {
	t.Parallel()
	sessions := &fileTestSessionStore{found: false}
	srv := NewHTTPServer(Config{AdminToken: "token", Sessions: sessions})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, filesRouteURL("no-such-session", "report.txt", false), nil)
	req.Header.Set("Authorization", "Bearer token")
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/files (unknown session) status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

// TestHTTPServerServeFileMissingOrInvalidAuthReturnsUnauthorized guards that
// the endpoint is covered by the same loopback admin-token gate as every
// other route (enforced centrally in HTTPServer.authorized), not just
// reachable by anyone who knows a session id.
func TestHTTPServerServeFileMissingOrInvalidAuthReturnsUnauthorized(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v, want nil", err)
	}
	sessions := &fileTestSessionStore{
		session: domain.AgentSession{ID: "session-1", WorkingDir: dir},
		found:   true,
	}
	srv := NewHTTPServer(Config{AdminToken: "token", Sessions: sessions})

	for name, mutate := range map[string]func(*http.Request){
		"missing header": func(r *http.Request) {},
		"wrong token":    func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") },
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, filesRouteURL("session-1", "report.txt", false), nil)
			mutate(req)
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("GET /v1/files (%s) status = %d, want %d body=%s", name, rec.Code, http.StatusUnauthorized, rec.Body.String())
			}
		})
	}
}

// TestHTTPServerFileURLBuildsRelativePathByDefault guards the fileURL
// builder's no-FileBaseURL contract: without a configured public base it
// returns a relative "/v1/files?..." path the loopback frontend resolves
// against its own origin.
func TestHTTPServerFileURLBuildsRelativePathByDefault(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{AdminToken: "token"})
	got := srv.fileURL("session-1", "out/report.txt", false)
	want := "/v1/files?path=out%2Freport.txt&session_id=session-1"
	if got != want {
		t.Fatalf("fileURL() = %q, want %q", got, want)
	}
}

// TestHTTPServerFileURLUsesFileBaseURLWhenConfigured guards that a configured
// FileBaseURL produces an absolute link suitable for a deployment where the
// GUI is not on the same origin as the agent server.
func TestHTTPServerFileURLUsesFileBaseURLWhenConfigured(t *testing.T) {
	t.Parallel()
	srv := NewHTTPServer(Config{AdminToken: "token", FileBaseURL: "https://agent.example.com"})
	got := srv.fileURL("session-1", "out/report.txt", true)
	want := "https://agent.example.com/v1/files?download=1&path=out%2Freport.txt&session_id=session-1"
	if got != want {
		t.Fatalf("fileURL() = %q, want %q", got, want)
	}
}
