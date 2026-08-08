package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Sources
// ---------------------------------------------------------------------------

// Source is a git repository of manifests.
type Source struct {
	ID            int64
	Name          string
	URL           string
	Branch        string
	PathGlob      string
	PollInterval  time.Duration
	PruneOrphans  bool
	Enabled       bool
	LastCommitSHA string
}

// UpsertSource creates or updates a source by name and returns its id.
func (d *DB) UpsertSource(ctx context.Context, s Source) (int64, error) {
	const q = `
INSERT INTO sources (name, url, branch, path_glob, poll_interval, prune_orphans, enabled)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (name) DO UPDATE SET
    url = EXCLUDED.url,
    branch = EXCLUDED.branch,
    path_glob = EXCLUDED.path_glob,
    poll_interval = EXCLUDED.poll_interval,
    prune_orphans = EXCLUDED.prune_orphans,
    enabled = EXCLUDED.enabled,
    updated_at = now()
RETURNING id`
	var id int64
	err := d.pool.QueryRow(ctx, q, s.Name, s.URL, s.Branch, s.PathGlob,
		s.PollInterval, s.PruneOrphans, s.Enabled).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upserting source %q: %w", s.Name, err)
	}
	return id, nil
}

// GetSourceByName looks up a source.
func (d *DB) GetSourceByName(ctx context.Context, name string) (*Source, error) {
	const q = `
SELECT id, name, url, branch, path_glob, poll_interval, prune_orphans, enabled,
       coalesce(last_commit_sha, '')
FROM sources WHERE name = $1`
	var s Source
	err := d.pool.QueryRow(ctx, q, name).Scan(&s.ID, &s.Name, &s.URL, &s.Branch,
		&s.PathGlob, &s.PollInterval, &s.PruneOrphans, &s.Enabled, &s.LastCommitSHA)
	if noRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ListEnabledSources returns every source the reconciler should poll.
func (d *DB) ListEnabledSources(ctx context.Context) ([]Source, error) {
	const q = `
SELECT id, name, url, branch, path_glob, poll_interval, prune_orphans, enabled,
       coalesce(last_commit_sha, '')
FROM sources WHERE enabled ORDER BY id`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Source
	for rows.Next() {
		var s Source
		if err := rows.Scan(&s.ID, &s.Name, &s.URL, &s.Branch, &s.PathGlob,
			&s.PollInterval, &s.PruneOrphans, &s.Enabled, &s.LastCommitSHA); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// MarkSourcePolled records the commit a source was last reconciled at.
func (d *DB) MarkSourcePolled(ctx context.Context, sourceID int64, commitSHA string, pollErr error) error {
	var errText *string
	if pollErr != nil {
		s := pollErr.Error()
		errText = &s
	}
	const q = `
UPDATE sources
   SET last_polled_at = now(),
       last_commit_sha = coalesce(nullif($2, ''), last_commit_sha),
       last_error = $3
 WHERE id = $1`
	_, err := d.pool.Exec(ctx, q, sourceID, commitSHA, errText)
	return err
}

// ---------------------------------------------------------------------------
// Desired state
// ---------------------------------------------------------------------------

// DesiredItem is one flattened manifest entry.
type DesiredItem struct {
	Kind     string
	Ref      string
	Platform string
	RawYAML  string
	SpecHash string
	Position int
}

// RecordRevision stores a manifest file at a commit along with its entries,
// and returns the revision id. Re-recording the same (source, path, commit) is
// a no-op that returns the existing id, so a reconcile can be retried safely.
func (d *DB) RecordRevision(ctx context.Context, sourceID int64, path, commitSHA, contentSHA string, items []DesiredItem) (int64, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	const insRev = `
INSERT INTO manifest_revisions (source_id, path, commit_sha, content_sha256)
VALUES ($1, $2, $3, $4)
ON CONFLICT (source_id, path, commit_sha) DO UPDATE SET parsed_at = now()
RETURNING id`
	var revID int64
	if err := tx.QueryRow(ctx, insRev, sourceID, path, commitSHA, contentSHA).Scan(&revID); err != nil {
		return 0, fmt.Errorf("recording revision %s@%s: %w", path, commitSHA, err)
	}

	// Replace the entries wholesale. A revision is immutable in principle,
	// but a retried run must not double-insert.
	if _, err := tx.Exec(ctx, `DELETE FROM desired_items WHERE manifest_revision_id = $1`, revID); err != nil {
		return 0, err
	}

	for _, it := range items {
		const insItem = `
INSERT INTO desired_items (manifest_revision_id, kind, ref, platform, raw_yaml, spec_hash, position)
VALUES ($1, $2, $3, $4, $5, $6, $7)`
		if _, err := tx.Exec(ctx, insItem, revID, it.Kind, it.Ref, it.Platform,
			it.RawYAML, it.SpecHash, it.Position); err != nil {
			return 0, fmt.Errorf("recording desired item %q: %w", it.Ref, err)
		}
	}

	return revID, tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// Images
// ---------------------------------------------------------------------------

// Image is a tracked image identity.
type Image struct {
	ID           int64
	Ref          string
	Repository   string
	Tag          string
	Platform     string
	LastSpecHash string
}

// UpsertImage records that a reference is currently desired and returns its id.
func (d *DB) UpsertImage(ctx context.Context, ref, repository, tag, platform string) (int64, error) {
	const q = `
INSERT INTO images (ref, repository, tag, platform, last_desired_at)
VALUES ($1, $2, nullif($3, ''), $4, now())
ON CONFLICT (ref, platform) DO UPDATE SET
    repository = EXCLUDED.repository,
    tag = EXCLUDED.tag,
    last_desired_at = now()
RETURNING id`
	var id int64
	if err := d.pool.QueryRow(ctx, q, ref, repository, tag, platform).Scan(&id); err != nil {
		return 0, fmt.Errorf("upserting image %q: %w", ref, err)
	}
	return id, nil
}

// SetImageSpecHash records the spec hash an image was last successfully
// processed at.
//
// This is written only after a pull succeeds. If it were written at planning
// time, a failed pull would look "already handled" on the next run and the
// image would never be fetched -- the exact silent-skip failure this system
// must not have.
func (d *DB) SetImageSpecHash(ctx context.Context, imageID int64, specHash string) error {
	_, err := d.pool.Exec(ctx, `UPDATE images SET last_spec_hash = $2 WHERE id = $1`, imageID, specHash)
	return err
}

// RecordDigest appends to an image's digest history. Repeat digests are
// ignored, so the row's resolved_at marks first observation.
func (d *DB) RecordDigest(ctx context.Context, imageID int64, digest string, runID *int64) error {
	const q = `
INSERT INTO image_digests (image_id, digest, resolved_by_run_id)
VALUES ($1, $2, $3)
ON CONFLICT (image_id, digest) DO NOTHING`
	_, err := d.pool.Exec(ctx, q, imageID, digest, runID)
	return err
}

// TrackedImages returns every image the database knows about, with the spec
// hash it was last processed at. This feeds the planner's KnownSpecHash and
// its orphan detection.
func (d *DB) TrackedImages(ctx context.Context) ([]Image, error) {
	const q = `
SELECT id, ref, repository, coalesce(tag, ''), platform, coalesce(last_spec_hash, '')
FROM images ORDER BY ref, platform`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Image
	for rows.Next() {
		var im Image
		if err := rows.Scan(&im.ID, &im.Ref, &im.Repository, &im.Tag, &im.Platform, &im.LastSpecHash); err != nil {
			return nil, err
		}
		out = append(out, im)
	}
	return out, rows.Err()
}

// DeleteImage removes a pruned image and, by cascade, its digests and
// deliveries.
func (d *DB) DeleteImage(ctx context.Context, imageID int64) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM images WHERE id = $1`, imageID)
	return err
}

// ---------------------------------------------------------------------------
// Targets and deliveries
// ---------------------------------------------------------------------------

// Target is a delivery destination.
type Target struct {
	ID            int64
	Name          string
	Kind          string
	Config        map[string]any
	RetentionDays *int
	Enabled       bool
}

// UpsertTarget creates or updates a target by name.
func (d *DB) UpsertTarget(ctx context.Context, t Target) (int64, error) {
	const q = `
INSERT INTO targets (name, kind, config, retention_days, enabled)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (name) DO UPDATE SET
    kind = EXCLUDED.kind,
    config = EXCLUDED.config,
    retention_days = EXCLUDED.retention_days,
    enabled = EXCLUDED.enabled,
    updated_at = now()
RETURNING id`
	cfg := t.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	var id int64
	if err := d.pool.QueryRow(ctx, q, t.Name, t.Kind, cfg, t.RetentionDays, t.Enabled).Scan(&id); err != nil {
		return 0, fmt.Errorf("upserting target %q: %w", t.Name, err)
	}
	return id, nil
}

// ListEnabledTargets returns the destinations a run must satisfy.
func (d *DB) ListEnabledTargets(ctx context.Context) ([]Target, error) {
	const q = `
SELECT id, name, kind, config, retention_days, enabled
FROM targets WHERE enabled ORDER BY id`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Target
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.ID, &t.Name, &t.Kind, &t.Config, &t.RetentionDays, &t.Enabled); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeliveredSet is the set of (target, digest) pairs already delivered. The key
// is "targetID|digest".
type DeliveredSet map[string]bool

// Has reports whether a digest has reached a target.
func (s DeliveredSet) Has(targetID int64, digest string) bool {
	if digest == "" {
		return false
	}
	return s[fmt.Sprintf("%d|%s", targetID, digest)]
}

// LoadDelivered reads every successful delivery into memory.
//
// Loading the whole set in one query, rather than asking per image, keeps
// planning to a fixed number of round trips no matter how many images a
// manifest declares. Only successful rows are loaded: a failed delivery must
// be retried, not treated as done.
func (d *DB) LoadDelivered(ctx context.Context) (DeliveredSet, error) {
	const q = `SELECT target_id, digest FROM deliveries WHERE status = 'succeeded'`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := DeliveredSet{}
	for rows.Next() {
		var targetID int64
		var digest string
		if err := rows.Scan(&targetID, &digest); err != nil {
			return nil, err
		}
		out[fmt.Sprintf("%d|%s", targetID, digest)] = true
	}
	return out, rows.Err()
}

// RecordDelivery marks a digest as delivered to a target. Re-delivering the
// same digest updates the existing row rather than failing on the unique
// constraint that makes dedupe work.
func (d *DB) RecordDelivery(ctx context.Context, runID *int64, targetID, imageID int64, digest, targetRef string, deliveryErr error) error {
	status := "succeeded"
	var errText *string
	if deliveryErr != nil {
		status = "failed"
		s := deliveryErr.Error()
		errText = &s
	}
	const q = `
INSERT INTO deliveries (run_id, target_id, image_id, digest, target_ref, status, error)
VALUES ($1, $2, $3, $4, nullif($5, ''), $6, $7)
ON CONFLICT (target_id, image_id, digest) DO UPDATE SET
    run_id = EXCLUDED.run_id,
    target_ref = EXCLUDED.target_ref,
    status = EXCLUDED.status,
    error = EXCLUDED.error,
    delivered_at = now()`
	_, err := d.pool.Exec(ctx, q, runID, targetID, imageID, digest, targetRef, status, errText)
	return err
}

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

// Run is one reconcile execution.
type Run struct {
	ID            int64
	SourceID      *int64
	CommitSHA     string
	Trigger       string
	Status        string
	HaulerVersion string
}

// CreateRun opens a run in the running state and returns its id.
func (d *DB) CreateRun(ctx context.Context, sourceID *int64, commitSHA, trigger, storeID, haulerVersion string, forceRepull bool) (int64, error) {
	const q = `
INSERT INTO runs (source_id, commit_sha, trigger, status, force_repull, store_id, hauler_version, started_at)
VALUES ($1, nullif($2, ''), $3, 'running', $4, nullif($5, ''), nullif($6, ''), now())
RETURNING id`
	var id int64
	err := d.pool.QueryRow(ctx, q, sourceID, commitSHA, trigger, forceRepull, storeID, haulerVersion).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("creating run: %w", err)
	}
	return id, nil
}

// FinishRun closes a run.
func (d *DB) FinishRun(ctx context.Context, runID int64, status string, stats map[string]any, logTail string, runErr error) error {
	var errText *string
	if runErr != nil {
		s := runErr.Error()
		errText = &s
	}
	if stats == nil {
		stats = map[string]any{}
	}
	const q = `
UPDATE runs SET status = $2, stats = $3, log_tail = nullif($4, ''), error = $5, finished_at = now()
WHERE id = $1`
	_, err := d.pool.Exec(ctx, q, runID, status, stats, logTail, errText)
	return err
}

// FailStaleRuns marks runs left in 'running' by a crashed worker as failed.
//
// Without this a killed worker leaves a row that never resolves, and any logic
// keyed on "is a run in flight" blocks forever. Called at worker startup.
func (d *DB) FailStaleRuns(ctx context.Context, olderThan time.Duration) (int64, error) {
	const q = `
UPDATE runs
   SET status = 'failed',
       error = 'run was interrupted; the worker restarted without finishing it',
       finished_at = now()
 WHERE status = 'running'
   AND started_at < now() - $1::interval`
	tag, err := d.pool.Exec(ctx, q, olderThan)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RunItem is a per-image outcome within a run.
type RunItem struct {
	RunID    int64
	ImageID  *int64
	Ref      string
	Platform string
	Action   string
	Status   string
	Reason   string
	Digest   string
	Size     *int64
	Layers   *int
	Duration time.Duration
	Err      error
}

// InsertRunItem records one image's outcome.
func (d *DB) InsertRunItem(ctx context.Context, it RunItem) error {
	var errText *string
	if it.Err != nil {
		s := it.Err.Error()
		errText = &s
	}
	var durMS *int64
	if it.Duration > 0 {
		ms := it.Duration.Milliseconds()
		durMS = &ms
	}
	const q = `
INSERT INTO run_items (run_id, image_id, ref, platform, action, status, reason, digest,
                       size_bytes, layers, duration_ms, error, started_at, finished_at)
VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, ''), $9, $10, $11, $12, now(), now())`
	_, err := d.pool.Exec(ctx, q, it.RunID, it.ImageID, it.Ref, it.Platform, it.Action,
		it.Status, it.Reason, it.Digest, it.Size, it.Layers, durMS, errText)
	if err != nil {
		return fmt.Errorf("recording run item %q: %w", it.Ref, err)
	}
	return nil
}

// CountRunItemsByAction summarises a run. Used by the e2e assertions and the
// run detail view.
func (d *DB) CountRunItemsByAction(ctx context.Context, runID int64) (map[string]int, error) {
	const q = `SELECT action, count(*) FROM run_items WHERE run_id = $1 GROUP BY action`
	rows, err := d.pool.Query(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var action string
		var n int
		if err := rows.Scan(&action, &n); err != nil {
			return nil, err
		}
		out[action] = n
	}
	return out, rows.Err()
}

// GetRun reads a run's current state.
func (d *DB) GetRun(ctx context.Context, runID int64) (*Run, error) {
	const q = `
SELECT id, source_id, coalesce(commit_sha, ''), trigger, status, coalesce(hauler_version, '')
FROM runs WHERE id = $1`
	var r Run
	err := d.pool.QueryRow(ctx, q, runID).Scan(&r.ID, &r.SourceID, &r.CommitSHA,
		&r.Trigger, &r.Status, &r.HaulerVersion)
	if noRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ---------------------------------------------------------------------------
// Store locking
// ---------------------------------------------------------------------------

// storeLockNamespace keeps hauler-web's advisory locks from colliding with any
// other application sharing the database.
const storeLockNamespace = 0x48415530 // "HAU0"

// TryLockStore takes a session-scoped advisory lock for a store.
//
// hauler serialises writes within one process, but two processes writing one
// OCI layout will corrupt the index. The lock is the cross-process half of
// that guarantee; the single-replica worker StatefulSet is the other half.
//
// The returned release function must be called on the same connection, so the
// connection is held for the lock's lifetime.
func (d *DB) TryLockStore(ctx context.Context, storeKey int32) (locked bool, release func(), err error) {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return false, nil, err
	}

	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`,
		int32(storeLockNamespace), storeKey).Scan(&ok); err != nil {
		conn.Release()
		return false, nil, err
	}
	if !ok {
		conn.Release()
		return false, nil, nil
	}

	return true, func() {
		// Best effort: releasing the connection would drop the lock anyway
		// when the session ends, but unlocking explicitly returns a healthy
		// connection to the pool instead of one holding a stale lock.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1, $2)`,
			int32(storeLockNamespace), storeKey)
		conn.Release()
	}, nil
}

// InTx runs fn inside a transaction, rolling back on error.
func (d *DB) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}
