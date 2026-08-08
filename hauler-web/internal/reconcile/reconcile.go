// Package reconcile drives one pass of desired state into reality.
//
// It is the seam where the pure planner meets the side-effecting world: git,
// registries, the hauler subprocess, and Postgres. Everything it decides comes
// from planner.Build; everything it records is a fact about work that actually
// completed. That split is deliberate -- a "skip" must only ever be recorded
// because a previous run genuinely finished, never because this one assumed it
// had.
package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Drblanco24/hauler-web/internal/db"
	"github.com/Drblanco24/hauler-web/internal/gitsource"
	"github.com/Drblanco24/hauler-web/internal/hauler"
	"github.com/Drblanco24/hauler-web/internal/manifest"
	"github.com/Drblanco24/hauler-web/internal/planner"
	"github.com/Drblanco24/hauler-web/internal/registryclient"
)

// Reconciler performs reconcile passes against one hauler store.
type Reconciler struct {
	DB       *db.DB
	Hauler   *hauler.Client
	Registry *registryclient.Client
	Logger   *slog.Logger

	// WorkDir holds git checkouts and generated manifests.
	WorkDir string

	// Concurrency is hauler's -j.
	Concurrency int

	// LogTailBytes bounds how much run output is kept inline in Postgres.
	LogTailBytes int
}

// Options tune a single pass.
type Options struct {
	Source      db.Source
	Trigger     string
	ForceRepull bool

	// SkipUnchanged returns early when the source's commit has not moved.
	// Manual and forced runs set this false so an operator can always make
	// something happen.
	SkipUnchanged bool
}

// Result summarises a pass.
type Result struct {
	RunID     int64
	CommitSHA string
	Status    string
	Plan      *planner.Plan
	Counts    map[planner.Action]int

	// Skipped is true when the pass returned early because nothing changed.
	Skipped bool
}

// ErrStoreBusy means another worker holds the store lock.
var ErrStoreBusy = errors.New("another worker is using this store")

func (r *Reconciler) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// Once performs a single reconcile pass.
func (r *Reconciler) Once(ctx context.Context, opts Options) (*Result, error) {
	log := r.logger().With("source", opts.Source.Name)

	// Two processes writing one OCI layout corrupts its index. hauler
	// serialises within a process; this is the cross-process half.
	locked, release, err := r.DB.TryLockStore(ctx, storeKey(r.Hauler.StoreDir))
	if err != nil {
		return nil, fmt.Errorf("acquiring the store lock: %w", err)
	}
	if !locked {
		return nil, ErrStoreBusy
	}
	defer release()

	// ---- desired state ---------------------------------------------------
	repo, commitSHA, err := r.syncRepo(ctx, opts.Source)
	if err != nil {
		_ = r.DB.MarkSourcePolled(ctx, opts.Source.ID, "", err)
		return nil, err
	}

	if opts.SkipUnchanged && commitSHA == opts.Source.LastCommitSHA && !opts.ForceRepull {
		log.Debug("commit unchanged, nothing to reconcile", "commit", short(commitSHA))
		return &Result{CommitSHA: commitSHA, Status: "succeeded", Skipped: true}, nil
	}

	desired, manifests, err := r.loadDesired(ctx, opts.Source, repo, commitSHA)
	if err != nil {
		_ = r.DB.MarkSourcePolled(ctx, opts.Source.ID, commitSHA, err)
		return nil, err
	}

	// ---- observed state --------------------------------------------------
	obs, imageIDs, err := r.observe(ctx, desired)
	if err != nil {
		return nil, err
	}

	targets, targetsByID, err := r.loadTargets(ctx)
	if err != nil {
		return nil, err
	}

	plan := planner.Build(desired, obs, planner.Options{
		Targets:      targets,
		ForceRepull:  opts.ForceRepull,
		PruneOrphans: opts.Source.PruneOrphans,
	})
	log.Info("planned", "commit", short(commitSHA), "summary", plan.Summary())

	// ---- execute ---------------------------------------------------------
	haulerVersion, _ := r.Hauler.Version(ctx)
	sourceID := opts.Source.ID
	runID, err := r.DB.CreateRun(ctx, &sourceID, commitSHA, triggerOr(opts.Trigger),
		"", haulerVersion, opts.ForceRepull)
	if err != nil {
		return nil, err
	}

	res := &Result{RunID: runID, CommitSHA: commitSHA, Plan: plan, Counts: plan.Counts()}
	var logBuf tailBuffer
	logBuf.limit = r.logTailBytes()

	execErr := r.execute(ctx, runID, plan, manifests, imageIDs, targetsByID, &logBuf)

	status := "succeeded"
	switch {
	case execErr != nil:
		status = "failed"
	case len(plan.Errors()) > 0:
		// Some images could not even be resolved. The run did real work, so
		// it is not a failure, but it must not read as a clean success
		// either -- that is how a missing image goes unnoticed.
		status = "partial"
	}
	res.Status = status

	stats := map[string]any{
		"pull":         plan.Counts()[planner.ActionPull],
		"push":         plan.Counts()[planner.ActionPush],
		"skip_present": plan.Counts()[planner.ActionSkipPresent],
		"prune":        plan.Counts()[planner.ActionPrune],
		"error":        plan.Counts()[planner.ActionError],
	}
	if err := r.DB.FinishRun(ctx, runID, status, stats, logBuf.String(), execErr); err != nil {
		log.Warn("could not finalise the run record", "error", err)
	}
	if err := r.DB.MarkSourcePolled(ctx, opts.Source.ID, commitSHA, execErr); err != nil {
		log.Warn("could not record the poll", "error", err)
	}

	if execErr != nil {
		return res, execErr
	}
	log.Info("reconciled", "run", runID, "status", status, "summary", plan.Summary())
	return res, nil
}

