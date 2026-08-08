// Package gitsource keeps a local checkout of a manifest repository in sync.
//
// It shells out to the git CLI rather than embedding a Go implementation, for
// the same reason internal/hauler shells out: git is already a runtime
// dependency of the image, and the CLI transparently honours every auth
// mechanism an operator already has configured -- ssh agents, credential
// helpers, insteadOf rewrites, custom CA bundles -- none of which a library
// would pick up without being told.
package gitsource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultTimeout bounds a single git invocation.
const DefaultTimeout = 5 * time.Minute

// Repo is a local mirror of one manifest repository.
type Repo struct {
	// URL is the remote. Any transport git understands works.
	URL string

	// Branch is the ref to track. Empty means the remote's default branch.
	Branch string

	// Dir is the local checkout path. It is created on first sync.
	Dir string

	// GitBin is the git executable. Empty resolves "git" through PATH.
	GitBin string

	// Timeout bounds a single git invocation.
	Timeout time.Duration

	// Env supplies extra environment entries as "KEY=value", for
	// GIT_SSH_COMMAND, GIT_ASKPASS, and similar.
	Env []string
}

// Error reports a failed git invocation.
type Error struct {
	Args   []string
	Output string
	Err    error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Err)
	if tail := lastLine(e.Output); tail != "" {
		return msg + ": " + tail
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Sync brings the local checkout up to date and returns the commit SHA now
// checked out.
//
// The working tree is treated as disposable: it is hard-reset to the remote
// ref rather than merged. A manifest repo is read-only input, and a merge
// conflict in a directory nobody edits by hand would wedge the reconciler.
func (r *Repo) Sync(ctx context.Context) (string, error) {
	if r.URL == "" {
		return "", errors.New("repository url is required")
	}
	if r.Dir == "" {
		return "", errors.New("checkout directory is required")
	}

	exists, err := r.isRepo()
	if err != nil {
		return "", err
	}
	if !exists {
		if err := r.clone(ctx); err != nil {
			return "", err
		}
	} else if err := r.fetch(ctx); err != nil {
		return "", err
	}

	return r.HeadSHA(ctx)
}

func (r *Repo) isRepo() (bool, error) {
	_, err := os.Stat(filepath.Join(r.Dir, ".git"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (r *Repo) clone(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(r.Dir), 0o755); err != nil {
		return fmt.Errorf("creating checkout parent: %w", err)
	}
	// A shallow clone is enough: only the tree at HEAD is ever read, and the
	// history of a manifest repo can be long.
	args := []string{"clone", "--depth", "1"}
	if r.Branch != "" {
		args = append(args, "--branch", r.Branch)
	}
	args = append(args, r.URL, r.Dir)

	_, err := r.run(ctx, "", args...)
	return err
}

func (r *Repo) fetch(ctx context.Context) error {
	branch := r.Branch
	if branch == "" {
		var err error
		if branch, err = r.remoteHead(ctx); err != nil {
			return err
		}
	}

	if _, err := r.run(ctx, r.Dir, "fetch", "--depth", "1", "origin", branch); err != nil {
		return err
	}
	if _, err := r.run(ctx, r.Dir, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return err
	}
	// Untracked files left by a previous run would otherwise be picked up by
	// the manifest glob and reconciled as if they were declared.
	_, err := r.run(ctx, r.Dir, "clean", "-fd")
	return err
}

// remoteHead resolves the remote's default branch.
func (r *Repo) remoteHead(ctx context.Context) (string, error) {
	out, err := r.run(ctx, r.Dir, "symbolic-ref", "--short", "HEAD")
	if err == nil {
		if b := strings.TrimSpace(out); b != "" {
			return b, nil
		}
	}
	// Detached head or an unusual setup: fall back to whatever origin says.
	out, err = r.run(ctx, r.Dir, "rev-parse", "--abbrev-ref", "origin/HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(strings.TrimSpace(out), "origin/"), nil
}

// HeadSHA returns the currently checked-out commit.
func (r *Repo) HeadSHA(ctx context.Context) (string, error) {
	out, err := r.run(ctx, r.Dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Manifests returns the files in the checkout matching glob, sorted, as paths
// relative to the checkout root.
//
// Paths are validated to stay inside the checkout: a glob is operator input,
// and a reconciler that can be pointed at /etc through a crafted pattern is a
// problem even when the operator is trusted.
func (r *Repo) Manifests(glob string) ([]string, error) {
	if glob == "" {
		glob = "*.yaml"
	}

	root, err := filepath.Abs(r.Dir)
	if err != nil {
		return nil, err
	}

	// Validate the pattern before globbing, not the matches after it.
	// filepath.Join cleans "../*.yaml" into a sibling of the checkout; if
	// nothing there happens to match, a post-hoc check would return an empty
	// list and silently accept a glob that escapes. Rejecting the pattern is
	// deterministic regardless of what is on disk.
	pattern := filepath.Join(root, glob)
	if !within(root, pattern) {
		return nil, fmt.Errorf("manifest glob %q escapes the checkout directory", glob)
	}

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid manifest glob %q: %w", glob, err)
	}

	var out []string
	for _, m := range matches {
		abs, err := filepath.Abs(m)
		if err != nil {
			continue
		}
		// Resolve symlinks too: a manifest repo is untrusted content, and a
		// symlink committed into it could otherwise point the reconciler at
		// a file outside the checkout.
		if resolved, err := filepath.EvalSymlinks(abs); err == nil && !within(root, resolved) {
			return nil, fmt.Errorf("manifest %q resolves outside the checkout directory", m)
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("manifest glob %q escapes the checkout directory", glob)
		}
		// Skip anything inside .git -- a glob like "**" would otherwise pull
		// in git's own YAML-looking internals.
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			continue
		}
		st, err := os.Stat(abs)
		if err != nil || st.IsDir() {
			continue
		}
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

// Path resolves a repository-relative manifest path to an absolute one.
func (r *Repo) Path(rel string) string { return filepath.Join(r.Dir, rel) }

func (r *Repo) bin() string {
	if r.GitBin == "" {
		return "git"
	}
	return r.GitBin
}

func (r *Repo) run(ctx context.Context, dir string, args ...string) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, r.bin(), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		// Never block a reconcile on an interactive credential prompt: fail
		// fast with a usable error instead of hanging until the timeout.
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ADVICE=0",
	)
	cmd.Env = append(cmd.Env, r.Env...)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s: %w", timeout, err)
		}
		return buf.String(), &Error{Args: args, Output: buf.String(), Err: err}
	}
	return buf.String(), nil
}

// within reports whether path is root itself or lives beneath it. Both are
// expected to be absolute and lexically clean.
func within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}
