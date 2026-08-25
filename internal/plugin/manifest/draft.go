package manifest

import "fmt"

// DraftEntry converts an already-verified PluginManifest into an Entry
// ready to be written into plugins.json. It is a pure function: it touches
// no filesystem, makes no request, and reads no config. Verifying pm is the
// caller's job (see PluginManifest's doc comment); source and digest are
// what the caller resolved the plugin's package to (a remote URL with its
// sha256, or a local package path with an empty digest), and DraftEntry
// itself enforces that the pair matches Entry's own Source/Digest pairing
// rules — a source and digest that do not pair (a foreign URL scheme, an
// empty source, a remote source with no or malformed digest, a local
// source carrying a digest, or a URL carrying userinfo) is refused here,
// naming the drafting step, rather than accepted and left for
// ParseDeployment to reject one layer later against a less actionable parse
// failure.
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
// plugin would already have been refused by ParsePlugin (manifest.go), so
// an already-verified PluginManifest cannot trip this guard — it is kept as
// defence in depth for a hand-built PluginManifest, failing here names the
// cause earlier and more actionably than a later, unrelated failure would.
func DraftEntry(pm PluginManifest, source, digest string) (Entry, error) {
	if len(pm.Tools) == 0 {
		return Entry{}, fmt.Errorf("draft entry for plugin %q: manifest declares no tools", pm.Name)
	}
	if source == "" {
		return Entry{}, fmt.Errorf("draft entry for plugin %q: source is empty", pm.Name)
	}

	tools := make([]ToolAccept, len(pm.Tools))
	for i, tool := range pm.Tools {
		tools[i] = ToolAccept{Name: tool.Name}
	}

	entry := Entry{
		Name:    pm.Name,
		Source:  source,
		Digest:  digest,
		Enabled: false,
		Grant:   GrantDecl{Capabilities: []string{}},
		Tools:   tools,
	}

	if err := rejectForeignScheme(pm.Name, source); err != nil {
		return Entry{}, fmt.Errorf("draft entry for plugin %q: %w", pm.Name, err)
	}
	if err := validateEntrySource(entry); err != nil {
		return Entry{}, fmt.Errorf("draft entry for plugin %q: %w", pm.Name, err)
	}

	return entry, nil
}