// syncRepo brings the source's checkout up to date.
func (r *Reconciler) syncRepo(ctx context.Context, src db.Source) (*gitsource.Repo, string, error) {
	repo := r.repoFor(src)
	sha, err := repo.Sync(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("syncing %s: %w", src.Name, err)
	}
	return repo, sha, nil
}

// loadDesired parses every manifest at the current commit, records the
// revision, and returns planner input alongside the manifest entries needed to
// regenerate a scoped manifest later.
func (r *Reconciler) loadDesired(ctx context.Context, src db.Source, repo *gitsource.Repo, commitSHA string) ([]planner.Desired, []*manifest.Manifest, error) {
	paths, err := repo.Manifests(src.PathGlob)
	if err != nil {
		return nil, nil, err
	}
	if len(paths) == 0 {
		return nil, nil, fmt.Errorf("no manifests matched %q in %s at %s", src.PathGlob, src.Name, short(commitSHA))
	}

	var desired []planner.Desired
	var manifests []*manifest.Manifest
	seen := map[string]bool{}

	for _, rel := range paths {
		abs := repo.Path(rel)
		raw, err := os.ReadFile(abs)
		if err != nil {
			return nil, nil, err
		}
		m, err := manifest.ParseFile(abs)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", rel, err)
		}

		var items []db.DesiredItem
		for _, e := range m.Images() {
			hash, err := e.SpecHash()
			if err != nil {
				return nil, nil, err
			}
			rawYAML, err := e.RawYAML()
			if err != nil {
				return nil, nil, err
			}
			d := planner.Desired{
				Ref:      manifest.NormalizeRef(e.Ref()),
				Platform: e.Platform(),
				SpecHash: hash,
			}
			items = append(items, db.DesiredItem{
				Kind: "image", Ref: d.Ref, Platform: d.Platform,
				RawYAML: rawYAML, SpecHash: hash, Position: e.Index,
			})
			// The same image may be declared in two manifests. Planning it
			// twice would double every count and race two writers onto one
			// run_items row.
			if seen[d.Key()] {
				continue
			}
			seen[d.Key()] = true
			desired = append(desired, d)
		}
		manifests = append(manifests, m)

		sum := sha256.Sum256(raw)
		if _, err := r.DB.RecordRevision(ctx, src.ID, rel, commitSHA, hex.EncodeToString(sum[:]), items); err != nil {
			return nil, nil, err
		}
	}

	return desired, manifests, nil
}

