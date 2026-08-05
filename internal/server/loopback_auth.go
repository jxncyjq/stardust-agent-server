package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
