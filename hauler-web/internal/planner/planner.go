// Package planner decides what a reconcile run should do.
//
// It is deliberately pure: no database, no network, no subprocess. Everything
// it needs is passed in, and everything it decides comes back as a Plan. That
// makes the interesting cases -- a tag that moved, an image already in Harbor,
// a cosign policy that tightened without the digest changing -- cheap to test
// exhaustively, which matters because this is the component that decides
// whether a pull is skipped. A wrong "skip" is the worst failure this system
// can have: it reports success while the airgap silently lacks an image.
package planner

import (
	"fmt"
	"sort"
	"strings"
)

// Action is what the run should do with one desired entry.
type Action string

const (
	// ActionPull fetches the image into the store, then delivers it.
	ActionPull Action = "pull"

	// ActionPush delivers an image already present in the store. No bytes
	// are fetched from upstream.
	ActionPush Action = "push"

	// ActionSkipPresent means the image is in the store and has reached
	// every enabled target at this digest. Nothing to do.
	ActionSkipPresent Action = "skip_present"

	// ActionPrune removes an image the manifests no longer declare.
	ActionPrune Action = "prune"

	// ActionError marks an entry whose digest could not be resolved. It is a
	// planning outcome rather than a hard failure so that one unreachable
	// registry does not block every other image in the manifest.
	ActionError Action = "error"
)

// Desired is one image declared by the manifests at the commit being
// reconciled.
type Desired struct {
	// Ref is the image reference exactly as written in the manifest.
	Ref string

	// Platform is the requested platform, or "" for all platforms.
	Platform string

	// SpecHash is a digest of the entry's full YAML. It changes when any
	// field changes, including verification settings that leave the
	// reference untouched.
	SpecHash string
}

// Key identifies a desired entry. Platform is part of the identity because
// the same reference pulled for linux/amd64 and linux/arm64 are two distinct
// things to track and deliver.
func (d Desired) Key() string { return d.Ref + "|" + d.Platform }

// String renders the entry for logs and the drift view.
func (d Desired) String() string {
	if d.Platform == "" {
		return d.Ref
	}
	return d.Ref + " (" + d.Platform + ")"
}

// IsDigestPinned reports whether the reference already names a digest, in
// which case it can never drift.
func (d Desired) IsDigestPinned() bool { return strings.Contains(d.Ref, "@sha256:") }

// TargetKind distinguishes the two terminal steps.
type TargetKind string

const (
	// TargetRegistry pushes images to a registry with `hauler store copy`.
	TargetRegistry TargetKind = "registry"

	// TargetArchive writes a haul tarball with `hauler store save`. Archive
	// targets are decided per run rather than per image: a haul is a full
	// snapshot of the store, so it is produced when the store changed at
	// all, not once per image.
	TargetArchive TargetKind = "archive"
)

// Target is an enabled delivery destination.
type Target struct {
	ID   string
	Name string
	Kind TargetKind
}

// Observed is everything known about current state. The function fields keep
// the planner free of the database and the registry client.
type Observed struct {
	// ResolvedDigest maps Desired.Key() to the digest the reference points
	// at upstream right now.
	ResolvedDigest map[string]string

	// ResolveError maps Desired.Key() to a resolution failure. An entry
	// present here becomes ActionError.
	ResolveError map[string]error

	// KnownSpecHash maps Desired.Key() to the spec hash recorded the last
	// time the entry was processed. A missing key means "never seen".
	KnownSpecHash map[string]string

	// InStore reports whether a digest is already in the local hauler store.
	InStore func(digest string) bool

	// Delivered reports whether a digest has successfully reached a target.
	Delivered func(targetID, digest string) bool

	// Tracked is every entry the database currently records, used to find
	// orphans. Entries here that are absent from the desired set are
	// candidates for pruning.
	Tracked []Desired
}

