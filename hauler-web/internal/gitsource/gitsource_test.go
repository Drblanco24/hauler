package gitsource

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run against real local git repositories. git is a runtime
// dependency of the image anyway, and a fake would only assert that this
// package's own argv strings are unchanged -- not that git does what the
// reconciler assumes (that a hard reset discards local edits, that a shallow
// fetch still moves HEAD).

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newUpstream creates a repo with one manifest and returns its path.
func newUpstream(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main", dir)
	writeFile(t, filepath.Join(dir, "images.yaml"), imagesManifest("alpine:3.20"))
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "initial")
	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func imagesManifest(refs ...string) string {
	var b strings.Builder
	b.WriteString("apiVersion: content.hauler.cattle.io/v1\nkind: Images\nmetadata:\n  name: t\nspec:\n  images:\n")
	for _, r := range refs {
		b.WriteString("    - name: " + r + "\n")
	}
	return b.String()
}

func newRepo(t *testing.T, upstream string) *Repo {
	t.Helper()
	return &Repo{URL: upstream, Branch: "main", Dir: filepath.Join(t.TempDir(), "checkout")}
}

func TestSyncClonesThenFetches(t *testing.T) {
	upstream := newUpstream(t)
	r := newRepo(t, upstream)
	ctx := context.Background()

	sha1, err := r.Sync(ctx)
	if err != nil {
		t.Fatalf("first Sync() error = %v", err)
	}
	if len(sha1) != 40 {
		t.Errorf("Sync() = %q, want a 40-char sha", sha1)
	}

	// An unchanged upstream must yield the same sha, which is what lets the
	// reconciler short-circuit without doing any work.
	sha2, err := r.Sync(ctx)
	if err != nil {
		t.Fatalf("second Sync() error = %v", err)
	}
	if sha1 != sha2 {
		t.Errorf("Sync() changed with no upstream commit: %q then %q", sha1, sha2)
	}

	// A new upstream commit must be picked up.
	writeFile(t, filepath.Join(upstream, "images.yaml"), imagesManifest("alpine:3.20", "busybox:stable"))
	git(t, upstream, "add", "-A")
	git(t, upstream, "commit", "-qm", "add busybox")

	sha3, err := r.Sync(ctx)
	if err != nil {
		t.Fatalf("third Sync() error = %v", err)
	}
	if sha3 == sha1 {
		t.Error("Sync() did not advance after an upstream commit")
	}

	body, err := os.ReadFile(r.Path("images.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "busybox") {
		t.Errorf("checkout did not receive the new content:\n%s", body)
	}
}

// The checkout is disposable input. Local edits must never survive to be
// reconciled as if they were declared in git.
func TestSyncDiscardsLocalModifications(t *testing.T) {
	upstream := newUpstream(t)
	r := newRepo(t, upstream)
	ctx := context.Background()

	if _, err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	writeFile(t, r.Path("images.yaml"), imagesManifest("evil:latest"))
	writeFile(t, r.Path("stray.yaml"), imagesManifest("also-evil:latest"))

	if _, err := r.Sync(ctx); err != nil {
		t.Fatalf("Sync() after local edits error = %v", err)
	}

	body, err := os.ReadFile(r.Path("images.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "evil") {
		t.Errorf("a local modification survived Sync():\n%s", body)
	}
	if _, err := os.Stat(r.Path("stray.yaml")); !os.IsNotExist(err) {
		t.Error("an untracked file survived Sync(); it would be reconciled as declared")
	}
}

func TestManifestsGlob(t *testing.T) {
	upstream := newUpstream(t)
	writeFile(t, filepath.Join(upstream, "images", "prod.yaml"), imagesManifest("a:1"))
	writeFile(t, filepath.Join(upstream, "images", "platform.yml"), imagesManifest("b:1"))
	writeFile(t, filepath.Join(upstream, "images", "README.md"), "not a manifest")
	git(t, upstream, "add", "-A")
	git(t, upstream, "commit", "-qm", "add manifests")

	r := newRepo(t, upstream)
	if _, err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		glob string
		want []string
	}{
		{"images/*.yaml", []string{"images/prod.yaml"}},
		{"images/*.y*ml", []string{"images/platform.yml", "images/prod.yaml"}},
		{"*.yaml", []string{"images.yaml"}},
		{"nothing/*.yaml", nil},
		{"", []string{"images.yaml"}}, // default
	}
	for _, tt := range tests {
		t.Run(tt.glob, func(t *testing.T) {
			got, err := r.Manifests(tt.glob)
			if err != nil {
				t.Fatalf("Manifests() error = %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Manifests(%q) = %v, want %v", tt.glob, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Manifests(%q)[%d] = %q, want %q", tt.glob, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// A glob is operator input; it must not be able to reach outside the checkout.
func TestManifestsRejectsEscapingGlob(t *testing.T) {
	upstream := newUpstream(t)
	r := newRepo(t, upstream)
	if _, err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Manifests("../*.yaml"); err == nil {
		t.Error("Manifests(\"../*.yaml\") error = nil, want a rejection")
	}
	if _, err := r.Manifests("../../../../etc/*.conf"); err == nil {
		t.Error("a deep traversal glob was accepted")
	}
}

// Directories and git's own internals must never be returned as manifests.
func TestManifestsSkipsDirectoriesAndGitInternals(t *testing.T) {
	upstream := newUpstream(t)
	if err := os.MkdirAll(filepath.Join(upstream, "dir.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(upstream, "dir.yaml", "inner.yaml"), imagesManifest("a:1"))
	git(t, upstream, "add", "-A")
	git(t, upstream, "commit", "-qm", "add dir")

	r := newRepo(t, upstream)
	if _, err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := r.Manifests("*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		if m == "dir.yaml" {
			t.Errorf("Manifests() returned a directory: %v", got)
		}
		if strings.HasPrefix(m, ".git") {
			t.Errorf("Manifests() returned a git internal: %q", m)
		}
	}
}

func TestSyncRejectsMissingConfiguration(t *testing.T) {
	ctx := context.Background()
	if _, err := (&Repo{Dir: "/tmp/x"}).Sync(ctx); err == nil {
		t.Error("Sync() with no URL: expected an error")
	}
	if _, err := (&Repo{URL: "https://example/x.git"}).Sync(ctx); err == nil {
		t.Error("Sync() with no Dir: expected an error")
	}
}

func TestSyncReportsUnreachableRemote(t *testing.T) {
	r := &Repo{
		URL:    filepath.Join(t.TempDir(), "does-not-exist.git"),
		Branch: "main",
		Dir:    filepath.Join(t.TempDir(), "checkout"),
	}
	_, err := r.Sync(context.Background())
	if err == nil {
		t.Fatal("Sync() against a missing remote error = nil, want an error")
	}
	// The message must name git and carry git's own output, or an operator
	// debugging a credential problem has nothing to go on.
	if !strings.Contains(err.Error(), "git ") {
		t.Errorf("Error() = %q, want it to name the git command", err)
	}
}

func TestSyncTracksNonDefaultBranch(t *testing.T) {
	upstream := newUpstream(t)
	git(t, upstream, "checkout", "-q", "-b", "release")
	writeFile(t, filepath.Join(upstream, "images.yaml"), imagesManifest("release-only:1"))
	git(t, upstream, "add", "-A")
	git(t, upstream, "commit", "-qm", "release content")
	git(t, upstream, "checkout", "-q", "main")

	r := &Repo{URL: upstream, Branch: "release", Dir: filepath.Join(t.TempDir(), "checkout")}
	if _, err := r.Sync(context.Background()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	body, err := os.ReadFile(r.Path("images.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "release-only") {
		t.Errorf("checked out the wrong branch:\n%s", body)
	}
}
