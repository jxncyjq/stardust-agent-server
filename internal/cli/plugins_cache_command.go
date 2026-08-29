package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/stardust/legion-agent/internal/config"
	"github.com/stardust/legion-agent/internal/plugin/fetch"
)

// pluginCacheConfigFlagUsage matches the --config help text every other
// plugins subcommand uses, so the flag reads the same wherever it appears.
const pluginCacheConfigFlagUsage = "agent JSON config file (default: ~/.stardust/agent.json, or $STARDUST_HOME/agent.json)"

// staleUnpackAge is how old a leftover ".unpack-*" staging directory must be
// before prune removes it.
//
// It is not zero because a staging directory belonging to a download happening
// RIGHT NOW looks exactly like one abandoned by a crash: both are a directory
// with a temp name and no entry beside it yet. An hour is far longer than any
// fetch this deployment allows (plugins.fetch.timeout_ms defaults to 30s), so
// anything older is certainly abandoned.
const staleUnpackAge = time.Hour

// newPluginsCacheCommand builds `agent plugins cache`, the operator's handle on
// the content-addressed package cache.
//
// It belongs to the group of plugin subcommands that touch no loader and start
// no service: like install/grant/deny it reads the plugins config and works on
// disk, and nothing it does reaches a running process until the next
// `agent plugins reload` re-fetches whatever it removed.
//
// The POLICY lives here rather than in internal/plugin/fetch on purpose:
// "which digests are still referenced" is a fact about plugins.json, and the
// cache package has no business knowing a deployment manifest exists. fetch
// provides the mechanism (List/Remove); this file decides what to call.
func newPluginsCacheCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect and reclaim the downloaded plugin package cache",
	}
	cmd.AddCommand(newPluginsCacheListCommand(out))
	cmd.AddCommand(newPluginsCacheRemoveCommand(out))
	cmd.AddCommand(newPluginsCachePruneCommand(out))
	return cmd
}

// cacheContext is everything the three subcommands need: the cache itself, and
// the digests the deployment still points at.
type cacheContext struct {
	cache *fetch.Cache
	// referencedBy maps a digest to the plugin names whose entries carry it.
	// A digest can be referenced by more than one entry (two plugins pinned to
	// the same package), which is why the value is a list.
	referencedBy map[string][]string
}

// loadCacheContext opens the configured cache and reads the deployment.
//
// A deployment with no plugins.cache is REFUSED rather than defaulted: there
// is no safe directory to guess, and these commands delete things. The same
// refusal is what `install` gives for a remote source, so an operator sees one
// consistent story about the setting.
func loadCacheContext(cmd *cobra.Command, configPath string) (cacheContext, error) {
	cfg, err := config.Load(cmd.Context(), config.Options{Path: configPath})
	if err != nil {
		return cacheContext{}, err
	}
	if strings.TrimSpace(cfg.Plugins.Cache) == "" {
		return cacheContext{}, fmt.Errorf(`plugin cache: no "plugins.cache" is configured, ` +
			`so there is no cache directory to work on`)
	}
	cache, err := fetch.NewCache(cfg.Plugins.Cache)
	if err != nil {
		return cacheContext{}, fmt.Errorf("open plugin cache %q: %w", cfg.Plugins.Cache, err)
	}

	referencedBy := map[string][]string{}
	if path := strings.TrimSpace(cfg.Plugins.Manifest); path != "" {
		deployment, err := readPluginDeployment(path)
		if err != nil {
			// An unreadable manifest is fatal here rather than "assume nothing
			// is referenced": that assumption would make prune delete every
			// package the deployment owns.
			return cacheContext{}, err
		}
		for _, entry := range deployment.Plugins {
			if digest := strings.TrimSpace(entry.Digest); digest != "" {
				referencedBy[strings.ToLower(digest)] = append(referencedBy[strings.ToLower(digest)], entry.Name)
			}
		}
	}
	return cacheContext{cache: cache, referencedBy: referencedBy}, nil
}

func (c cacheContext) references(digest string) []string {
	return c.referencedBy[strings.ToLower(digest)]
}

func newPluginsCacheListCommand(out io.Writer) *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cached plugin packages, their size, and whether the deployment still references them",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, err := loadCacheContext(cmd, configPath)
			if err != nil {
				return err
			}
			entries, err := ctx.cache.List()
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				fmt.Fprintln(out, "the plugin cache is empty")
				return nil
			}
			var total int64
			for _, entry := range entries {
				total += entry.Bytes
				state := "unreferenced"
				if names := ctx.references(entry.Digest); len(names) > 0 {
					state = "referenced by " + strings.Join(names, ",")
				}
				shape := "complete"
				if !entry.Complete {
					// A partial directory is neither usable nor visible to
					// Has(); saying so is the only way an operator learns it is
					// occupying space for nothing.
					shape = "INCOMPLETE"
				}
				fmt.Fprintf(out, "%s  %10d bytes  %s  %s  %s\n",
					entry.Digest, entry.Bytes, entry.ModTime.Format(time.RFC3339), shape, state)
			}
			fmt.Fprintf(out, "%d entries, %d bytes total\n", len(entries), total)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", pluginCacheConfigFlagUsage)
	return cmd
}