// Options tune a single planning pass.
type Options struct {
	// Targets are the enabled delivery destinations.
	Targets []Target

	// ForceRepull ignores the store and delivery state and re-pulls
	// everything. Exposed in the UI for the case where an operator suspects
	// the store is wrong.
	ForceRepull bool

	// PruneOrphans enables removing tracked entries the manifests no longer
	// declare. Off by default: deleting content is not something to do on an
	// operator's behalf without them asking.
	PruneOrphans bool
}

// TargetPlan is one target an item still needs to reach.
type TargetPlan struct {
	TargetID string
	Name     string
	Kind     TargetKind
}

// Item is the decision for one entry.
type Item struct {
	Desired Desired
	Action  Action

	// Digest is the resolved upstream digest, empty when resolution failed.
	Digest string

	// Reason explains the decision in the drift view. Written for an
	// operator asking "why is this being pulled again?".
	Reason string

	// Err is set when Action is ActionError.
	Err error

	// Targets are the registry targets this item has not reached yet.
	Targets []TargetPlan
}

// Plan is the full decision for a run.
type Plan struct {
	Items []Item

	// Archive is true when the run should write a new haul: the store is
	// going to change, so the snapshot is stale.
	Archive bool

	// ArchiveTargets are the enabled archive destinations.
	ArchiveTargets []TargetPlan
}

// Build computes the plan. The result is deterministic: items come back
// sorted by reference then platform, so two runs over unchanged inputs produce
// identical plans and the drift view does not shuffle between refreshes.
func Build(desired []Desired, obs Observed, opts Options) *Plan {
	inStore := obs.InStore
	if inStore == nil {
		inStore = func(string) bool { return false }
	}
	delivered := obs.Delivered
	if delivered == nil {
		delivered = func(string, string) bool { return false }
	}

	var registryTargets, archiveTargets []TargetPlan
	for _, t := range opts.Targets {
		tp := TargetPlan{TargetID: t.ID, Name: t.Name, Kind: t.Kind}
		switch t.Kind {
		case TargetRegistry:
			registryTargets = append(registryTargets, tp)
		case TargetArchive:
			archiveTargets = append(archiveTargets, tp)
		}
	}

	plan := &Plan{ArchiveTargets: archiveTargets}

	desiredKeys := make(map[string]bool, len(desired))
	for _, d := range desired {
		desiredKeys[d.Key()] = true
	}

	for _, d := range desired {
		plan.Items = append(plan.Items, planOne(d, obs, opts, registryTargets, inStore, delivered))
	}

	if opts.PruneOrphans {
		seen := make(map[string]bool)
		for _, tr := range obs.Tracked {
			key := tr.Key()
			if desiredKeys[key] || seen[key] {
				continue
			}
			seen[key] = true
			plan.Items = append(plan.Items, Item{
				Desired: tr,
				Action:  ActionPrune,
				Reason:  "no longer declared in any manifest at this commit",
			})
		}
	}

	sort.SliceStable(plan.Items, func(i, j int) bool {
		a, b := plan.Items[i].Desired, plan.Items[j].Desired
		if a.Ref != b.Ref {
			return a.Ref < b.Ref
		}
		return a.Platform < b.Platform
	})

	// The haul is a snapshot of the store, so it only needs rewriting when
	// the store's contents will actually change. A run that pushes an
	// already-stored image to a new registry leaves the store identical and
	// must not burn time and object storage on a duplicate archive.
	if len(archiveTargets) > 0 {
		for _, it := range plan.Items {
			if it.Action == ActionPull || it.Action == ActionPrune {
				plan.Archive = true
				break
			}
		}
	}

	return plan
}

