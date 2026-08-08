// Package hauler drives the hauler CLI as a subprocess.
//
// hauler-web deliberately does not import hauler's Go packages. Shelling out
// keeps this service free of hauler's transitive graph (helm v4, cosign v3,
// containerd v2, apimachinery), lets an operator pin or roll back the hauler
// version independently of hauler-web, and means an upstream refactor of an
// internal package cannot break a deployment. The contract between the two is
// hauler's CLI surface and its documented `store info -o json` output, both of
// which are stable.
package hauler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LogLevelSilent is hauler's zerolog level string that suppresses all log
// output. It matters because hauler writes its logs to stdout -- the same
// stream `store info -o json` prints to -- so any command whose stdout is
// parsed must run silent. See Client.Info.
const LogLevelSilent = "disabled"

// Client runs hauler commands against a single store directory.
//
// A Client is safe for concurrent use, but the store it points at is not:
// hauler takes a lock on the store index, and concurrent writers to one store
// are a corruption risk. Serialising access is the caller's job (see the
// advisory lock in internal/jobs).
type Client struct {
	// Bin is the hauler executable: a bare name resolved via PATH, or an
	// absolute path.
	Bin string

	// StoreDir is passed as --store.
	StoreDir string

	// TempDir is passed as --tempdir when non-empty.
	TempDir string

	// HaulerDir is passed as --haulerdir when non-empty. Pointing it at a
	// per-deployment directory keeps hauler's stores.json and audit.log off
	// the home directory of whatever uid the container runs as.
	HaulerDir string

	// DockerConfig, when non-empty, is exported as DOCKER_CONFIG so hauler
	// picks up registry credentials from a per-run config.json. This is
	// preferred over `hauler login`, which mutates shared state that
	// concurrent runs would race on.
	DockerConfig string

	// Retries is passed as --retries when > 0.
	Retries int

	// Timeout bounds a single invocation. Zero means no timeout beyond the
	// caller's context.
	Timeout time.Duration

	// Env supplies additional environment entries as "KEY=value". They are
	// appended to the parent environment, so later entries win.
	Env []string

	// LogLevel is passed as --log-level. Empty means hauler's default.
	LogLevel string

	// WaitDelay bounds how long Wait blocks on inherited pipes after the
	// process has been cancelled. Zero uses DefaultWaitDelay.
	WaitDelay time.Duration
}

// DefaultWaitDelay is the grace period between killing hauler and forcing its
// output pipes closed.
const DefaultWaitDelay = 5 * time.Second

func (c *Client) waitDelay() time.Duration {
	if c.WaitDelay > 0 {
		return c.WaitDelay
	}
	return DefaultWaitDelay
}

// Result captures one hauler invocation.
type Result struct {
	Args     []string
	ExitCode int
	Duration time.Duration

	// Stdout and Stderr hold the captured streams. For long syncs the caller
	// should also pass a sink to stream output live; these buffers exist so
	// errors can quote what actually happened.
	Stdout string
	Stderr string
}

