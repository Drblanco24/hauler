//go:build integration

package hauler

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run against a real hauler binary. The unit tests pin the argv
// hauler-web constructs; these pin the assumptions it makes about hauler's
// actual behaviour -- that `--log-level disabled` really does silence the
// logger that shares stdout with the JSON, and that `store info -o json`
// really does deserialise into StoreInfo.
//
//	go test -tags=integration ./internal/hauler/ \
//	  -hauler-bin=/usr/local/bin/hauler \
//	  -haul=testdata/haul.tar.zst
//
// Both are also readable from the environment so CI can set them once.

func testBin(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("HAULERWEB_TEST_HAULER_BIN")
	if bin == "" {
		bin = "hauler"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		t.Skipf("hauler binary %q not found: %v", bin, err)
	}
	return resolved
}

func newIntegrationClient(t *testing.T, storeDir string) *Client {
	t.Helper()
	return &Client{
		Bin:       testBin(t),
		StoreDir:  storeDir,
		HaulerDir: filepath.Join(t.TempDir(), "haulerdir"),
		Timeout:   10 * time.Minute,
		LogLevel:  "info",
	}
}

func TestIntegrationVersion(t *testing.T) {
	c := newIntegrationClient(t, t.TempDir())

	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version() error = %v", err)
	}
	// hauler prints a large ASCII banner before the version payload on the
	// human-readable path. If that banner ever reaches --json output, the
	// JSON extractor is what saves us -- assert we got a version, not a
	// fragment of ASCII art.
	if !strings.HasPrefix(v, "v") {
		t.Errorf("Version() = %q, want something starting with 'v'", v)
	}
	t.Logf("hauler version: %s", v)
}

// The load-bearing assumption of Info: hauler's logger writes to stdout, the
// same stream the JSON is printed on, and `--log-level disabled` suppresses
// it. If a future hauler release stops honouring that level, this fails here
// rather than by silently returning a zero-artifact inventory in production.
func TestIntegrationInfoOnRealStore(t *testing.T) {
	haul := os.Getenv("HAULERWEB_TEST_HAUL")
	if haul == "" {
		t.Skip("set HAULERWEB_TEST_HAUL to a haul.tar.zst to run this test")
	}
	if _, err := os.Stat(haul); err != nil {
		t.Skipf("haul %q not readable: %v", haul, err)
	}

	storeDir := filepath.Join(t.TempDir(), "store")
	c := newIntegrationClient(t, storeDir)

	// Populate the store from an archive so the test needs no network.
	load := exec.Command(c.Bin, "store", "load", "--filename", haul,
		"--store", storeDir, "--haulerdir", c.HaulerDir, "--log-level", "warn")
	if out, err := load.CombinedOutput(); err != nil {
		t.Fatalf("seeding the store failed: %v\n%s", err, out)
	}

	info, err := c.Info(context.Background(), InfoOptions{})
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}

	if info.StoreID == "" {
		t.Error("StoreID is empty -- the JSON did not deserialise as expected")
	}
	if info.StorePath == "" {
		t.Error("StorePath is empty")
	}
	if len(info.Artifacts) == 0 {
		t.Fatal("no artifacts parsed from a seeded store")
	}

	images := info.Images()
	if len(images) == 0 {
		t.Error("no image artifacts found in a seeded store")
	}
	for _, a := range images {
		if a.Reference == "" {
			t.Errorf("artifact has no reference: %+v", a)
		}
		if !strings.HasPrefix(a.Digest, "sha256:") {
			t.Errorf("artifact %q digest = %q, want a sha256 digest (is --digests still supported?)",
				a.Reference, a.Digest)
		}
		if a.Size <= 0 {
			t.Errorf("artifact %q size = %d, want > 0", a.Reference, a.Size)
		}
		if !info.HasDigest(a.Digest) {
			t.Errorf("HasDigest(%q) = false for an artifact that is present", a.Digest)
		}
	}

	t.Logf("parsed %d artifacts (%d images) from store %s",
		len(info.Artifacts), len(images), info.StoreID)
}

// A fresh volume has no index. InfoIfExists must treat that as an empty
// inventory, because it is every deployment's first reconcile.
func TestIntegrationInfoIfExistsOnEmptyStore(t *testing.T) {
	c := newIntegrationClient(t, filepath.Join(t.TempDir(), "store"))

	info, err := c.InfoIfExists(context.Background(), InfoOptions{})
	if err != nil {
		t.Fatalf("InfoIfExists() on an empty store error = %v", err)
	}
	if len(info.Artifacts) != 0 {
		t.Errorf("len(Artifacts) = %d, want 0", len(info.Artifacts))
	}
}

// hauler must accept a manifest produced by internal/manifest's Scope. The
// Files kind is used because it is the only content type that resolves
// entirely from local disk, so this stays runnable without network access.
func TestIntegrationHaulerAcceptsScopedManifest(t *testing.T) {
	c := newIntegrationClient(t, filepath.Join(t.TempDir(), "store"))

	dir := t.TempDir()
	local := filepath.Join(dir, "payload.txt")
	if err := os.WriteFile(local, []byte("hauler-web integration payload\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Written in the same shape internal/manifest emits: original apiVersion,
	// kind, and metadata preserved, spec list filtered.
	scoped := filepath.Join(dir, "scoped.yaml")
	body := "apiVersion: content.hauler.cattle.io/v1\n" +
		"kind: Files\n" +
		"metadata:\n" +
		"  name: hauler-web-scoped\n" +
		"spec:\n" +
		"  files:\n" +
		"    - path: " + local + "\n"
	if err := os.WriteFile(scoped, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Sync(context.Background(), SyncOptions{Filenames: []string{scoped}}, nil); err != nil {
		t.Fatalf("hauler rejected a scoped manifest: %v", err)
	}

	info, err := c.Info(context.Background(), InfoOptions{})
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	var found bool
	for _, a := range info.Artifacts {
		if strings.Contains(a.Reference, "payload.txt") {
			found = true
		}
	}
	if !found {
		t.Errorf("synced file not present in the store: %+v", info.Artifacts)
	}
}
