package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

// Handshake is the self-connect credential handed to the frontend in App mode
// (spec §3.4 handshake). The webview can only reach the agent over TCP, so the
// actual base URL and one-time bearer token are exchanged out of band through a
// file the frontend reads at startup.
type Handshake struct {
	BaseURL string `json:"baseURL"`
	Token   string `json:"token"`
}

// GenerateLoopbackToken generates a one-time random bearer token (rotated on
// every startup). It returns 32 bytes of crypto/rand entropy hex-encoded (64
// hex chars). A failure to read the system CSPRNG is a fatal, non-recoverable
// condition and is surfaced as an error rather than masked with a weak token.
func GenerateLoopbackToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate loopback token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// WriteHandshake writes {baseURL, token} to the agreed file (the frontend reads
// it and self-connects with the Bearer token). The file is created with 0600
// permissions (owner-only) so other users on the same machine cannot steal the
// token.
func WriteHandshake(path string, h Handshake) error {
	b, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("marshal handshake: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write handshake %q: %w", path, err)
	}
	return nil
}

// LoopbackOriginGuard is the Origin-check middleware for App/loopback mode (one
// of the spec §3.4 hardening quartet). It blocks other processes or web pages on
// the same machine from reaching this loopback port on the user's behalf. The
// policy is deliberately narrow because the Bearer token remains the primary
// auth; Origin is defense-in-depth:
//   - no Origin header (webview same-origin fetch/EventSource and non-browser,
//     server-to-server clients usually omit it) → pass;
//   - Origin equal to this server's base URL → pass;
//   - any other (present but mismatched) Origin → 403.
//
// It composes with the Bearer token check (http.go) so both apply to every
// route; wrap the whole mux with it so SSE endpoints (/v1/browser/sessions/{id}/
// stream, /v1/events) are guarded too.
func LoopbackOriginGuard(next http.Handler, allowedOrigin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && origin != allowedOrigin {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
