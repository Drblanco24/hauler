// Command hauler-web operates hauler from a git-controlled set of manifests
// and records what it moved.
//
// The subcommands present here are the ones that are actually implemented.
// `serve`, `worker`, and `migrate` arrive with the database and web phases;
// adding them as stubs now would make the CLI lie about what it can do.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Drblanco24/hauler-web/internal/config"
	"github.com/Drblanco24/hauler-web/internal/hauler"
	"github.com/Drblanco24/hauler-web/internal/manifest"
	"github.com/Drblanco24/hauler-web/internal/planner"
	"github.com/Drblanco24/hauler-web/internal/registryclient"
)

// version is stamped at build time via -ldflags.
var version = "devel"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "hauler-web",
		Short:         "Stateful image movement on top of hauler",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newVersionCmd(), newPlanCmd())
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the hauler-web version and the hauler binary it will drive",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "hauler-web %s\n", version)

			// The hauler version is part of the answer: two deployments of
			// the same hauler-web behave differently if they drive different
			// hauler binaries, and every run records which one it used.
			c := &hauler.Client{Bin: os.Getenv(config.EnvHaulerBin), Timeout: 30 * time.Second}
			hv, err := c.Version(cmd.Context())
			if err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "hauler     unavailable (%v)\n", err)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "hauler     %s\n", hv)
			return nil
		},
	}
}

type planFlags struct {
	manifests    string
	storeDir     string
	haulerBin    string
	haulerDir    string
	insecure     bool
	forceRepull  bool
	pruneOrphans bool
	timeout      time.Duration
}

func newPlanCmd() *cobra.Command {
	var f planFlags

	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show what a reconcile would do, without changing anything",
		Long: `Reads hauler manifests from a directory, resolves each image's current
digest upstream, compares that against the local hauler store, and prints the
resulting plan.

This is the drift view as a command. It performs no pulls, no pushes, and no
writes -- it only reads manifests, HEADs upstream registries, and inspects the
store.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runPlan(cmd, f) },
	}

	fl := cmd.Flags()
	fl.StringVarP(&f.manifests, "manifests", "m", ".", "Directory containing hauler manifests")
	fl.StringVarP(&f.storeDir, "store", "s", "", "hauler store directory (defaults to $"+config.EnvStoreDir+")")
	fl.StringVar(&f.haulerBin, "hauler-bin", "", "Path to the hauler binary (defaults to $"+config.EnvHaulerBin+", then PATH)")
	fl.StringVar(&f.haulerDir, "hauler-dir", "", "hauler home directory")
	fl.BoolVar(&f.insecure, "insecure", false, "Allow plain HTTP and skip TLS verification when resolving digests")
	fl.BoolVar(&f.forceRepull, "force-repull", false, "Plan a re-pull of everything, ignoring store and delivery state")
	fl.BoolVar(&f.pruneOrphans, "prune-orphans", false, "Include entries the manifests no longer declare")
	fl.DurationVar(&f.timeout, "timeout", 5*time.Minute, "Overall timeout")

	return cmd
}

func runPlan(cmd *cobra.Command, f planFlags) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
	defer cancel()

	out := cmd.OutOrStdout()

	// 1. Desired state: every image declared by the manifests on disk.
	desired, err := loadDesired(f.manifests)
	if err != nil {
		return err
	}
	if len(desired) == 0 {
		fmt.Fprintf(out, "no images declared in %s\n", f.manifests)
		return nil
	}

	// 2. Upstream truth: what each reference resolves to right now.
	rc := &registryclient.Client{Insecure: f.insecure}
	refs := make([]registryclient.Reference, 0, len(desired))
	for _, d := range desired {
		refs = append(refs, registryclient.Reference{Ref: d.Ref, Platform: d.Platform})
	}
	digests, failures := rc.Resolve(ctx, refs)

	// 3. Local truth: what the store already holds. A missing store is the
	// normal first run, so it must not be an error.
	storeDir := f.storeDir
	if storeDir == "" {
		storeDir = os.Getenv(config.EnvStoreDir)
	}
	if storeDir == "" {
		storeDir = config.DefaultStoreDir
	}
	hc := &hauler.Client{
		Bin:       firstNonEmpty(f.haulerBin, os.Getenv(config.EnvHaulerBin)),
		StoreDir:  storeDir,
		HaulerDir: f.haulerDir,
		Timeout:   f.timeout,
	}
	info, err := hc.InfoIfExists(ctx, hauler.InfoOptions{})
	if err != nil {
		return fmt.Errorf("reading the hauler store: %w", err)
	}

	// Without a database there is no record of previous runs, so every entry
	// looks new unless its digest is already in the store. That is the
	// correct conservative answer for a stateless plan.
	known := make(map[string]string, len(desired))
	for _, d := range desired {
		if info.HasDigest(digests[d.Key()]) {
			known[d.Key()] = d.SpecHash
		}
	}

	plan := planner.Build(desired, planner.Observed{
		ResolvedDigest: digests,
		ResolveError:   failures,
		KnownSpecHash:  known,
		InStore:        info.HasDigest,
	}, planner.Options{
		ForceRepull:  f.forceRepull,
		PruneOrphans: f.pruneOrphans,
	})

	printPlan(out, plan, storeDir, len(info.Artifacts))

	if len(plan.Errors()) > 0 {
		return fmt.Errorf("%d of %d entries could not be resolved", len(plan.Errors()), len(plan.Items))
	}
	return nil
}

// loadDesired parses every manifest in dir into planner input.
func loadDesired(dir string) ([]planner.Desired, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("reading manifests: %w", err)
	}

	var paths []string
	if st.IsDir() {
		for _, pattern := range []string{"*.yaml", "*.yml"} {
			found, gerr := filepath.Glob(filepath.Join(dir, pattern))
			if gerr != nil {
				return nil, gerr
			}
			paths = append(paths, found...)
		}
		sort.Strings(paths)
	} else {
		paths = []string{dir}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no .yaml or .yml manifests found in %s", dir)
	}

	seen := make(map[string]bool)
	var desired []planner.Desired
	for _, p := range paths {
		m, perr := manifest.ParseFile(p)
		if perr != nil {
			return nil, perr
		}
		for _, e := range m.Images() {
			hash, herr := e.SpecHash()
			if herr != nil {
				return nil, herr
			}
			d := planner.Desired{
				Ref:      manifest.NormalizeRef(e.Ref()),
				Platform: e.Platform(),
				SpecHash: hash,
			}
			// The same image can legitimately appear in two manifests;
			// planning it twice would double-count every statistic.
			if seen[d.Key()] {
				continue
			}
			seen[d.Key()] = true
			desired = append(desired, d)
		}
	}
	return desired, nil
}

func printPlan(out io.Writer, plan *planner.Plan, storeDir string, artifacts int) {
	fmt.Fprintf(out, "store %s (%d artifacts)\n\n", storeDir, artifacts)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTION\tREFERENCE\tPLATFORM\tDIGEST\tREASON")
	for _, it := range plan.Items {
		digest := it.Digest
		if len(digest) > 19 {
			digest = digest[:19] + "..."
		}
		platform := it.Desired.Platform
		if platform == "" {
			platform = "-"
		}
		if digest == "" {
			digest = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			it.Action, it.Desired.Ref, platform, digest, it.Reason)
	}
	_ = tw.Flush()

	fmt.Fprintf(out, "\n%s\n", plan.Summary())
	if !plan.HasWork() {
		fmt.Fprintln(out, "everything is already pulled and delivered; nothing to do")
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