// observe gathers current reality: what upstream resolves to, what the store
// holds, and what has already been delivered.
func (r *Reconciler) observe(ctx context.Context, desired []planner.Desired) (planner.Observed, map[string]int64, error) {
	refs := make([]registryclient.Reference, 0, len(desired))
	for _, d := range desired {
		refs = append(refs, registryclient.Reference{Ref: d.Ref, Platform: d.Platform})
	}
	digests, failures := r.Registry.Resolve(ctx, refs)

	info, err := r.Hauler.InfoIfExists(ctx, hauler.InfoOptions{})
	if err != nil {
		return planner.Observed{}, nil, fmt.Errorf("reading the hauler store: %w", err)
	}

	tracked, err := r.DB.TrackedImages(ctx)
	if err != nil {
		return planner.Observed{}, nil, err
	}
	known := make(map[string]string, len(tracked))
	trackedDesired := make([]planner.Desired, 0, len(tracked))
	imageIDs := make(map[string]int64, len(tracked))
	for _, im := range tracked {
		key := im.Ref + "|" + im.Platform
		imageIDs[key] = im.ID
		if im.LastSpecHash != "" {
			known[key] = im.LastSpecHash
		}
		trackedDesired = append(trackedDesired, planner.Desired{Ref: im.Ref, Platform: im.Platform})
	}

	// Register every currently-desired image so run items can reference it,
	// even the ones that will be skipped.
	for _, d := range desired {
		repository, tag := splitRef(d.Ref)
		id, err := r.DB.UpsertImage(ctx, d.Ref, repository, tag, d.Platform)
		if err != nil {
			return planner.Observed{}, nil, err
		}
		imageIDs[d.Key()] = id
		if digest := digests[d.Key()]; digest != "" {
			if err := r.DB.RecordDigest(ctx, id, digest, nil); err != nil {
				return planner.Observed{}, nil, err
			}
		}
	}

	delivered, err := r.DB.LoadDelivered(ctx)
	if err != nil {
		return planner.Observed{}, nil, err
	}

	return planner.Observed{
		ResolvedDigest: digests,
		ResolveError:   failures,
		KnownSpecHash:  known,
		Tracked:        trackedDesired,
		InStore:        info.HasDigest,
		Delivered: func(targetID, digest string) bool {
			var id int64
			if _, err := fmt.Sscanf(targetID, "%d", &id); err != nil {
				return false
			}
			return delivered.Has(id, digest)
		},
	}, imageIDs, nil
}

func (r *Reconciler) loadTargets(ctx context.Context) ([]planner.Target, map[string]db.Target, error) {
	rows, err := r.DB.ListEnabledTargets(ctx)
	if err != nil {
		return nil, nil, err
	}
	targets := make([]planner.Target, 0, len(rows))
	byID := make(map[string]db.Target, len(rows))
	for _, t := range rows {
		id := fmt.Sprintf("%d", t.ID)
		targets = append(targets, planner.Target{ID: id, Name: t.Name, Kind: planner.TargetKind(t.Kind)})
		byID[id] = t
	}
	return targets, byID, nil
}

