// Package serve exposes the agent HTTP service builder to external modules
// (such as legionAgentGUI) that cannot import internal/cli directly, since
// Go forbids importing an internal/ package from outside its module tree.
package serve

import (
	"context"

	"github.com/stardust/legion-agent/internal/cli"
)

// Options configures BuildService. It aliases the internal cli.ServeOptions so
// callers outside the module can construct it without reaching into internal/.
type Options = cli.ServeOptions

// Result holds the running service and its cleanup func. It aliases
// cli.ServeResult.
type Result = cli.ServeResult

// BuildService constructs a ready-to-Start agent service using the same
// dependency wiring as the `agent serve` command.
func BuildService(ctx context.Context, opts Options) (Result, error) {
	return cli.BuildServeService(ctx, opts)
}
