// Package config resolves hauler-web's runtime configuration.
//
// Precedence mirrors the hauler CLI's own convention: an explicitly set flag
// wins over a HAULERWEB_* environment variable, which wins over the compiled
// default. Invalid values are rejected rather than clamped -- a typo'd
// concurrency of -1 should fail loudly at startup, not silently become 1.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Environment variable names. Every knob is settable from the environment so
// the Kubernetes deployment needs no config file.
const (
	EnvDatabaseURL     = "HAULERWEB_DATABASE_URL"
	EnvListenAddr      = "HAULERWEB_LISTEN_ADDR"
	EnvStoreDir        = "HAULERWEB_STORE_DIR"
	EnvTempDir         = "HAULERWEB_TEMP_DIR"
	EnvWorkDir         = "HAULERWEB_WORK_DIR"
	EnvHaulerBin       = "HAULERWEB_HAULER_BIN"
	EnvConcurrency     = "HAULERWEB_CONCURRENCY"
	EnvLogLevel        = "HAULERWEB_LOG_LEVEL"
	EnvPollInterval    = "HAULERWEB_POLL_INTERVAL"
	EnvHaulerTimeout   = "HAULERWEB_HAULER_TIMEOUT"
	EnvWorkerID        = "HAULERWEB_WORKER_ID"
	EnvLogTailBytes    = "HAULERWEB_LOG_TAIL_BYTES"
	EnvShutdownTimeout = "HAULERWEB_SHUTDOWN_TIMEOUT"
)

// Defaults.
const (
	DefaultListenAddr      = ":8080"
	DefaultStoreDir        = "/var/lib/hauler-web/store"
	DefaultWorkDir         = "/var/lib/hauler-web/work"
	DefaultHaulerBin       = "hauler"
	DefaultConcurrency     = 5
	DefaultLogLevel        = "info"
	DefaultPollInterval    = 5 * time.Minute
	DefaultHaulerTimeout   = 6 * time.Hour
	DefaultLogTailBytes    = 32 * 1024
	DefaultShutdownTimeout = 30 * time.Second
)

// MinPollInterval guards against a values.yaml typo turning the reconciler
// into a denial-of-service against the git remote and every upstream registry.
const MinPollInterval = 10 * time.Second

var validLogLevels = map[string]bool{
	"trace": true, "debug": true, "info": true, "warn": true, "error": true,
}

// Config is the fully resolved configuration. Nothing reads the environment
// after Load returns.
type Config struct {
	// DatabaseURL is the Postgres DSN. Required by every subcommand except
	// `version`.
	DatabaseURL string

	// ListenAddr is the web/API bind address.
	ListenAddr string

	// StoreDir is the persistent hauler OCI store. Exactly one worker may
	// write to it at a time; see the advisory lock in internal/jobs.
	StoreDir string

	// TempDir is passed to hauler as --tempdir. Empty means "let the OS
	// decide". Archive scratch space lands here, so in Kubernetes it is
	// usually a separate, cheaper volume than StoreDir.
	TempDir string

	// WorkDir holds git checkouts, generated manifests, and per-run docker
	// config files.
	WorkDir string

	// HaulerBin is the hauler executable: a bare name resolved through PATH,
	// or an absolute path.
	HaulerBin string

	// Concurrency is hauler's -j: how many artifacts it fetches at once.
	Concurrency int

	// LogLevel is one of trace, debug, info, warn, error.
	LogLevel string

	// PollInterval is the default git poll cadence for sources that do not
	// override it.
	PollInterval time.Duration

	// HaulerTimeout bounds a single hauler invocation. A large sync of a
	// Rancher release legitimately takes hours, so the default is generous.
	HaulerTimeout time.Duration

	// WorkerID identifies this worker when it claims jobs. Defaults to the
	// hostname, which in Kubernetes is the stable StatefulSet pod name.
	WorkerID string

	// LogTailBytes is how much of a run's output is kept inline in Postgres
	// so the UI renders without an object-store round-trip.
	LogTailBytes int

	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout time.Duration
}

