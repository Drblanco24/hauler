//go:build e2e

package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/Drblanco24/hauler-web/internal/db"
	"github.com/Drblanco24/hauler-web/internal/hauler"
	"github.com/Drblanco24/hauler-web/internal/planner"
	"github.com/Drblanco24/hauler-web/internal/registryclient"
)

// End-to-end acceptance tests for the reconcile loop.
//
// These are the tests that can actually prove the product's central claim --
// that a second reconcile over unchanged inputs does no work. Every other test
// in the repository checks a decision in isolation; only this one runs the real
// binary against real registries and a real database and then checks that the
// second pass stayed still.
//
// The whole stack is in-process: go-containerregistry's registry package
// serves both the "upstream" and "destination" registries over loopback HTTP,
// which hauler reaches without any insecure flag because go-containerregistry
// selects http:// for 127.0.0.1. Nothing here pulls from the internet, which
// is the right property for a test of an airgap tool -- and a hard requirement
// in an environment whose egress policy blocks registry blob storage.
//
//	go test -tags=e2e ./internal/reconcile/ -v
//
// Requires HAULERWEB_TEST_DATABASE_URL and a hauler binary.

type harness struct {
	t        *testing.T
	db       *db.DB
	rec      *Reconciler
	upstream string // host:port
	dest     string // host:port
	repoDir  string // git working copy that acts as the manifest source
	source   db.Source
	destID   int64
}

func requireEnv(t *testing.T) (dsn, haulerBin string) {
	t.Helper()
	dsn = os.Getenv("HAULERWEB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set HAULERWEB_TEST_DATABASE_URL to run e2e tests")
	}
	haulerBin = os.Getenv("HAULERWEB_TEST_HAULER_BIN")
	if haulerBin == "" {
		haulerBin = "hauler"
	}
	resolved, err := exec.LookPath(haulerBin)
	if err != nil {
		t.Skipf("hauler binary %q not found: %v", haulerBin, err)
	}
	return dsn, resolved
}

func startRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dsn, haulerBin := requireEnv(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()

	database, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(database.Close)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if _, err := database.Pool().Exec(ctx, `
TRUNCATE sources, manifest_revisions, desired_items, images, image_digests,
         runs, run_items, targets, deliveries, archives, archive_contents,
         registry_inventory, jobs RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncating: %v", err)
	}

	h := &harness{t: t, db: database}
	h.upstream = startRegistry(t)
	h.dest = startRegistry(t)

	// The manifest repository.
	h.repoDir = filepath.Join(t.TempDir(), "manifests")
	if err := os.MkdirAll(h.repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, h.repoDir, "init", "-q", "-b", "main", h.repoDir)

	root := t.TempDir()
	h.rec = &Reconciler{
		DB: database,
		Hauler: &hauler.Client{
			Bin:       haulerBin,
			StoreDir:  filepath.Join(root, "store"),
			HaulerDir: filepath.Join(root, "haulerdir"),
			Timeout:   5 * time.Minute,
		},
		Registry:    &registryclient.Client{},
		WorkDir:     filepath.Join(root, "work"),
		Concurrency: 2,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// The destination target. plain_http and insecure are set explicitly:
	// hauler's copy path does not infer them from a loopback address the way
	// its pull path does.
	h.destID, err = database.UpsertTarget(ctx, db.Target{
		Name: "dest", Kind: "registry", Enabled: true,
		Config: map[string]any{
			"url": h.dest, "project": "airgap",
			"plain_http": true, "insecure": true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	return h
}

// pushUpstream publishes an image to the upstream registry and returns its
// digest.
func (h *harness) pushUpstream(repoTag string) string {
	h.t.Helper()
	ref := h.upstream + "/" + repoTag
	img, err := random.Image(1024, 1)
	if err != nil {
		h.t.Fatal(err)
	}
	pr, err := name.ParseReference(ref)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := remote.Write(pr, img); err != nil {
		h.t.Fatalf("pushing %s: %v", ref, err)
	}
	d, err := img.Digest()
	if err != nil {
		h.t.Fatal(err)
	}
	return d.String()
}

// commitManifest writes a manifest listing refs and commits it.
func (h *harness) commitManifest(msg string, repoTags ...string) {
	h.t.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: content.hauler.cattle.io/v1\nkind: Images\nmetadata:\n  name: e2e\nspec:\n  images:\n")
	for _, rt := range repoTags {
		b.WriteString("    - name: " + h.upstream + "/" + rt + "\n")
	}
	if err := os.WriteFile(filepath.Join(h.repoDir, "images.yaml"), []byte(b.String()), 0o644); err != nil {
		h.t.Fatal(err)
	}
	git(h.t, h.repoDir, "add", "-A")
	git(h.t, h.repoDir, "commit", "-qm", msg)
}

// registerSource creates the source row pointing at the manifest repo.
func (h *harness) registerSource() {
	h.t.Helper()
	ctx := context.Background()
	id, err := h.db.UpsertSource(ctx, db.Source{
		Name: "e2e", URL: h.repoDir, Branch: "main", PathGlob: "*.yaml",
		PollInterval: time.Minute, Enabled: true,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	src, err := h.db.GetSourceByName(ctx, "e2e")
	if err != nil {
		h.t.Fatal(err)
	}
	h.source = *src
	h.source.ID = id
}

// reconcile runs one pass, refreshing the source row first so LastCommitSHA is
// current -- the worker loop does the same.
func (h *harness) reconcile(force bool) *Result {
	h.t.Helper()
	ctx := context.Background()
	src, err := h.db.GetSourceByName(ctx, "e2e")
	if err != nil {
		h.t.Fatal(err)
	}
	res, err := h.rec.Once(ctx, Options{Source: *src, Trigger: "manual", ForceRepull: force})
	if err != nil {
		h.t.Fatalf("reconcile error = %v", err)
	}
	return res
}

// destCatalog lists repositories in the destination registry.
func (h *harness) destCatalog() []string {
	h.t.Helper()
	resp, err := http.Get("http://" + h.dest + "/v2/_catalog")
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatal(err)
	}
	return out.Repositories
}

func (h *harness) countRows(table string) int {
	h.t.Helper()
	var n int
	q := fmt.Sprintf("SELECT count(*) FROM %s", table)
	if err := h.db.Pool().QueryRow(context.Background(), q).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// TestReconcileMovesThenStaysStill is assertion 1-4, 6 and 7 of the plan's
// acceptance gate, in one flow because they are inherently sequential.
func TestReconcileMovesThenStaysStill(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	digest1 := h.pushUpstream("team/app:v1")
	h.commitManifest("initial", "team/app:v1")
	h.registerSource()

	// ---- first pass: it must actually move the image --------------------
	res := h.reconcile(false)
	if res.Status != "succeeded" {
		t.Fatalf("first run status = %q, want succeeded", res.Status)
	}
	if got := res.Counts[planner.ActionPull]; got != 1 {
		t.Fatalf("first run pulls = %d, want 1 (summary: %s)", got, res.Plan.Summary())
	}

	counts, err := h.db.CountRunItemsByAction(ctx, res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if counts["pull"] != 1 {
		t.Errorf("run_items pull rows = %d, want 1", counts["pull"])
	}

	// The image must exist in the destination registry, not merely be
	// recorded as delivered.
	repos := h.destCatalog()
	if len(repos) == 0 {
		t.Fatal("destination registry is empty after a reconcile that reported a pull")
	}
	t.Logf("destination repositories: %v", repos)

	if n := h.countRows("deliveries"); n == 0 {
		t.Error("no delivery rows recorded")
	}
	if n := h.countRows("image_digests"); n == 0 {
		t.Error("no digest history recorded")
	}

	var recorded string
	if err := h.db.Pool().QueryRow(ctx,
		`SELECT digest FROM image_digests ORDER BY id LIMIT 1`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != digest1 {
		t.Errorf("recorded digest = %q, want the upstream digest %q", recorded, digest1)
	}

	deliveriesAfterFirst := h.countRows("deliveries")

	// ---- second pass: the headline assertion ----------------------------
	// Nothing changed upstream or in git, so nothing may be pulled. A
	// regression here is the failure mode the whole system exists to avoid,
	// and it is invisible without this check: a re-pull still "succeeds".
	res2 := h.reconcile(false)
	if res2.Status != "succeeded" {
		t.Fatalf("second run status = %q, want succeeded", res2.Status)
	}
	if got := res2.Counts[planner.ActionPull]; got != 0 {
		t.Errorf("second run pulled %d images, want 0 (summary: %s)", got, res2.Plan.Summary())
	}
	if got := res2.Counts[planner.ActionSkipPresent]; got != 1 {
		t.Errorf("second run skips = %d, want 1 (summary: %s)", got, res2.Plan.Summary())
	}
	if got := h.countRows("deliveries"); got != deliveriesAfterFirst {
		t.Errorf("delivery rows changed on a no-op run: %d then %d", deliveriesAfterFirst, got)
	}

	// ---- third pass: a moved tag must be noticed ------------------------
	digest2 := h.pushUpstream("team/app:v1") // same tag, new content
	if digest1 == digest2 {
		t.Fatal("test setup produced identical digests")
	}

	res3 := h.reconcile(false)
	if got := res3.Counts[planner.ActionPull]; got != 1 {
		t.Errorf("after a tag move, pulls = %d, want 1 (summary: %s)", got, res3.Plan.Summary())
	}

	var digestRows int
	if err := h.db.Pool().QueryRow(ctx, `SELECT count(*) FROM image_digests`).Scan(&digestRows); err != nil {
		t.Fatal(err)
	}
	if digestRows != 2 {
		t.Errorf("image_digests rows = %d, want 2 (both digests of the moved tag)", digestRows)
	}
}

// A forced re-pull must override an otherwise-complete state, or an operator
// who suspects the store is wrong has no way to correct it.
func TestForceRepullOverridesSkip(t *testing.T) {
	h := newHarness(t)

	h.pushUpstream("team/app:v1")
	h.commitManifest("initial", "team/app:v1")
	h.registerSource()

	h.reconcile(false)

	res := h.reconcile(true)
	if got := res.Counts[planner.ActionPull]; got != 1 {
		t.Errorf("forced run pulls = %d, want 1 (summary: %s)", got, res.Plan.Summary())
	}
}

// Adding an image to the manifest must pull only the new one.
func TestAddingAnImagePullsOnlyTheNewOne(t *testing.T) {
	h := newHarness(t)

	h.pushUpstream("team/app:v1")
	h.commitManifest("initial", "team/app:v1")
	h.registerSource()
	h.reconcile(false)

	h.pushUpstream("team/other:v1")
	h.commitManifest("add other", "team/app:v1", "team/other:v1")

	res := h.reconcile(false)
	if got := res.Counts[planner.ActionPull]; got != 1 {
		t.Errorf("pulls = %d, want 1 -- only the new image should be fetched (summary: %s)",
			got, res.Plan.Summary())
	}
	if got := res.Counts[planner.ActionSkipPresent]; got != 1 {
		t.Errorf("skips = %d, want 1 (summary: %s)", got, res.Plan.Summary())
	}
}

// An unresolvable image must not stop the rest of the manifest, and the run
// must not report a clean success.
func TestUnresolvableImageDegradesToPartial(t *testing.T) {
	h := newHarness(t)

	h.pushUpstream("team/app:v1")
	// team/missing:v1 was never pushed.
	h.commitManifest("initial", "team/app:v1", "team/missing:v1")
	h.registerSource()

	res := h.reconcile(false)
	if res.Status != "partial" {
		t.Errorf("status = %q, want partial -- an unresolved image must not read as success", res.Status)
	}
	if got := res.Counts[planner.ActionPull]; got != 1 {
		t.Errorf("pulls = %d, want 1 -- the healthy image should still be fetched", got)
	}
	if got := res.Counts[planner.ActionError]; got != 1 {
		t.Errorf("errors = %d, want 1", got)
	}
}

// SkipUnchanged short-circuits before any registry or store work when the
// commit has not moved. This is what keeps a one-minute poll cheap.
func TestSkipUnchangedShortCircuits(t *testing.T) {
	h := newHarness(t)

	h.pushUpstream("team/app:v1")
	h.commitManifest("initial", "team/app:v1")
	h.registerSource()
	h.reconcile(false)

	ctx := context.Background()
	src, err := h.db.GetSourceByName(ctx, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	runsBefore := h.countRows("runs")

	res, err := h.rec.Once(ctx, Options{Source: *src, Trigger: "schedule", SkipUnchanged: true})
	if err != nil {
		t.Fatalf("Once() error = %v", err)
	}
	if !res.Skipped {
		t.Error("Skipped = false, want true for an unchanged commit")
	}
	if got := h.countRows("runs"); got != runsBefore {
		t.Errorf("a skipped poll created a run row: %d then %d", runsBefore, got)
	}
}

// Two reconcilers must never write one store concurrently.
func TestConcurrentReconcileIsRefused(t *testing.T) {
	h := newHarness(t)

	h.pushUpstream("team/app:v1")
	h.commitManifest("initial", "team/app:v1")
	h.registerSource()

	ctx := context.Background()
	locked, release, err := h.db.TryLockStore(ctx, storeKey(h.rec.Hauler.StoreDir))
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("could not take the store lock for the test")
	}
	defer release()

	src, _ := h.db.GetSourceByName(ctx, "e2e")
	_, err = h.rec.Once(ctx, Options{Source: *src, Trigger: "manual"})
	if err == nil {
		t.Fatal("Once() succeeded while the store was locked by another holder")
	}
	if !strings.Contains(err.Error(), "another worker") {
		t.Errorf("error = %v, want it to say the store is busy", err)
	}
}