// Error reports a non-zero exit from hauler.
type Error struct {
	Result *Result
	Err    error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("hauler %s: %v", strings.Join(e.Result.Args, " "), e.Err)
	// hauler reports most failures on stdout because that is where its logger
	// writes, so check both before giving up on a useful message.
	if tail := lastNonEmptyLines(e.Result.Stderr, 3); tail != "" {
		return msg + ": " + tail
	}
	if tail := lastNonEmptyLines(e.Result.Stdout, 3); tail != "" {
		return msg + ": " + tail
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// ExitCode returns hauler's exit status, or -1 if it never ran.
func (e *Error) ExitCode() int { return e.Result.ExitCode }

// SyncOptions mirrors the subset of `hauler store sync` flags hauler-web sets.
type SyncOptions struct {
	// Filenames are manifests to sync (-f). At least one is required.
	Filenames []string

	// ImageTxts are plain image lists to sync (-i).
	ImageTxts []string

	// Concurrency is -j. Zero leaves hauler's default.
	Concurrency int

	// Platform is -p, applied to entries that do not set their own.
	Platform string

	// Registry is -g, a default registry for unqualified references.
	Registry string

	// ExcludeExtras drops cosign signatures, attestations, SBOMs, and OCI
	// referrers.
	ExcludeExtras bool

	// IgnoreErrors maps to --ignore-errors: warn and continue rather than
	// failing the whole sync on one bad image. The planner records per-image
	// outcomes from `store info` afterwards either way.
	IgnoreErrors bool
}

// Sync pulls content into the store. Output is streamed to sink as it is
// produced; pass nil to discard it.
func (c *Client) Sync(ctx context.Context, o SyncOptions, sink io.Writer) (*Result, error) {
	if len(o.Filenames) == 0 && len(o.ImageTxts) == 0 {
		return nil, errors.New("sync requires at least one manifest or image list")
	}

	args := []string{"store", "sync"}
	for _, f := range o.Filenames {
		args = append(args, "--filename", f)
	}
	for _, f := range o.ImageTxts {
		args = append(args, "--image-txt", f)
	}
	if o.Concurrency > 0 {
		args = append(args, "--concurrency", strconv.Itoa(o.Concurrency))
	}
	if o.Platform != "" {
		args = append(args, "--platform", o.Platform)
	}
	if o.Registry != "" {
		args = append(args, "--registry", o.Registry)
	}
	if o.ExcludeExtras {
		args = append(args, "--exclude-extras")
	}
	if o.IgnoreErrors {
		args = append(args, "--ignore-errors")
	}
	// The live progress renderer draws braille spinners and redraws rows in
	// place. Useful on a terminal, unreadable in a persisted run log.
	args = append(args, "--no-progress")

	return c.run(ctx, c.withStoreFlags(args), sink)
}

// CopyOptions mirrors `hauler store copy`.
type CopyOptions struct {
	// Target is the destination, e.g. "registry://harbor.example/airgap" or
	// "dir://./out".
	Target string

	Insecure  bool
	PlainHTTP bool

	// Only restricts the copy to specific image items.
	Only string
}

// Copy pushes store contents to a registry or directory target.
func (c *Client) Copy(ctx context.Context, o CopyOptions, sink io.Writer) (*Result, error) {
	if o.Target == "" {
		return nil, errors.New("copy requires a target")
	}
	args := []string{"store", "copy", o.Target}
	if o.Insecure {
		args = append(args, "--insecure")
	}
	if o.PlainHTTP {
		args = append(args, "--plain-http")
	}
	if o.Only != "" {
		args = append(args, "--only", o.Only)
	}
	return c.run(ctx, c.withStoreFlags(args), sink)
}

// SaveOptions mirrors `hauler store save`.
type SaveOptions struct {
	// Filename is the haul archive to write, e.g. "haul-abc1234.tar.zst".
	Filename string

	// ChunkSize splits the archive, e.g. "2G". Empty writes one file.
	ChunkSize string
}

// Save writes the store to a haul archive.
func (c *Client) Save(ctx context.Context, o SaveOptions, sink io.Writer) (*Result, error) {
	if o.Filename == "" {
		return nil, errors.New("save requires a filename")
	}
	args := []string{"store", "save", "--filename", o.Filename}
	if o.ChunkSize != "" {
		args = append(args, "--chunk-size", o.ChunkSize)
	}
	return c.run(ctx, c.withStoreFlags(args), sink)
}

// Remove deletes a reference from the store. hauler garbage-collects the
// orphaned blobs itself.
func (c *Client) Remove(ctx context.Context, ref string, sink io.Writer) (*Result, error) {
	if ref == "" {
		return nil, errors.New("remove requires a reference")
	}
	return c.run(ctx, c.withStoreFlags([]string{"store", "remove", ref}), sink)
}

// withStoreFlags appends the store-scoped persistent flags. Cobra accepts
// persistent flags anywhere after the command that declares them, so
// appending at the end is safe and keeps the argv readable in logs.
func (c *Client) withStoreFlags(args []string) []string {
	if c.StoreDir != "" {
		args = append(args, "--store", c.StoreDir)
	}
	if c.TempDir != "" {
		args = append(args, "--tempdir", c.TempDir)
	}
	if c.Retries > 0 {
		args = append(args, "--retries", strconv.Itoa(c.Retries))
	}
	return args
}

func (c *Client) withRootFlags(args []string, logLevel string) []string {
	if logLevel != "" {
		args = append(args, "--log-level", logLevel)
	}
	if c.HaulerDir != "" {
		args = append(args, "--haulerdir", c.HaulerDir)
	}
	return args
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "hauler"
	}
	return c.Bin
}

// run executes hauler at the client's configured log level, streaming
// combined output to sink.
func (c *Client) run(ctx context.Context, args []string, sink io.Writer) (*Result, error) {
	return c.runWithLogLevel(ctx, args, c.LogLevel, sink)
}

// runWithLogLevel executes hauler and streams output to sink as it is
// produced, while also buffering it for error reporting.
func (c *Client) runWithLogLevel(ctx context.Context, args []string, logLevel string, sink io.Writer) (*Result, error) {
	args = c.withRootFlags(args, logLevel)

	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Env = c.environ()
	setProcessGroup(cmd)
	// Backstop for the case where a killed process leaves a pipe open anyway:
	// after cancellation, Wait gives I/O this long before closing the
	// descriptors itself and returning.
	cmd.WaitDelay = c.waitDelay()

	var stdout, stderr bytes.Buffer
	// A mutex around the sink keeps stdout and stderr from interleaving
	// mid-line when both are written concurrently by exec's copier
	// goroutines.
	var mu sync.Mutex
	tee := func(buf *bytes.Buffer) io.Writer {
		if sink == nil {
			return buf
		}
		return io.MultiWriter(buf, &lockedWriter{mu: &mu, w: sink})
	}
	cmd.Stdout = tee(&stdout)
	cmd.Stderr = tee(&stderr)

	started := time.Now()
	err := cmd.Run()
	res := &Result{
		Args:     args,
		Duration: time.Since(started),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: -1,
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	if err != nil {
		// Distinguish "we killed it" from "hauler failed", because a timeout
		// on a six-hour sync means something very different to an operator
		// than a manifest error.
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			err = fmt.Errorf("timed out after %s: %w", c.Timeout, err)
		case errors.Is(ctx.Err(), context.Canceled):
			err = fmt.Errorf("cancelled: %w", err)
		}
		return res, &Error{Result: res, Err: err}
	}
	return res, nil
}

func (c *Client) environ() []string {
	env := os.Environ()
	if c.DockerConfig != "" {
		env = append(env, "DOCKER_CONFIG="+c.DockerConfig)
	}
	env = append(env, c.Env...)
	return env
}

// lockedWriter serialises writes from the stdout and stderr copiers so lines
// in the streamed run log stay intact.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func lastNonEmptyLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			kept = append([]string{strings.TrimSpace(lines[i])}, kept...)
		}
	}
	return strings.Join(kept, "; ")
}
