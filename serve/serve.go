// Package serve exposes the agent HTTP service builder to external modules
// (such as legionAgentGUI) that cannot import internal/cli directly, since
// Go forbids importing an internal/ package from outside its module tree.
package serve

import (
	"context"

	"github.com/stardust/legion-agent/internal/cli"
	"github.com/stardust/legion-agent/internal/server"
)

// Options configures BuildService. It aliases the internal cli.ServeOptions so
// callers outside the module can construct it without reaching into internal/.
type Options = cli.ServeOptions

// Result holds the running service and its cleanup func. It aliases
// cli.ServeResult.
type Result = cli.ServeResult

// Tokens is the live holder of the credential the built service accepts.
//
// An embedder must read Result.Tokens.Current() per request rather than keeping
// Result.Token: the two agree until something rotates the credential, and after
// that the captured string is a token the server has already burned — every
// call made with it answers 401, with nothing on the embedder's screen to
// explain why.
type Tokens = server.TokenStore

// NewTokens builds a holder around a fixed token. An embedder does not need it
// for normal use — BuildService supplies one — but a test that drives the
// embedder without a real service does.
func NewTokens(token string) *Tokens { return server.NewTokenStore(token) }

// BuildService constructs a ready-to-Start agent service using the same
// dependency wiring as the `agent serve` command.
func BuildService(ctx context.Context, opts Options) (Result, error) {
	return cli.BuildServeService(ctx, opts)
}