// execute performs the plan's work and records what happened.
func (r *Reconciler) execute(
	ctx context.Context,
	runID int64,
	plan *planner.Plan,
	manifests []*manifest.Manifest,
	imageIDs map[string]int64,
	targetsByID map[string]db.Target,
	logBuf *tailBuffer,
) error {
	log := r.logger()

	// ---- pull ------------------------------------------------------------
	pulls := plan.Pulls()
	if len(pulls) > 0 {
		if err := r.pull(ctx, pulls, manifests, logBuf); err != nil {
			r.recordItems(ctx, runID, plan, imageIDs, "failed", err)
			return err
		}
	}

	// Re-read the store so recorded sizes and digests describe what is
	// actually on disk, not what was predicted.
	info, err := r.Hauler.InfoIfExists(ctx, hauler.InfoOptions{})
	if err != nil {
		return fmt.Errorf("reading the store after sync: %w", err)
	}
	byDigest := map[string]hauler.Artifact{}
	for _, a := range info.Images() {
		byDigest[a.Digest] = a
	}

	// ---- deliver ---------------------------------------------------------
	// hauler copies the whole store, not individual references, so a target
	// needing any image gets one copy and every image then in the store is
	// recorded as delivered to it. Recording per-image would either lie
	// (claiming images the copy did not touch were skipped) or require a
	// copy per image, which would be far slower for no gain.
	needed := map[string]bool{}
	for _, it := range plan.Items {
		if it.Action != planner.ActionPull && it.Action != planner.ActionPush {
			continue
		}
		for _, tp := range it.Targets {
			needed[tp.TargetID] = true
		}
	}

	for targetID := range needed {
		t, ok := targetsByID[targetID]
		if !ok || t.Kind != string(planner.TargetRegistry) {
			continue
		}
		if err := r.copyTo(ctx, t, logBuf); err != nil {
			r.recordItems(ctx, runID, plan, imageIDs, "failed", err)
			return fmt.Errorf("delivering to %s: %w", t.Name, err)
		}
		if err := r.recordDeliveries(ctx, runID, t, info, imageIDs); err != nil {
			return err
		}
		log.Info("delivered", "target", t.Name, "images", len(info.Images()))
	}

	// ---- prune -----------------------------------------------------------
	for _, it := range plan.Items {
		if it.Action != planner.ActionPrune {
			continue
		}
		if _, err := r.Hauler.Remove(ctx, it.Desired.Ref, logBuf); err != nil {
			log.Warn("could not prune", "ref", it.Desired.Ref, "error", err)
			continue
		}
		if id, ok := imageIDs[it.Desired.Key()]; ok {
			if err := r.DB.DeleteImage(ctx, id); err != nil {
				log.Warn("could not delete the pruned image row", "ref", it.Desired.Ref, "error", err)
			}
		}
	}

	// ---- record ----------------------------------------------------------
	r.recordItemsWithStore(ctx, runID, plan, imageIDs, byDigest)

	// A pull is only "done" once it succeeded, so the spec hash is written
	// here rather than at planning time. Writing it earlier would make a
	// failed pull look handled on the next run.
	for _, it := range plan.Pulls() {
		id, ok := imageIDs[it.Desired.Key()]
		if !ok {
			continue
		}
		if _, inStore := byDigest[it.Digest]; !inStore {
			// hauler did not end up storing it; leave the hash unset so the
			// next run retries.
			continue
		}
		if err := r.DB.SetImageSpecHash(ctx, id, it.Desired.SpecHash); err != nil {
			log.Warn("could not record the spec hash", "ref", it.Desired.Ref, "error", err)
		}
		if err := r.DB.RecordDigest(ctx, id, it.Digest, &runID); err != nil {
			log.Warn("could not record the digest", "ref", it.Desired.Ref, "error", err)
		}
	}

	return nil
}

// pull generates manifests scoped to exactly the images that need fetching
// and hands them to hauler.
//
// One scoped file is produced per source manifest rather than one combined
// file. hauler accepts repeated --filename, and keeping the split preserves
// each document's own metadata and annotations -- which resolveImageJobs reads
// -- without this package having to merge YAML streams by hand.
func (r *Reconciler) pull(ctx context.Context, pulls []planner.Item, manifests []*manifest.Manifest, logBuf *tailBuffer) error {
	wanted := make(map[string]bool, len(pulls))
	for _, it := range pulls {
		wanted[it.Desired.Key()] = true
	}

	if err := os.MkdirAll(r.WorkDir, 0o755); err != nil {
		return err
	}

	var files []string
	for i, m := range manifests {
		body, ok, err := m.Scope(func(e manifest.Entry) bool {
			if e.Kind != manifest.EntryImage {
				return false
			}
			key := manifest.NormalizeRef(e.Ref()) + "|" + e.Platform()
			return wanted[key]
		})
		if err != nil {
			return fmt.Errorf("generating a scoped manifest: %w", err)
		}
		if !ok {
			continue
		}
		path := filepath.Join(r.WorkDir, fmt.Sprintf("scoped-%02d.yaml", i))
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return fmt.Errorf("writing the scoped manifest: %w", err)
		}
		files = append(files, path)
	}

	if len(files) == 0 {
		return nil
	}

	r.logger().Info("pulling", "images", len(pulls), "manifests", len(files))
	_, err := r.Hauler.Sync(ctx, hauler.SyncOptions{
		Filenames:   files,
		Concurrency: r.Concurrency,
	}, logBuf)
	return err
}

