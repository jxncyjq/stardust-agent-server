package fetch

import (
	"log/slog"
	"strings"
)

// EvictUntrusted removes a cached package that failed signature verification.
//
// It exists because a package that is not trusted is POISON: leaving it in the
// cache means those bytes stay in a directory the deployment reads from, and
// nothing else removes them — a cached untrusted package makes every later
// attempt read the same rejected bytes back, so the state persists until an
// operator goes in with rm.
//
// Callers must have established BOTH conditions before calling:
//
//  1. the failure carries manifest.ErrUntrustedPackage. Any other load failure
//     — a corrupt module, a missing file, an I/O error — is not a trust
//     problem, and evicting for it only makes the next attempt download an
//     identical broken package;
//  2. the package came from the cache (a remote entry with a digest). A local
//     entry's directory is the operator's own file tree, and deleting from it
//     would be deleting their files.
//
// The removal is LOGGED at Warn, because it is a deletion performed on the
// operator's behalf that they did not ask for. A removal that itself fails is
// logged too and otherwise ignored: the untrusted-package error is what the
// caller must return, and burying it under a cleanup failure would report the
// wrong problem.
//
// A nil cache or an empty digest means there is nothing addressable to remove
// (an embedder with no cache configured), which is a contract-declared absence
// rather than an error.
func EvictUntrusted(cache *Cache, digest string, logger *slog.Logger) {
	if cache == nil || strings.TrimSpace(digest) == "" {
		return
	}
	removed, err := cache.Remove(digest)
	if err != nil {
		if logger != nil {
			logger.Warn("could not evict an untrusted plugin package from the cache",
				"digest", digest, "error", err,
				"consequence", "the rejected package stays on disk until it is removed by hand "+
					"(`agent plugins cache remove`)")
		}
		return
	}
	if removed && logger != nil {
		logger.Warn("evicted an untrusted plugin package from the cache",
			"digest", digest,
			"reason", "signature verification failed, so the cached bytes are not trusted")
	}
}