func planOne(
	d Desired,
	obs Observed,
	opts Options,
	registryTargets []TargetPlan,
	inStore func(string) bool,
	delivered func(string, string) bool,
) Item {
	key := d.Key()

	if err, ok := obs.ResolveError[key]; ok && err != nil {
		return Item{
			Desired: d,
			Action:  ActionError,
			Err:     err,
			Reason:  fmt.Sprintf("could not resolve digest: %v", err),
		}
	}

	digest := obs.ResolvedDigest[key]
	if digest == "" {
		return Item{
			Desired: d,
			Action:  ActionError,
			Err:     fmt.Errorf("no digest resolved for %s", d),
			Reason:  "no digest resolved",
		}
	}

	// Targets that have not yet received this exact digest. Computed before
	// the force check so a forced re-pull still reports where it will go.
	var pending []TargetPlan
	for _, t := range registryTargets {
		if !delivered(t.TargetID, digest) {
			pending = append(pending, t)
		}
	}

	if opts.ForceRepull {
		return Item{
			Desired: d, Action: ActionPull, Digest: digest,
			Reason:  "forced re-pull requested",
			Targets: registryTargets,
		}
	}

	known, seenBefore := obs.KnownSpecHash[key]

	switch {
	case !seenBefore:
		return Item{
			Desired: d, Action: ActionPull, Digest: digest,
			Reason:  "new entry",
			Targets: pending,
		}

	case known != d.SpecHash:
		// The reference may be unchanged while verification settings moved.
		// Re-pulling is the only way a tightened cosign policy is actually
		// applied, so a spec change always forces a fetch.
		return Item{
			Desired: d, Action: ActionPull, Digest: digest,
			Reason:  "manifest entry changed since last run",
			Targets: pending,
		}

	case !inStore(digest):
		// Either the tag moved to a new digest, or the store lost it.
		reason := "digest not present in the store"
		if d.IsDigestPinned() {
			reason = "pinned digest not present in the store"
		}
		return Item{
			Desired: d, Action: ActionPull, Digest: digest,
			Reason:  reason,
			Targets: pending,
		}

	case len(pending) > 0:
		names := make([]string, 0, len(pending))
		for _, t := range pending {
			names = append(names, t.Name)
		}
		return Item{
			Desired: d, Action: ActionPush, Digest: digest,
			Reason:  "already in the store, not yet delivered to " + strings.Join(names, ", "),
			Targets: pending,
		}

	default:
		return Item{
			Desired: d, Action: ActionSkipPresent, Digest: digest,
			Reason: "already pulled and delivered at this digest",
		}
	}
}

// Counts summarises the plan by action, for the dashboard and run summary.
func (p *Plan) Counts() map[Action]int {
	out := make(map[Action]int, 5)
	for _, it := range p.Items {
		out[it.Action]++
	}
	return out
}

// Work returns the items that require hauler to do something.
func (p *Plan) Work() []Item {
	var out []Item
	for _, it := range p.Items {
		switch it.Action {
		case ActionPull, ActionPush, ActionPrune:
			out = append(out, it)
		}
	}
	return out
}

// Pulls returns only the items needing a fetch. These are what the scoped
// manifest is built from.
func (p *Plan) Pulls() []Item {
	var out []Item
	for _, it := range p.Items {
		if it.Action == ActionPull {
			out = append(out, it)
		}
	}
	return out
}

// Errors returns the entries whose digest could not be resolved.
func (p *Plan) Errors() []Item {
	var out []Item
	for _, it := range p.Items {
		if it.Action == ActionError {
			out = append(out, it)
		}
	}
	return out
}

// HasWork reports whether the run would change anything. A reconcile with no
// work is the steady state and should not create noise.
func (p *Plan) HasWork() bool { return len(p.Work()) > 0 || p.Archive }

// Summary is a one-line description for logs.
func (p *Plan) Summary() string {
	c := p.Counts()
	s := fmt.Sprintf("%d pull, %d push, %d skip", c[ActionPull], c[ActionPush], c[ActionSkipPresent])
	if c[ActionPrune] > 0 {
		s += fmt.Sprintf(", %d prune", c[ActionPrune])
	}
	if c[ActionError] > 0 {
		s += fmt.Sprintf(", %d error", c[ActionError])
	}
	if p.Archive {
		s += ", archive"
	}
	return s
}