func newPluginsCacheRemoveCommand(out io.Writer) *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "remove <digest>",
		Short: "Remove one cached package by digest",
		Long: "Remove one cached package by digest.\n\n" +
			"The digest is removed even when the deployment still references it: an operator who " +
			"names a digest means that digest. What still points at it is printed, because the " +
			"next `agent plugins reload` will download it again.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, err := loadCacheContext(cmd, configPath)
			if err != nil {
				return err
			}
			digest := strings.TrimSpace(args[0])
			removed, err := ctx.cache.Remove(digest)
			if err != nil {
				return err
			}
			if !removed {
				fmt.Fprintf(out, "%s is not in the cache; nothing to remove.\n", digest)
				return nil
			}
			if names := ctx.references(digest); len(names) > 0 {
				fmt.Fprintf(out, "removed %s, which is still referenced by %s; "+
					"the next `agent plugins reload` will download it again.\n",
					digest, strings.Join(names, ","))
				return nil
			}
			fmt.Fprintf(out, "removed %s.\n", digest)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", pluginCacheConfigFlagUsage)
	return cmd
}

func newPluginsCachePruneCommand(out io.Writer) *cobra.Command {
	var (
		configPath string
		dryRun     bool
		maxBytes   int64
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove cached packages the deployment no longer references",
		Long: "Remove cached packages the deployment no longer references, plus staging " +
			"directories left behind by interrupted downloads.\n\n" +
			"With --max-bytes, keep pruning after that — oldest first — until the cache fits " +
			"the budget. Entries the deployment still references are NEVER removed: if the " +
			"budget cannot be met without them, the command says by how much it missed and " +
			"exits non-zero.\n\n" +
			"\"Oldest\" means least recently WRITTEN, not least recently used. Nothing in this " +
			"deployment records cache reads — doing so would mean a disk write on every hit — " +
			"so calling this an LRU policy would be a lie.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, err := loadCacheContext(cmd, configPath)
			if err != nil {
				return err
			}
			entries, err := ctx.cache.List()
			if err != nil {
				return err
			}

			// Two modes, and the difference is what --max-bytes is FOR.
			//
			// Without a budget every unreferenced entry goes: the operator asked
			// to reclaim what the deployment no longer points at.
			//
			// With a budget a warm cache is worth keeping — a package that is
			// unreferenced today is referenced again the moment someone rolls
			// plugins.json back — so only as many as the budget requires are
			// removed, oldest first.
			freed, remaining, err := pruneUnreferenced(out, ctx, entries, maxBytes, dryRun)
			if err != nil {
				return err
			}

			stale, err := ctx.cache.RemoveStaleStaging(staleUnpackAge, dryRun)
			if err != nil {
				return err
			}
			for _, name := range stale {
				if dryRun {
					fmt.Fprintf(out, "would remove staging directory %s\n", name)
				} else {
					fmt.Fprintf(out, "removed staging directory %s\n", name)
				}
			}

			if maxBytes <= 0 {
				fmt.Fprintf(out, "pruned %d bytes.\n", freed)
				return nil
			}
			if remaining > maxBytes {
				return fmt.Errorf("pruned %d bytes, but %d bytes remain against a %d byte budget: "+
					"the rest is referenced by the deployment manifest and is never removed here "+
					"(drop those entries from plugins.json first, or name a digest explicitly with "+
					"`agent plugins cache remove`)", freed, remaining, maxBytes)
			}
			fmt.Fprintf(out, "pruned %d bytes; %d bytes remain, within the %d byte budget.\n",
				freed, remaining, maxBytes)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", pluginCacheConfigFlagUsage)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"report what would be removed without removing anything")
	cmd.Flags().Int64Var(&maxBytes, "max-bytes", 0,
		"after pruning unreferenced entries, keep removing the oldest ones until the cache fits this "+
			"many bytes; referenced entries are never removed")
	return cmd
}

// pruneUnreferenced removes unreferenced entries, reporting how much it freed
// and how much is left in the cache.
//
// maxBytes <= 0 means "remove them all". A positive budget makes it
// incremental: entries go oldest-first and only while the total is over
// budget, so a cache that already fits is left alone and a warm package
// survives a prune it was not needed for.
//
// "Oldest" is least recently WRITTEN. Nothing in this deployment records cache
// reads — that would mean a disk write on every hit — so calling this LRU
// would be a lie, and the flag's help says so too.
//
// Referenced entries are never candidates: deleting a package the deployment
// points at to satisfy a disk target turns a space problem into a failed mount
// at the next reload.
func pruneUnreferenced(out io.Writer, ctx cacheContext, entries []fetch.CacheEntry, maxBytes int64, dryRun bool) (freed, remaining int64, err error) {
	var unreferenced []fetch.CacheEntry
	for _, entry := range entries {
		remaining += entry.Bytes
		if len(ctx.references(entry.Digest)) == 0 {
			unreferenced = append(unreferenced, entry)
		}
	}
	sort.Slice(unreferenced, func(i, j int) bool { return unreferenced[i].ModTime.Before(unreferenced[j].ModTime) })

	for _, entry := range unreferenced {
		if maxBytes > 0 && remaining <= maxBytes {
			break
		}
		if !dryRun {
			if _, rmErr := ctx.cache.Remove(entry.Digest); rmErr != nil {
				return freed, remaining, rmErr
			}
		}
		verb := "removed"
		if dryRun {
			verb = "would remove"
		}
		fmt.Fprintf(out, "%s %s (%d bytes, unreferenced)\n", verb, entry.Digest, entry.Bytes)
		freed += entry.Bytes
		remaining -= entry.Bytes
	}
	return freed, remaining, nil
}
