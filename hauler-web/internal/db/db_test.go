package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// These tests need a real Postgres. They are skipped when
// HAULERWEB_TEST_DATABASE_URL is unset so `go test ./...` stays runnable
// anywhere, and CI sets it against a service container.
//
// They deliberately exercise real SQL rather than a mock: the behaviour that
// matters here -- ON CONFLICT semantics on the deliveries unique constraint,
// advisory lock contention, interval arithmetic -- lives in Postgres, and a
// mock would assert only that this file's own strings are unchanged.

func testDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("HAULERWEB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set HAULERWEB_TEST_DATABASE_URL to run database tests")
	}

	ctx := context.Background()
	d, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(d.Close)

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	truncateAll(t, d)
	return d
}

func truncateAll(t *testing.T, d *DB) {
	t.Helper()
	const q = `
TRUNCATE sources, manifest_revisions, desired_items, images, image_digests,
         runs, run_items, targets, deliveries, archives, archive_contents,
         registry_inventory, users, sessions, api_tokens, audit_events, jobs
RESTART IDENTITY CASCADE`
	if _, err := d.pool.Exec(context.Background(), q); err != nil {
		t.Fatalf("truncating: %v", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	d := testDB(t)
	// Migrate already ran in testDB; running again must be a clean no-op, as
	// it will be on every pod restart and every helm upgrade.
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}
}

func TestSourceRoundTrip(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	id, err := d.UpsertSource(ctx, Source{
		Name: "prod", URL: "https://git.example/m.git", Branch: "main",
		PathGlob: "images/*.yaml", PollInterval: 5 * time.Minute, Enabled: true,
	})
	if err != nil {
		t.Fatalf("UpsertSource() error = %v", err)
	}

	// Upserting again must update in place, not create a duplicate.
	id2, err := d.UpsertSource(ctx, Source{
		Name: "prod", URL: "https://git.example/m.git", Branch: "release",
		PathGlob: "images/*.yaml", PollInterval: time.Minute, Enabled: true,
	})
	if err != nil {
		t.Fatalf("second UpsertSource() error = %v", err)
	}
	if id != id2 {
		t.Errorf("upsert created a new row: %d then %d", id, id2)
	}

	got, err := d.GetSourceByName(ctx, "prod")
	if err != nil {
		t.Fatalf("GetSourceByName() error = %v", err)
	}
	if got.Branch != "release" {
		t.Errorf("Branch = %q, want %q", got.Branch, "release")
	}
	if got.PollInterval != time.Minute {
		t.Errorf("PollInterval = %s, want 1m", got.PollInterval)
	}

	if _, err := d.GetSourceByName(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSourceByName(absent) error = %v, want ErrNotFound", err)
	}

	if err := d.MarkSourcePolled(ctx, id, "abc123", nil); err != nil {
		t.Fatalf("MarkSourcePolled() error = %v", err)
	}
	got, _ = d.GetSourceByName(ctx, "prod")
	if got.LastCommitSHA != "abc123" {
		t.Errorf("LastCommitSHA = %q, want %q", got.LastCommitSHA, "abc123")
	}

	// An empty sha must not clobber the recorded one -- a failed poll should
	// leave the last known good commit in place.
	if err := d.MarkSourcePolled(ctx, id, "", errors.New("dial timeout")); err != nil {
		t.Fatalf("MarkSourcePolled(err) error = %v", err)
	}
	got, _ = d.GetSourceByName(ctx, "prod")
	if got.LastCommitSHA != "abc123" {
		t.Errorf("LastCommitSHA = %q after a failed poll, want it preserved", got.LastCommitSHA)
	}
}

func TestRecordRevisionIsRetryable(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	srcID, err := d.UpsertSource(ctx, Source{Name: "s", URL: "u", Branch: "main", PathGlob: "*", PollInterval: time.Minute, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	items := []DesiredItem{
		{Kind: "image", Ref: "alpine:3.20", Platform: "linux/amd64", RawYAML: "- name: alpine:3.20\n", SpecHash: "h1"},
		{Kind: "image", Ref: "busybox:stable", RawYAML: "- name: busybox:stable\n", SpecHash: "h2", Position: 1},
	}

	revID, err := d.RecordRevision(ctx, srcID, "images.yaml", "sha1", "csum", items)
	if err != nil {
		t.Fatalf("RecordRevision() error = %v", err)
	}

	// A retried run must not double-insert the entries.
	revID2, err := d.RecordRevision(ctx, srcID, "images.yaml", "sha1", "csum", items)
	if err != nil {
		t.Fatalf("second RecordRevision() error = %v", err)
	}
	if revID != revID2 {
		t.Errorf("revision id changed on retry: %d then %d", revID, revID2)
	}

	var n int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM desired_items WHERE manifest_revision_id = $1`, revID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("desired_items count = %d after a retry, want 2", n)
	}
}

func TestImageAndDigestHistory(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	id, err := d.UpsertImage(ctx, "alpine:3.20", "index.docker.io/library/alpine", "3.20", "linux/amd64")
	if err != nil {
		t.Fatalf("UpsertImage() error = %v", err)
	}

	// Same ref and platform is the same image.
	again, err := d.UpsertImage(ctx, "alpine:3.20", "index.docker.io/library/alpine", "3.20", "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if id != again {
		t.Errorf("UpsertImage created a duplicate: %d then %d", id, again)
	}

	// A different platform is a different image.
	arm, err := d.UpsertImage(ctx, "alpine:3.20", "index.docker.io/library/alpine", "3.20", "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	if arm == id {
		t.Error("UpsertImage collapsed two platforms into one row")
	}

	if err := d.RecordDigest(ctx, id, "sha256:aaa", nil); err != nil {
		t.Fatalf("RecordDigest() error = %v", err)
	}
	// Recording the same digest twice must not error.
	if err := d.RecordDigest(ctx, id, "sha256:aaa", nil); err != nil {
		t.Fatalf("duplicate RecordDigest() error = %v", err)
	}
	if err := d.RecordDigest(ctx, id, "sha256:bbb", nil); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM image_digests WHERE image_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("digest history rows = %d, want 2", n)
	}

	if err := d.SetImageSpecHash(ctx, id, "h1"); err != nil {
		t.Fatal(err)
	}
	tracked, err := d.TrackedImages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracked) != 2 {
		t.Fatalf("TrackedImages() = %d rows, want 2", len(tracked))
	}
	var found bool
	for _, im := range tracked {
		if im.ID == id {
			found = true
			if im.LastSpecHash != "h1" {
				t.Errorf("LastSpecHash = %q, want %q", im.LastSpecHash, "h1")
			}
		}
	}
	if !found {
		t.Error("TrackedImages() did not include the amd64 image")
	}
}

// The deliveries unique constraint is the dedupe primitive; this test pins its
// exact behaviour.
func TestDeliveriesDedupe(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	harbor, err := d.UpsertTarget(ctx, Target{Name: "harbor", Kind: "registry", Enabled: true})
	if err != nil {
		t.Fatalf("UpsertTarget() error = %v", err)
	}
	backup, err := d.UpsertTarget(ctx, Target{Name: "backup", Kind: "registry", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	imgID, err := d.UpsertImage(ctx, "alpine:3.20", "docker.io/library/alpine", "3.20", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := d.RecordDelivery(ctx, nil, harbor, imgID, "sha256:aaa", "harbor.example/airgap/alpine:3.20", nil); err != nil {
		t.Fatalf("RecordDelivery() error = %v", err)
	}
	// Re-delivering the same digest must update, not violate the constraint.
	if err := d.RecordDelivery(ctx, nil, harbor, imgID, "sha256:aaa", "harbor.example/airgap/alpine:3.20", nil); err != nil {
		t.Fatalf("repeat RecordDelivery() error = %v", err)
	}

	set, err := d.LoadDelivered(ctx)
	if err != nil {
		t.Fatalf("LoadDelivered() error = %v", err)
	}
	if !set.Has(harbor, "sha256:aaa") {
		t.Error("Has(harbor, aaa) = false, want true")
	}
	// Delivery to one target says nothing about another.
	if set.Has(backup, "sha256:aaa") {
		t.Error("Has(backup, aaa) = true -- delivery leaked across targets")
	}
	// Nor does one digest say anything about another.
	if set.Has(harbor, "sha256:bbb") {
		t.Error("Has(harbor, bbb) = true -- delivery leaked across digests")
	}
	if set.Has(harbor, "") {
		t.Error(`Has(harbor, "") = true -- an empty digest must never count as delivered`)
	}

	// A failed delivery must not count as done, or the image would never be
	// retried.
	if err := d.RecordDelivery(ctx, nil, backup, imgID, "sha256:aaa", "", errors.New("500 from registry")); err != nil {
		t.Fatal(err)
	}
	set, _ = d.LoadDelivered(ctx)
	if set.Has(backup, "sha256:aaa") {
		t.Error("a failed delivery was loaded as succeeded")
	}

	// ...and a later success must flip it.
	if err := d.RecordDelivery(ctx, nil, backup, imgID, "sha256:aaa", "backup/alpine:3.20", nil); err != nil {
		t.Fatal(err)
	}
	set, _ = d.LoadDelivered(ctx)
	if !set.Has(backup, "sha256:aaa") {
		t.Error("a retried delivery did not become succeeded")
	}
}

func TestRunLifecycle(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	srcID, err := d.UpsertSource(ctx, Source{Name: "s", URL: "u", Branch: "main", PathGlob: "*", PollInterval: time.Minute, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	runID, err := d.CreateRun(ctx, &srcID, "abc123", "manual", "store-uuid", "v2.0.0", false)
	if err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}

	r, err := d.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "running" {
		t.Errorf("Status = %q, want %q", r.Status, "running")
	}
	if r.HaulerVersion != "v2.0.0" {
		t.Errorf("HaulerVersion = %q", r.HaulerVersion)
	}

	for _, it := range []RunItem{
		{RunID: runID, Ref: "alpine:3.20", Action: "pull", Status: "succeeded", Digest: "sha256:aaa", Duration: 2 * time.Second},
		{RunID: runID, Ref: "busybox:stable", Action: "skip_present", Status: "skipped"},
		{RunID: runID, Ref: "broken:1", Action: "error", Status: "failed", Err: errors.New("unauthorized")},
	} {
		if err := d.InsertRunItem(ctx, it); err != nil {
			t.Fatalf("InsertRunItem(%s) error = %v", it.Ref, err)
		}
	}

	counts, err := d.CountRunItemsByAction(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for action, want := range map[string]int{"pull": 1, "skip_present": 1, "error": 1} {
		if counts[action] != want {
			t.Errorf("counts[%s] = %d, want %d", action, counts[action], want)
		}
	}

	if err := d.FinishRun(ctx, runID, "partial", map[string]any{"bytes": 1234}, "tail", nil); err != nil {
		t.Fatalf("FinishRun() error = %v", err)
	}
	r, _ = d.GetRun(ctx, runID)
	if r.Status != "partial" {
		t.Errorf("Status = %q, want %q", r.Status, "partial")
	}
}

// A killed worker must not leave a run stuck in 'running' forever.
func TestFailStaleRuns(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	runID, err := d.CreateRun(ctx, nil, "", "manual", "", "", false)
	if err != nil {
		t.Fatal(err)
	}

	// Too recent to be considered abandoned.
	n, err := d.FailStaleRuns(ctx, time.Hour)
	if err != nil {
		t.Fatalf("FailStaleRuns() error = %v", err)
	}
	if n != 0 {
		t.Errorf("FailStaleRuns(1h) swept %d fresh runs, want 0", n)
	}

	// Backdate it to simulate a crash.
	if _, err := d.pool.Exec(ctx, `UPDATE runs SET started_at = now() - interval '2 hours' WHERE id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	n, err = d.FailStaleRuns(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("FailStaleRuns(1h) = %d, want 1", n)
	}

	r, _ := d.GetRun(ctx, runID)
	if r.Status != "failed" {
		t.Errorf("Status = %q, want %q", r.Status, "failed")
	}
}

// Two processes must never write one store concurrently.
func TestAdvisoryStoreLockExcludes(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	locked, release, err := d.TryLockStore(ctx, 42)
	if err != nil {
		t.Fatalf("TryLockStore() error = %v", err)
	}
	if !locked {
		t.Fatal("first TryLockStore() = false, want true")
	}

	// A second attempt on the same key must fail while the first is held.
	locked2, release2, err := d.TryLockStore(ctx, 42)
	if err != nil {
		t.Fatalf("second TryLockStore() error = %v", err)
	}
	if locked2 {
		release2()
		t.Fatal("second TryLockStore() = true -- two writers could corrupt the store")
	}

	// A different store is unaffected.
	locked3, release3, err := d.TryLockStore(ctx, 43)
	if err != nil {
		t.Fatal(err)
	}
	if !locked3 {
		t.Error("TryLockStore(43) = false -- an unrelated store was blocked")
	} else {
		release3()
	}

	release()

	// After release the lock is available again.
	locked4, release4, err := d.TryLockStore(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !locked4 {
		t.Fatal("TryLockStore() after release = false, want true")
	}
	release4()
}

func TestTargets(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	days := 30
	if _, err := d.UpsertTarget(ctx, Target{
		Name: "cold", Kind: "archive", Enabled: true, RetentionDays: &days,
		Config: map[string]any{"s3_bucket": "hauls", "chunk_size": "2G"},
	}); err != nil {
		t.Fatalf("UpsertTarget() error = %v", err)
	}
	if _, err := d.UpsertTarget(ctx, Target{Name: "off", Kind: "registry", Enabled: false}); err != nil {
		t.Fatal(err)
	}

	targets, err := d.ListEnabledTargets(ctx)
	if err != nil {
		t.Fatalf("ListEnabledTargets() error = %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("ListEnabledTargets() = %d, want 1 (disabled targets must be excluded)", len(targets))
	}
	got := targets[0]
	if got.Name != "cold" || got.Kind != "archive" {
		t.Errorf("target = %+v", got)
	}
	if got.RetentionDays == nil || *got.RetentionDays != 30 {
		t.Errorf("RetentionDays = %v, want 30", got.RetentionDays)
	}
	if got.Config["s3_bucket"] != "hauls" {
		t.Errorf("Config = %v, want the JSONB to round-trip", got.Config)
	}
}

// The schema's CHECK constraints are load-bearing documentation; verify they
// actually reject bad values rather than being decorative.
func TestCheckConstraintsReject(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	tests := []struct {
		name string
		sql  string
		args []any
	}{
		{"unknown target kind", `INSERT INTO targets (name, kind) VALUES ('x', 'ftp')`, nil},
		{"unknown run trigger", `INSERT INTO runs (trigger) VALUES ('telepathy')`, nil},
		{"unknown run status", `INSERT INTO runs (trigger, status) VALUES ('manual', 'vibing')`, nil},
		{"negative retention", `INSERT INTO targets (name, kind, retention_days) VALUES ('y', 'archive', -1)`, nil},
		{"unknown desired kind", `INSERT INTO manifest_revisions (source_id, path, commit_sha, content_sha256) VALUES (NULL, 'p', 'c', 'h')`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := d.pool.Exec(ctx, tt.sql, tt.args...); err == nil {
				t.Fatalf("expected the database to reject: %s", tt.sql)
			}
		})
	}
}

func TestDeleteImageCascades(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	target, err := d.UpsertTarget(ctx, Target{Name: "t", Kind: "registry", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	imgID, err := d.UpsertImage(ctx, "gone:1", "docker.io/library/gone", "1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RecordDigest(ctx, imgID, "sha256:aaa", nil); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordDelivery(ctx, nil, target, imgID, "sha256:aaa", "", nil); err != nil {
		t.Fatal(err)
	}

	if err := d.DeleteImage(ctx, imgID); err != nil {
		t.Fatalf("DeleteImage() error = %v", err)
	}

	for _, table := range []string{"image_digests", "deliveries"} {
		var n int
		q := fmt.Sprintf(`SELECT count(*) FROM %s WHERE image_id = $1`, table)
		if err := d.pool.QueryRow(ctx, q, imgID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s still has %d rows after the image was deleted", table, n)
		}
	}
}