func (r *Reconciler) copyTo(ctx context.Context, t db.Target, logBuf *tailBuffer) error {
	target, insecure, plainHTTP := registryTargetRef(t)
	if target == "" {
		return fmt.Errorf("target %q has no registry url configured", t.Name)
	}
	_, err := r.Hauler.Copy(ctx, hauler.CopyOptions{
		Target:    target,
		Insecure:  insecure,
		PlainHTTP: plainHTTP,
	}, logBuf)
	return err
}

func (r *Reconciler) recordDeliveries(ctx context.Context, runID int64, t db.Target, info *hauler.StoreInfo, imageIDs map[string]int64) error {
	for _, a := range info.Images() {
		id, ok := imageIDs[a.Reference+"|"+a.Platform]
		if !ok {
			// hauler normalises platform to "-" for artifacts with none, and
			// stores references it derived rather than ones we declared.
			// Fall back to a ref-only match before giving up.
			id, ok = lookupByRef(imageIDs, a.Reference)
		}
		if !ok {
			continue
		}
		if err := r.DB.RecordDelivery(ctx, &runID, t.ID, id, a.Digest, a.Reference, nil); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) recordItems(ctx context.Context, runID int64, plan *planner.Plan, imageIDs map[string]int64, status string, cause error) {
	for _, it := range plan.Items {
		var imgID *int64
		if id, ok := imageIDs[it.Desired.Key()]; ok {
			imgID = &id
		}
		_ = r.DB.InsertRunItem(ctx, db.RunItem{
			RunID: runID, ImageID: imgID, Ref: it.Desired.Ref, Platform: it.Desired.Platform,
			Action: string(it.Action), Status: status, Reason: it.Reason, Digest: it.Digest, Err: cause,
		})
	}
}

func (r *Reconciler) recordItemsWithStore(ctx context.Context, runID int64, plan *planner.Plan, imageIDs map[string]int64, byDigest map[string]hauler.Artifact) {
	for _, it := range plan.Items {
		var imgID *int64
		if id, ok := imageIDs[it.Desired.Key()]; ok {
			imgID = &id
		}

		status := "succeeded"
		var itemErr error
		switch it.Action {
		case planner.ActionSkipPresent:
			status = "skipped"
		case planner.ActionError:
			status = "failed"
			itemErr = it.Err
		}

		item := db.RunItem{
			RunID: runID, ImageID: imgID, Ref: it.Desired.Ref, Platform: it.Desired.Platform,
			Action: string(it.Action), Status: status, Reason: it.Reason,
			Digest: it.Digest, Err: itemErr,
		}
		if a, ok := byDigest[it.Digest]; ok {
			size, layers := a.Size, a.Layers
			item.Size, item.Layers = &size, &layers
		}
		_ = r.DB.InsertRunItem(ctx, item)
	}
}

func (r *Reconciler) logTailBytes() int {
	if r.LogTailBytes > 0 {
		return r.LogTailBytes
	}
	return 32 * 1024
}

// storeKey derives a stable advisory-lock key from a store path. A collision
// would only ever over-serialise two unrelated stores, never under-serialise
// one, so a 32-bit hash is safe here.
func storeKey(dir string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(filepath.Clean(dir)))
	return int32(h.Sum32()) //nolint:gosec // intentional truncation; see above
}

func triggerOr(t string) string {
	if t == "" {
		return "schedule"
	}
	return t
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func splitRef(ref string) (repository, tag string) {
	repository, err := registryclient.SplitRepository(ref)
	if err != nil {
		return ref, ""
	}
	return repository, registryclient.SplitTag(ref)
}

func lookupByRef(ids map[string]int64, ref string) (int64, bool) {
	for key, id := range ids {
		if strings.HasPrefix(key, ref+"|") {
			return id, true
		}
	}
	return 0, false
}

// tailBuffer keeps the last N bytes of run output for the run record while
// discarding the rest. A large sync can emit megabytes; only the end is
// useful inline, and the full log goes to object storage separately.
type tailBuffer struct {
	buf   []byte
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if t.limit > 0 && len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
