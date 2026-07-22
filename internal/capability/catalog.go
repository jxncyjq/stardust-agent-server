// Package capability aggregates the agent's callable tools and its loadable
// skills into one read-only directory: each entry is a name, a group and a
// one-line summary, and each entry's full definition can be fetched on demand.
//
// It deliberately does not execute anything. Tool calls keep going through
// tool.Registry so permission, audit, timeout and the manual-approval gate all
// stay on the one path they were written for.
package capability

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// ErrUnknownCapability reports a name that no provider offers. Callers turn it
// into a message for the model rather than aborting the task.
var ErrUnknownCapability = errors.New("unknown capability")

// MaxSummaryChars bounds a catalog entry's one-line summary. The catalog sits
// in the prompt's cached prefix and is re-sent every round, so a runaway
// summary is paid for on every inference.
const MaxSummaryChars = 120

// Kind distinguishes what an entry is, because the two behave differently once
// loaded: a tool is invoked, a skill is read.
type Kind uint8

const (
	KindTool Kind = iota
	KindSkill
)

// String returns the lowercase name used in rendered catalogs.
func (k Kind) String() string {
	switch k {
	case KindTool:
		return "tool"
	case KindSkill:
		return "skill"
	default:
		return fmt.Sprintf("kind(%d)", uint8(k))
	}
}

// Entry is one line of the catalog.
type Entry struct {
	Name    string
	Group   string
	Summary string
	Kind    Kind
}

// Validate reports why an entry may not enter the catalog. Every field is
// author-controlled, so a violation is a fixable mistake in a tool descriptor
// or a skill's front matter -- reported, never trimmed away.
func (e Entry) Validate() error {
	if e.Name == "" {
		return errors.New("capability entry: name is empty")
	}
	if e.Group == "" {
		return fmt.Errorf("capability %q: group is empty", e.Name)
	}
	if e.Summary == "" {
		return fmt.Errorf("capability %q: summary is empty", e.Name)
	}
	if n := len([]rune(e.Summary)); n > MaxSummaryChars {
		return fmt.Errorf("capability %q: summary is %d chars, limit %d", e.Name, n, MaxSummaryChars)
	}
	return nil
}

// Provider supplies one class of capability.
type Provider interface {
	// Entries lists this provider's catalog lines.
	Entries(ctx context.Context) ([]Entry, error)
	// Detail returns the full definition of one capability: a tool's JSON
	// schema, or a skill's body. It returns ErrUnknownCapability for a name it
	// does not offer.
	Detail(ctx context.Context, name string) (string, error)
}

// Catalog is the aggregated, validated, deterministically ordered directory.
type Catalog struct {
	providers []Provider
}

// NewCatalog returns a Catalog over the given providers. Provider order does
// not affect output: entries are sorted by group then name.
func NewCatalog(providers ...Provider) *Catalog {
	return &Catalog{providers: providers}
}

// Entries returns every provider's entries, validated, checked for duplicate
// names, and sorted by (group, name).
//
// The ordering is not cosmetic: the rendered catalog goes into the prompt's
// cached prefix, so any instability in it costs a cache miss on every round.
func (c *Catalog) Entries(ctx context.Context) ([]Entry, error) {
	all := make([]Entry, 0)
	seen := make(map[string]string)
	for _, provider := range c.providers {
		entries, err := provider.Entries(ctx)
		if err != nil {
			return nil, fmt.Errorf("list capabilities: %w", err)
		}
		for _, entry := range entries {
			if err := entry.Validate(); err != nil {
				return nil, err
			}
			if group, ok := seen[entry.Name]; ok {
				return nil, fmt.Errorf("capability %q declared twice (groups %q and %q): a duplicate name has no single meaning for load or call", entry.Name, group, entry.Group)
			}
			seen[entry.Name] = entry.Group
			all = append(all, entry)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Group == all[j].Group {
			return all[i].Name < all[j].Name
		}
		return all[i].Group < all[j].Group
	})
	return all, nil
}

// Detail returns one capability's full definition, or ErrUnknownCapability.
func (c *Catalog) Detail(ctx context.Context, name string) (string, error) {
	for _, provider := range c.providers {
		detail, err := provider.Detail(ctx, name)
		if errors.Is(err, ErrUnknownCapability) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("load capability %q: %w", name, err)
		}
		return detail, nil
	}
	return "", fmt.Errorf("%w: %s", ErrUnknownCapability, name)
}
