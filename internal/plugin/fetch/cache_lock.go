package fetch

import (
	"errors"
	"io/fs"
)

// lockCreateIsContended reports whether a failed O_CREATE|O_EXCL create of the
// digest lock file means "another writer holds it right now" — the one case
// lockDigestDir waits out — rather than a condition no amount of waiting will
// fix.
//
// The split matters in one direction more than the other. Treating contention
// as fatal fails a plugin install that had nothing wrong with it, and does so
// at random, only under load; treating a genuine failure as contention costs a
// bounded wait and then reports it anyway. So the ordinary "the file is
// already there" answer is joined by whatever else the platform returns for
// the same situation (see the platform files) — and nothing else.
func lockCreateIsContended(err error) bool {
	if errors.Is(err, fs.ErrExist) {
		return true
	}
	return lockCreateIsContendedOnThisPlatform(err)
}
