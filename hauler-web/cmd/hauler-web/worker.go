package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Drblanco24/hauler-web/internal/config"
	"github.com/Drblanco24/hauler-web/internal/db"
	"github.com/Drblanco24/hauler-web/internal/hauler"
	"github.com/Drblanco24/hauler-web/internal/reconcile"
	"github.com/Drblanco24/hauler-web/internal/registryclient"
)

// StaleRunAge is how long a run may sit in 'running' before a starting worker
// treats it as abandoned. Generous, because a legitimate sync of a large
// release genuinely takes hours.
const StaleRunAge = 12 * time.Hour

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply database migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			database, err := db.Open(cmd.Context(), cfg.DatabaseURL)
			if err != nil {
				return err
			}
			defer database.Close()

			if err := database.Migrate(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "migrations applied")
			return nil
		},
	}
	return cmd
}

func newReconcileCmd() *cobra.Command {
	var (
		sourceName  string
		forceRepull bool
		once        bool
	)

	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Run one reconcile pass over every enabled source",
		Long: `Syncs each source's manifest repository, works out what is missing, pulls it
with hauler, delivers it to every enabled target, and records the result.

Unlike ` + "`plan`" + `, this changes things.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_ = once // accepted for symmetry with worker; reconcile is always one pass
			return runReconcile(cmd, sourceName, forceRepull)
		},
	}

	fl := cmd.Flags()
	fl.StringVar(&sourceName, "source", "", "Reconcile only this source (default: all enabled)")
	fl.BoolVar(&forceRepull, "force-repull", false, "Re-pull everything, ignoring store and delivery state")
	fl.BoolVar(&once, "once", true, "Run a single pass (always true; use `worker` to loop)")

	return cmd
}

func newWorkerCmd() *cobra.Command {
	var interval time.Duration

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Continuously reconcile every enabled source",
		Long: `Polls each source's manifest repository on an interval and reconciles any
change. This is the long-running process a Deployment runs.

Exactly one worker may write to a given hauler store; a Postgres advisory lock
enforces that, and a second worker will decline rather than corrupt the store.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorker(cmd, interval)
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 0,
		"Poll interval (default: $"+config.EnvPollInterval+", then 5m)")
	return cmd
}

func loadConfig() (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "trace", "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

// build assembles the reconciler and its dependencies from configuration.
func build(ctx context.Context, cfg *config.Config) (*db.DB, *reconcile.Reconciler, error) {
	database, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}

	rec := &reconcile.Reconciler{
		DB: database,
		Hauler: &hauler.Client{
			Bin:      cfg.HaulerBin,
			StoreDir: cfg.StoreDir,
			TempDir:  cfg.TempDir,
			Timeout:  cfg.HaulerTimeout,
		},
		Registry:     &registryclient.Client{},
		WorkDir:      cfg.WorkDir,
		Concurrency:  cfg.Concurrency,
		LogTailBytes: cfg.LogTailBytes,
		Logger:       newLogger(cfg.LogLevel),
	}
	return database, rec, nil
}

func runReconcile(cmd *cobra.Command, sourceName string, force bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := cmd.Context()

	database, rec, err := build(ctx, cfg)
	if err != nil {
		return err
	}
	defer database.Close()

	sources, err := selectSources(ctx, database, sourceName)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no enabled sources configured")
		return nil
	}

	var failed int
	for _, src := range sources {
		res, err := rec.Once(ctx, reconcile.Options{
			Source: src, Trigger: "manual", ForceRepull: force,
		})
		if err != nil {
			failed++
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", src.Name, err)
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s: run %d %s (%s)\n",
			src.Name, res.RunID, res.Status, res.Plan.Summary())
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d sources failed", failed, len(sources))
	}
	return nil
}

func selectSources(ctx context.Context, database *db.DB, name string) ([]db.Source, error) {
	if name == "" {
		return database.ListEnabledSources(ctx)
	}
	src, err := database.GetSourceByName(ctx, name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, fmt.Errorf("no source named %q", name)
		}
		return nil, err
	}
	return []db.Source{*src}, nil
}

func runWorker(cmd *cobra.Command, interval time.Duration) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if interval <= 0 {
		interval = cfg.PollInterval
	}
	ctx := cmd.Context()
	log := newLogger(cfg.LogLevel)

	database, rec, err := build(ctx, cfg)
	if err != nil {
		return err
	}
	defer database.Close()

	// A worker that was killed mid-sync leaves a run stuck in 'running'.
	// Clearing those at startup is what stops one crash from wedging the
	// view of in-flight work forever.
	if n, err := database.FailStaleRuns(ctx, StaleRunAge); err != nil {
		log.Warn("could not sweep stale runs", "error", err)
	} else if n > 0 {
		log.Info("marked abandoned runs as failed", "count", n)
	}

	log.Info("worker started", "interval", interval, "store", cfg.StoreDir)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		pass(ctx, database, rec, log)

		select {
		case <-ctx.Done():
			log.Info("worker stopping")
			return nil
		case <-ticker.C:
		}
	}
}

// pass reconciles every enabled source once. Errors are logged rather than
// returned: one broken source must not stop the worker from serving the others
// on the next tick.
func pass(ctx context.Context, database *db.DB, rec *reconcile.Reconciler, log *slog.Logger) {
	sources, err := database.ListEnabledSources(ctx)
	if err != nil {
		log.Error("could not list sources", "error", err)
		return
	}

	for _, src := range sources {
		res, err := rec.Once(ctx, reconcile.Options{
			Source: src, Trigger: "schedule", SkipUnchanged: true,
		})
		switch {
		case errors.Is(err, reconcile.ErrStoreBusy):
			log.Debug("store busy, will retry", "source", src.Name)
		case err != nil:
			log.Error("reconcile failed", "source", src.Name, "error", err)
		case res.Skipped:
			log.Debug("no change", "source", src.Name)
		default:
			log.Info("reconciled", "source", src.Name, "run", res.RunID,
				"status", res.Status, "summary", res.Plan.Summary())
		}
	}
}
