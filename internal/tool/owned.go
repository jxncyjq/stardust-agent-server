package tool

import (
	"github.com/stardust/legion-agent/internal/lifecycle"
)

// RegisterOwned registers one tool and files its revocation under owner in
// ledger, returning the same one-shot handle the ledger hands out.
//
// Ownership follows the CREATOR, not the registry the handler lands in: a
// plugin that contributes a tool owns removing it, so the registry never has to
// track who contributed what. Disposing the owner removes every tool it
// contributed, in reverse registration order.
func RegisterOwned(
	ledger *lifecycle.Ledger,
	owner lifecycle.Owner,
	r *Registry,
	descriptor Descriptor,
	handler Handler,
) func() error {
	revoke := r.RegisterDescriptor(descriptor, handler)
	return ledger.Add(owner, "tool:"+descriptor.Name, func() error {
		revoke()
		return nil
	})
}
