package manifest

import "fmt"

// DraftEntry converts an already-verified PluginManifest into an Entry
// ready to be written into plugins.json. It is a pure function: it touches
// no filesystem, makes no request, and reads no config. Verifying pm is the
// caller's job (see PluginManifest's doc comment); source and digest are
// what the caller resolved the plugin's package to (a remote URL with its
// sha256, or a local package path with an empty digest, matching Entry's
// own Source/Digest pairing rules).
//
// The draft is deliberately inert:
//
//   - Entry.Enabled is always false. There is no parameter to override this
//     — an "install and enable in one step" path would immediately be used
//     in scripts, and installing a plugin must never authorize it to run.
//   - Entry.Grant.Capabilities is always empty, for the same reason: this
//     layer does not authorize anything.
//   - Entry.Tools maps pm.Tools to []ToolAccept{Name: ...} in the same
//     order pm.Tools declares them, carrying no RiskLevel or Sensitive
//     overrides — those exist for the deployment to tighten a plugin's own
//     declared risk level, and a draft does not decide that for the
//     operator.
//   - Entry.Name comes from pm.Name, never from a caller-supplied string,
//     so the entry's name always matches the plugin's self-declared
//     identity.
//
// DraftEntry returns an error naming pm.Name if pm.Tools is empty: such a
// plugin would be refused by AssembleSpec anyway (internal/plugin/manifest/
// assemble.go), so failing here names the cause earlier and more
// actionably.
func DraftEntry(pm PluginManifest, source, digest string) (Entry, error) {
	if len(pm.Tools) == 0 {
		return Entry{}, fmt.Errorf("draft entry for plugin %q: manifest declares no tools", pm.Name)
	}

	tools := make([]ToolAccept, len(pm.Tools))
	for i, tool := range pm.Tools {
		tools[i] = ToolAccept{Name: tool.Name}
	}

	return Entry{
		Name:    pm.Name,
		Source:  source,
		Digest:  digest,
		Enabled: false,
		Grant:   GrantDecl{Capabilities: []string{}},
		Tools:   tools,
	}, nil
}
