package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv(EnvDatabaseURL, "postgres://localhost/haulerweb")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	if c.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", c.ListenAddr, DefaultListenAddr)
	}
	if c.Concurrency != DefaultConcurrency {
		t.Errorf("Concurrency = %d, want %d", c.Concurrency, DefaultConcurrency)
	}
	if c.PollInterval != DefaultPollInterval {
		t.Errorf("PollInterval = %s, want %s", c.PollInterval, DefaultPollInterval)
	}
	if c.WorkerID == "" {
		t.Error("WorkerID should default to the hostname, got empty")
	}
	if !strings.HasPrefix(c.StoreDir, "/") {
		t.Errorf("StoreDir = %q, want an absolute path", c.StoreDir)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv(EnvDatabaseURL, "postgres://db/hw")
	t.Setenv(EnvListenAddr, "127.0.0.1:9999")
	t.Setenv(EnvConcurrency, "12")
	t.Setenv(EnvPollInterval, "90s")
	t.Setenv(EnvLogLevel, "DEBUG")
	t.Setenv(EnvWorkerID, "worker-0")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	if c.ListenAddr != "127.0.0.1:9999" {
		t.Errorf("ListenAddr = %q", c.ListenAddr)
	}
	if c.Concurrency != 12 {
		t.Errorf("Concurrency = %d, want 12", c.Concurrency)
	}
	if c.PollInterval != 90*time.Second {
		t.Errorf("PollInterval = %s, want 90s", c.PollInterval)
	}
	// Validate normalizes case so downstream comparisons need not.
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", c.LogLevel, "debug")
	}
	if c.WorkerID != "worker-0" {
		t.Errorf("WorkerID = %q, want %q", c.WorkerID, "worker-0")
	}
}

// Invalid values must be rejected, never clamped -- the same contract the
// hauler CLI holds for HAULER_CONCURRENCY and friends.
func TestLoadRejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{"non-numeric concurrency", EnvConcurrency, "many"},
		{"non-numeric log tail", EnvLogTailBytes, "32k"},
		{"unparseable poll interval", EnvPollInterval, "5 minutes"},
		{"unparseable hauler timeout", EnvHaulerTimeout, "forever"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvDatabaseURL, "postgres://db/hw")
			t.Setenv(tt.key, tt.val)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() with %s=%q: expected an error, got nil", tt.key, tt.val)
			}
		})
	}
}

func TestValidateRejectsOutOfRangeValues(t *testing.T) {
	base := func() *Config {
		return &Config{
			DatabaseURL:   "postgres://db/hw",
			ListenAddr:    DefaultListenAddr,
			StoreDir:      DefaultStoreDir,
			WorkDir:       DefaultWorkDir,
			HaulerBin:     DefaultHaulerBin,
			Concurrency:   DefaultConcurrency,
			LogLevel:      DefaultLogLevel,
			PollInterval:  DefaultPollInterval,
			HaulerTimeout: DefaultHaulerTimeout,
		}
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing database url", func(c *Config) { c.DatabaseURL = "" }},
		{"zero concurrency", func(c *Config) { c.Concurrency = 0 }},
		{"negative concurrency", func(c *Config) { c.Concurrency = -1 }},
		{"negative log tail", func(c *Config) { c.LogTailBytes = -1 }},
		{"poll interval below floor", func(c *Config) { c.PollInterval = time.Second }},
		{"zero hauler timeout", func(c *Config) { c.HaulerTimeout = 0 }},
		{"unknown log level", func(c *Config) { c.LogLevel = "verbose" }},
		{"empty store dir", func(c *Config) { c.StoreDir = "" }},
		{"empty hauler binary", func(c *Config) { c.HaulerBin = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mutate(c)
			if err := c.Validate(); err == nil {
				t.Fatal("Validate(): expected an error, got nil")
			}
		})
	}
}

func TestValidateIsIdempotent(t *testing.T) {
	c := &Config{
		DatabaseURL:   "postgres://db/hw",
		ListenAddr:    DefaultListenAddr,
		StoreDir:      "store",
		WorkDir:       "work",
		HaulerBin:     DefaultHaulerBin,
		Concurrency:   DefaultConcurrency,
		LogLevel:      "INFO",
		PollInterval:  DefaultPollInterval,
		HaulerTimeout: DefaultHaulerTimeout,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("first Validate() error = %v", err)
	}
	first := *c
	if err := c.Validate(); err != nil {
		t.Fatalf("second Validate() error = %v", err)
	}
	if c.StoreDir != first.StoreDir || c.WorkDir != first.WorkDir || c.LogLevel != first.LogLevel {
		t.Errorf("Validate() is not idempotent: %+v then %+v", first, *c)
	}
}
