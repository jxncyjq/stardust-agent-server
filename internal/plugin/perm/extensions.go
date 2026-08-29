package perm

import (
	"fmt"
	"sort"
	"strings"
)

// Extension names a place in the host's own machinery that a plugin may be
// consulted at.
//
// Extensions are the OTHER DIRECTION from capabilities. A capability
// (log/config/kv/http/fs/tool) gates the guest calling the host, and is
// enforced at link time: the host does not register a host function the
// deployment did not grant, so an ungranted capability makes the module fail
// to instantiate. An extension gates the HOST CALLING THE GUEST, which no
// import list can express — the enforcement is that the host never registers
// the plugin at that seam in the first place.
//
// One lock does not open both doors, which is why this is a separate grant
// dimension rather than six more capability names.
type Extension string

const (
	// ExtensionObserve lets a plugin be notified, read-only, after a tool call
	// has produced a result. It can change nothing: its answer is discarded
	// and its failures count only against its own health.
	ExtensionObserve Extension = "observe"

	// ExtensionDecide lets a plugin be consulted BEFORE a tool call is
	// dispatched, to answer allow or deny.
	//
	// It can only TIGHTEN. The host's own enforcer and policy run first, and a
	// call they refused is never shown to a plugin at all — so there is no
	// position from which a plugin could turn a refusal into a permission. A
	// plugin's "allow" means "I do not object", never "I authorize".
	ExtensionDecide Extension = "decide"
)

// knownExtensions is the complete set of extension names this host
// implements, in the order they are reported.
//
// A name outside it is REFUSED rather than ignored — the same stance
// capabilities take. A plugin that declares an extension nobody implements
// would otherwise believe it is being consulted while nothing ever calls it,
// which is the worst of the three possible outcomes (working, refused,
// silently inert).
var knownExtensions = []Extension{ExtensionObserve, ExtensionDecide}

// Extensions is the set of extension points a plugin has actually been
// granted, as the host consults them.
//
// It is a struct of bools rather than a set of strings for the same reason
// Grant is: the host asks "may this plugin observe?" at one specific place,
// and a bool field makes that question a compile-time-checked one.
type Extensions struct {
	// Observe is ExtensionObserve.
	Observe bool
	// Decide is ExtensionDecide.
	Decide bool
}

// Any reports whether any extension point at all was granted. It is what the
// caller checks before doing work that only matters to a plugin that
// participates in the host's machinery.
func (e Extensions) Any() bool { return e.Observe || e.Decide }

// Names renders the granted extensions, sorted, for errors and diagnostics.
func (e Extensions) Names() []string {
	var names []string
	if e.Observe {
		names = append(names, string(ExtensionObserve))
	}
	if e.Decide {
		names = append(names, string(ExtensionDecide))
	}
	sort.Strings(names)
	return names
}

// ParseExtensions turns a list of extension names into an Extensions.
//
// It refuses an unknown name (naming it and listing what is supported) and a
// repeated one. A repeat is refused rather than deduplicated because a
// deployment file that says the same thing twice is a file to fix: silently
// accepting it hides the copy-paste that produced it, and the next edit is as
// likely to make the two copies disagree as to keep them the same.
func ParseExtensions(names []string) (Extensions, error) {
	var parsed Extensions
	seen := make(map[Extension]struct{}, len(names))
	for _, raw := range names {
		name := Extension(strings.TrimSpace(raw))
		if name == "" {
			return Extensions{}, fmt.Errorf("extension name is empty")
		}
		if _, dup := seen[name]; dup {
			return Extensions{}, fmt.Errorf("extension %q appears twice", name)
		}
		seen[name] = struct{}{}

		switch name {
		case ExtensionObserve:
			parsed.Observe = true
		case ExtensionDecide:
			parsed.Decide = true
		default:
			return Extensions{}, fmt.Errorf("unknown extension %q; supported: %s",
				name, strings.Join(knownExtensionNames(), ", "))
		}
	}
	return parsed, nil
}

// Intersect returns the extensions granted by BOTH sides: what the plugin
// declared it wants and what the deployment granted.
//
// The intersection is the whole enforcement story for extensions, and it is
// deliberately a SUBSET relation rather than the equality capabilities
// require. "You may observe, but you may not decide" is a sentence a
// deployment must be able to say; making the grant an exact match of the
// declaration would delete it.
func (e Extensions) Intersect(other Extensions) Extensions {
	return Extensions{
		Observe: e.Observe && other.Observe,
		Decide:  e.Decide && other.Decide,
	}
}

func knownExtensionNames() []string {
	names := make([]string, 0, len(knownExtensions))
	for _, name := range knownExtensions {
		names = append(names, string(name))
	}
	return names
}
