package gateway

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashID anonymizes a raw platform identifier (chat/user id) into a stable,
// non-reversible 16-hex-char token. Only the hash is ever handed to the core, so
// the runtime never sees raw platform ids; the raw value stays in the gateway's
// private binding store for outbound delivery.
func HashID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:16]
}