// Load resolves configuration from the environment, applying defaults and
// validating every value.
//
// Flag overrides are applied by the caller before Validate: cobra binds flags
// to the returned struct's fields, then calls Validate. This keeps the
// "flag > env > default" precedence in one obvious place instead of smearing
// it across every command.
func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:     os.Getenv(EnvDatabaseURL),
		ListenAddr:      envString(EnvListenAddr, DefaultListenAddr),
		StoreDir:        envString(EnvStoreDir, DefaultStoreDir),
		TempDir:         os.Getenv(EnvTempDir),
		WorkDir:         envString(EnvWorkDir, DefaultWorkDir),
		HaulerBin:       envString(EnvHaulerBin, DefaultHaulerBin),
		LogLevel:        envString(EnvLogLevel, DefaultLogLevel),
		WorkerID:        os.Getenv(EnvWorkerID),
		ShutdownTimeout: DefaultShutdownTimeout,
	}

	var err error
	if c.Concurrency, err = envInt(EnvConcurrency, DefaultConcurrency); err != nil {
		return nil, err
	}
	if c.LogTailBytes, err = envInt(EnvLogTailBytes, DefaultLogTailBytes); err != nil {
		return nil, err
	}
	if c.PollInterval, err = envDuration(EnvPollInterval, DefaultPollInterval); err != nil {
		return nil, err
	}
	if c.HaulerTimeout, err = envDuration(EnvHaulerTimeout, DefaultHaulerTimeout); err != nil {
		return nil, err
	}
	if c.ShutdownTimeout, err = envDuration(EnvShutdownTimeout, DefaultShutdownTimeout); err != nil {
		return nil, err
	}

	if c.WorkerID == "" {
		host, herr := os.Hostname()
		if herr != nil || host == "" {
			host = "worker"
		}
		c.WorkerID = host
	}

	return c, nil
}

// Validate checks a Config that may have been mutated by flag binding since
// Load. It is safe to call more than once.
func (c *Config) Validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("database url is required: set %s", EnvDatabaseURL)
	}
	if c.Concurrency < 1 {
		return fmt.Errorf("concurrency must be at least 1, got %d", c.Concurrency)
	}
	if c.LogTailBytes < 0 {
		return fmt.Errorf("log tail bytes must not be negative, got %d", c.LogTailBytes)
	}
	if c.PollInterval < MinPollInterval {
		return fmt.Errorf("poll interval must be at least %s, got %s", MinPollInterval, c.PollInterval)
	}
	if c.HaulerTimeout <= 0 {
		return fmt.Errorf("hauler timeout must be positive, got %s", c.HaulerTimeout)
	}
	if !validLogLevels[strings.ToLower(c.LogLevel)] {
		return fmt.Errorf("invalid log level %q (want one of trace, debug, info, warn, error)", c.LogLevel)
	}
	c.LogLevel = strings.ToLower(c.LogLevel)

	if c.StoreDir == "" {
		return fmt.Errorf("store dir must not be empty")
	}
	if c.WorkDir == "" {
		return fmt.Errorf("work dir must not be empty")
	}
	if c.HaulerBin == "" {
		return fmt.Errorf("hauler binary must not be empty")
	}

	// Absolute paths keep hauler invocations independent of the process's
	// working directory, which changes between `serve`, `worker`, and tests.
	var err error
	if c.StoreDir, err = filepath.Abs(c.StoreDir); err != nil {
		return fmt.Errorf("resolving store dir: %w", err)
	}
	if c.WorkDir, err = filepath.Abs(c.WorkDir); err != nil {
		return fmt.Errorf("resolving work dir: %w", err)
	}
	if c.TempDir != "" {
		if c.TempDir, err = filepath.Abs(c.TempDir); err != nil {
			return fmt.Errorf("resolving temp dir: %w", err)
		}
	}

	return nil
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer", key, v)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration (try 5m, 90s, 2h)", key, v)
	}
	return d, nil
}
