package planner

import (
	"errors"
	"strings"
	"testing"
)

// harness builds an Observed from compact literals so each test case reads as
// a statement about state rather than a pile of map construction.
type harness struct {
	resolved  map[string]string
	resolveEr map[string]error
	knownHash map[string]string
	store     map[string]bool
	delivered map[string]bool // "targetID|digest"
	tracked   []Desired
}

func (h harness) observed() Observed {
	return Observed{
		ResolvedDigest: h.resolved,
		ResolveError:   h.resolveEr,
		KnownSpecHash:  h.knownHash,
		Tracked:        h.tracked,
		InStore:        func(d string) bool { return h.store[d] },
		Delivered:      func(t, d string) bool { return h.delivered[t+"|"+d] },
	}
}

var (
	harbor  = Target{ID: "t1", Name: "harbor", Kind: TargetRegistry}
	backup  = Target{ID: "t2", Name: "backup-registry", Kind: TargetRegistry}
	coldS3  = Target{ID: "t3", Name: "cold-storage", Kind: TargetArchive}
	alpine  = Desired{Ref: "alpine:3.20", Platform: "linux/amd64", SpecHash: "h1"}
	busybox = Desired{Ref: "busybox:stable", SpecHash: "h2"}
)

func itemFor(t *testing.T, p *Plan, ref string) Item {
	t.Helper()
	for _, it := range p.Items {
		if it.Desired.Ref == ref {
			return it
		}
	}
	t.Fatalf("no plan item for %q; got %+v", ref, p.Items)
	return Item{}
}

func TestBuildClassifiesEachCase(t *testing.T) {
	tests := []struct {
		name       string
		desired    []Desired
		h          harness
		opts       Options
		wantAction Action
		wantReason string
	}{
		{
			name:    "never seen before is a pull",
			desired: []Desired{alpine},
			h: harness{
				resolved: map[string]string{alpine.Key(): "sha256:aaa"},
			},
			opts:       Options{Targets: []Target{harbor}},
			wantAction: ActionPull,
			wantReason: "new entry",
		},
		{
			// The headline case: nothing changed, so nothing happens.
			name:    "seen, stored, and delivered is a skip",
			desired: []Desired{alpine},
			h: harness{
				resolved:  map[string]string{alpine.Key(): "sha256:aaa"},
				knownHash: map[string]string{alpine.Key(): "h1"},
				store:     map[string]bool{"sha256:aaa": true},
				delivered: map[string]bool{"t1|sha256:aaa": true},
			},
			opts:       Options{Targets: []Target{harbor}},
			wantAction: ActionSkipPresent,
		},
		{
			name:    "tag moved to a new digest is a pull",
			desired: []Desired{alpine},
			h: harness{
				// Upstream now resolves to bbb; the store only holds aaa.
				resolved:  map[string]string{alpine.Key(): "sha256:bbb"},
				knownHash: map[string]string{alpine.Key(): "h1"},
				store:     map[string]bool{"sha256:aaa": true},
				delivered: map[string]bool{"t1|sha256:aaa": true},
			},
			opts:       Options{Targets: []Target{harbor}},
			wantAction: ActionPull,
			wantReason: "digest not present in the store",
		},
		{
			name:    "in store but never delivered is a push",
			desired: []Desired{alpine},
			h: harness{
				resolved:  map[string]string{alpine.Key(): "sha256:aaa"},
				knownHash: map[string]string{alpine.Key(): "h1"},
				store:     map[string]bool{"sha256:aaa": true},
			},
			opts:       Options{Targets: []Target{harbor}},
			wantAction: ActionPush,
			wantReason: "not yet delivered to harbor",
		},
		{
			// Same digest, same store, but the operator tightened cosign
			// verification. Skipping would leave the policy unapplied.
			name:    "spec changed with an unchanged digest is still a pull",
			desired: []Desired{{Ref: "alpine:3.20", Platform: "linux/amd64", SpecHash: "h2-new"}},
			h: harness{
				resolved:  map[string]string{alpine.Key(): "sha256:aaa"},
				knownHash: map[string]string{alpine.Key(): "h1"},
				store:     map[string]bool{"sha256:aaa": true},
				delivered: map[string]bool{"t1|sha256:aaa": true},
			},
			opts:       Options{Targets: []Target{harbor}},
			wantAction: ActionPull,
			wantReason: "manifest entry changed",
		},
		{
			name:    "resolution failure is an error, not a skip",
			desired: []Desired{alpine},
			h: harness{
				resolveEr: map[string]error{alpine.Key(): errors.New("401 unauthorized")},
			},
			opts:       Options{Targets: []Target{harbor}},
			wantAction: ActionError,
			wantReason: "401 unauthorized",
		},
		{
			// A missing digest with no recorded error must never fall through
			// to "skip" -- that would report success for an image nobody
			// fetched.
			name:       "missing digest with no error is still an error",
			desired:    []Desired{alpine},
			h:          harness{},
			opts:       Options{Targets: []Target{harbor}},
			wantAction: ActionError,
		},
		{
			name:    "force re-pull overrides a complete state",
			desired: []Desired{alpine},
			h: harness{
				resolved:  map[string]string{alpine.Key(): "sha256:aaa"},
				knownHash: map[string]string{alpine.Key(): "h1"},
				store:     map[string]bool{"sha256:aaa": true},
				delivered: map[string]bool{"t1|sha256:aaa": true},
			},
			opts:       Options{Targets: []Target{harbor}, ForceRepull: true},
			wantAction: ActionPull,
			wantReason: "forced",
		},
		{
			name:    "digest-pinned ref missing from the store is a pull",
			desired: []Desired{{Ref: "gcr.io/distroless/base@sha256:ccc", SpecHash: "h3"}},
			h: harness{
				resolved:  map[string]string{"gcr.io/distroless/base@sha256:ccc|": "sha256:ccc"},
				knownHash: map[string]string{"gcr.io/distroless/base@sha256:ccc|": "h3"},
			},
			opts:       Options{Targets: []Target{harbor}},
			wantAction: ActionPull,
			wantReason: "pinned digest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Build(tt.desired, tt.h.observed(), tt.opts)
			if len(p.Items) != 1 {
				t.Fatalf("len(Items) = %d, want 1: %+v", len(p.Items), p.Items)
			}
			got := p.Items[0]
			if got.Action != tt.wantAction {
				t.Errorf("Action = %q, want %q (reason: %s)", got.Action, tt.wantAction, got.Reason)
			}
			if tt.wantReason != "" && !strings.Contains(got.Reason, tt.wantReason) {
				t.Errorf("Reason = %q, want it to contain %q", got.Reason, tt.wantReason)
			}
		})
	}
}

// With several targets, an image delivered to one but not the other is not
// done. Treating "delivered anywhere" as "delivered" would leave a registry
// permanently missing images.
func TestPartialDeliveryAcrossTargets(t *testing.T) {
	h := harness{
		resolved:  map[string]string{alpine.Key(): "sha256:aaa"},
		knownHash: map[string]string{alpine.Key(): "h1"},
		store:     map[string]bool{"sha256:aaa": true},
		delivered: map[string]bool{"t1|sha256:aaa": true}, // harbor only
	}

	p := Build([]Desired{alpine}, h.observed(), Options{Targets: []Target{harbor, backup}})
	it := itemFor(t, p, "alpine:3.20")

	if it.Action != ActionPush {
		t.Fatalf("Action = %q, want %q", it.Action, ActionPush)
	}
	if len(it.Targets) != 1 {
		t.Fatalf("len(Targets) = %d, want 1: %+v", len(it.Targets), it.Targets)
	}
	if it.Targets[0].TargetID != backup.ID {
		t.Errorf("pending target = %q, want %q", it.Targets[0].Name, backup.Name)
	}
}

// Delivery is tracked per digest. An old digest reaching a target says nothing
// about the new one.
func TestDeliveryIsDigestScoped(t *testing.T) {
	h := harness{
		resolved:  map[string]string{alpine.Key(): "sha256:new"},
		knownHash: map[string]string{alpine.Key(): "h1"},
		store:     map[string]bool{"sha256:new": true, "sha256:old": true},
		delivered: map[string]bool{"t1|sha256:old": true},
	}

	p := Build([]Desired{alpine}, h.observed(), Options{Targets: []Target{harbor}})
	it := itemFor(t, p, "alpine:3.20")

	if it.Action != ActionPush {
		t.Fatalf("Action = %q, want %q -- the new digest has not been delivered", it.Action, ActionPush)
	}
	if it.Digest != "sha256:new" {
		t.Errorf("Digest = %q, want the newly resolved digest", it.Digest)
	}
}

// Platform is part of an entry's identity: the amd64 build being present says
// nothing about arm64.
func TestPlatformIsPartOfIdentity(t *testing.T) {
	amd := Desired{Ref: "alpine:3.20", Platform: "linux/amd64", SpecHash: "h1"}
	arm := Desired{Ref: "alpine:3.20", Platform: "linux/arm64", SpecHash: "h1"}

	h := harness{
		resolved: map[string]string{
			amd.Key(): "sha256:amd",
			arm.Key(): "sha256:arm",
		},
		knownHash: map[string]string{amd.Key(): "h1"},
		store:     map[string]bool{"sha256:amd": true},
		delivered: map[string]bool{"t1|sha256:amd": true},
	}

	p := Build([]Desired{amd, arm}, h.observed(), Options{Targets: []Target{harbor}})
	if len(p.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2", len(p.Items))
	}

	byPlatform := map[string]Item{}
	for _, it := range p.Items {
		byPlatform[it.Desired.Platform] = it
	}
	if got := byPlatform["linux/amd64"].Action; got != ActionSkipPresent {
		t.Errorf("amd64 Action = %q, want %q", got, ActionSkipPresent)
	}
	if got := byPlatform["linux/arm64"].Action; got != ActionPull {
		t.Errorf("arm64 Action = %q, want %q", got, ActionPull)
	}
}

func TestPruneOrphans(t *testing.T) {
	gone := Desired{Ref: "removed:1.0", SpecHash: "hx"}
	h := harness{
		resolved:  map[string]string{alpine.Key(): "sha256:aaa"},
		knownHash: map[string]string{alpine.Key(): "h1"},
		store:     map[string]bool{"sha256:aaa": true},
		delivered: map[string]bool{"t1|sha256:aaa": true},
		tracked:   []Desired{alpine, gone},
	}

	// Pruning is opt-in: deleting content without being asked is not
	// acceptable behaviour for an airgap mirror.
	p := Build([]Desired{alpine}, h.observed(), Options{Targets: []Target{harbor}})
	if got := p.Counts()[ActionPrune]; got != 0 {
		t.Errorf("prune count = %d with PruneOrphans off, want 0", got)
	}

	p = Build([]Desired{alpine}, h.observed(), Options{Targets: []Target{harbor}, PruneOrphans: true})
	if got := p.Counts()[ActionPrune]; got != 1 {
		t.Fatalf("prune count = %d, want 1: %+v", got, p.Items)
	}
	it := itemFor(t, p, "removed:1.0")
	if it.Action != ActionPrune {
		t.Errorf("Action = %q, want %q", it.Action, ActionPrune)
	}
}

func TestPruneDoesNotTouchStillDesiredEntries(t *testing.T) {
	h := harness{
		resolved:  map[string]string{alpine.Key(): "sha256:aaa", busybox.Key(): "sha256:bbb"},
		knownHash: map[string]string{alpine.Key(): "h1", busybox.Key(): "h2"},
		store:     map[string]bool{"sha256:aaa": true, "sha256:bbb": true},
		delivered: map[string]bool{"t1|sha256:aaa": true, "t1|sha256:bbb": true},
		tracked:   []Desired{alpine, busybox},
	}

	p := Build([]Desired{alpine, busybox}, h.observed(),
		Options{Targets: []Target{harbor}, PruneOrphans: true})

	if got := p.Counts()[ActionPrune]; got != 0 {
		t.Errorf("prune count = %d, want 0 -- both entries are still declared", got)
	}
	if got := p.Counts()[ActionSkipPresent]; got != 2 {
		t.Errorf("skip count = %d, want 2", got)
	}
}

// A haul is a snapshot of the store. Writing a new one when the store did not
// change wastes an archive upload and its retention window.
func TestArchiveOnlyWhenTheStoreChanges(t *testing.T) {
	stable := harness{
		resolved:  map[string]string{alpine.Key(): "sha256:aaa"},
		knownHash: map[string]string{alpine.Key(): "h1"},
		store:     map[string]bool{"sha256:aaa": true},
		delivered: map[string]bool{"t1|sha256:aaa": true, "t3|sha256:aaa": true},
	}

	p := Build([]Desired{alpine}, stable.observed(), Options{Targets: []Target{harbor, coldS3}})
	if p.Archive {
		t.Error("Archive = true for a run that changes nothing")
	}
	if p.HasWork() {
		t.Errorf("HasWork() = true for a steady-state run: %s", p.Summary())
	}

	// A push-only run also leaves the store untouched.
	pushOnly := stable
	pushOnly.delivered = map[string]bool{}
	p = Build([]Desired{alpine}, pushOnly.observed(), Options{Targets: []Target{harbor, coldS3}})
	if p.Counts()[ActionPush] != 1 {
		t.Fatalf("expected a push-only item, got %s", p.Summary())
	}
	if p.Archive {
		t.Error("Archive = true for a push-only run; the store is unchanged")
	}

	// A pull does change the store.
	fresh := harness{resolved: map[string]string{alpine.Key(): "sha256:aaa"}}
	p = Build([]Desired{alpine}, fresh.observed(), Options{Targets: []Target{harbor, coldS3}})
	if !p.Archive {
		t.Error("Archive = false for a run that pulls new content")
	}
	if len(p.ArchiveTargets) != 1 {
		t.Errorf("len(ArchiveTargets) = %d, want 1", len(p.ArchiveTargets))
	}
}

func TestArchiveRequiresAnArchiveTarget(t *testing.T) {
	fresh := harness{resolved: map[string]string{alpine.Key(): "sha256:aaa"}}
	p := Build([]Desired{alpine}, fresh.observed(), Options{Targets: []Target{harbor}})
	if p.Archive {
		t.Error("Archive = true with no archive target configured")
	}
}

// Archive targets are per-run, not per-image, so they must not appear in an
// item's pending registry targets.
func TestArchiveTargetsAreNotPerImageTargets(t *testing.T) {
	fresh := harness{resolved: map[string]string{alpine.Key(): "sha256:aaa"}}
	p := Build([]Desired{alpine}, fresh.observed(), Options{Targets: []Target{harbor, coldS3}})

	it := itemFor(t, p, "alpine:3.20")
	for _, tp := range it.Targets {
		if tp.Kind == TargetArchive {
			t.Errorf("archive target %q leaked into per-image targets", tp.Name)
		}
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	d := []Desired{
		{Ref: "zzz:1", SpecHash: "h"},
		{Ref: "aaa:1", SpecHash: "h"},
		{Ref: "mmm:1", Platform: "linux/arm64", SpecHash: "h"},
		{Ref: "mmm:1", Platform: "linux/amd64", SpecHash: "h"},
	}
	h := harness{resolved: map[string]string{}}
	for _, x := range d {
		h.resolved[x.Key()] = "sha256:" + x.Ref
	}

	first := Build(d, h.observed(), Options{Targets: []Target{harbor}})
	second := Build(d, h.observed(), Options{Targets: []Target{harbor}})

	if len(first.Items) != len(second.Items) {
		t.Fatalf("item counts differ: %d vs %d", len(first.Items), len(second.Items))
	}
	var order []string
	for i := range first.Items {
		if first.Items[i].Desired.Key() != second.Items[i].Desired.Key() {
			t.Fatalf("item %d differs between runs: %q vs %q",
				i, first.Items[i].Desired.Key(), second.Items[i].Desired.Key())
		}
		order = append(order, first.Items[i].Desired.Key())
	}

	want := []string{"aaa:1|", "mmm:1|linux/amd64", "mmm:1|linux/arm64", "zzz:1|"}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("order[%d] = %q, want %q (full: %v)", i, order[i], want[i], order)
		}
	}
}

// One unreachable registry must not stop every other image from being pulled.
func TestOneResolutionFailureDoesNotBlockOtherWork(t *testing.T) {
	h := harness{
		resolved:  map[string]string{busybox.Key(): "sha256:bbb"},
		resolveEr: map[string]error{alpine.Key(): errors.New("dial tcp: i/o timeout")},
	}

	p := Build([]Desired{alpine, busybox}, h.observed(), Options{Targets: []Target{harbor}})

	if got := p.Counts()[ActionError]; got != 1 {
		t.Errorf("error count = %d, want 1", got)
	}
	if got := p.Counts()[ActionPull]; got != 1 {
		t.Errorf("pull count = %d, want 1 -- busybox is unaffected", got)
	}
	if len(p.Pulls()) != 1 || p.Pulls()[0].Desired.Ref != "busybox:stable" {
		t.Errorf("Pulls() = %+v, want just busybox", p.Pulls())
	}
	if len(p.Errors()) != 1 {
		t.Errorf("len(Errors()) = %d, want 1", len(p.Errors()))
	}
}

func TestNilObservedFunctionsAreSafe(t *testing.T) {
	// A first run has no store and no deliveries; the zero-value Observed
	// must behave as "nothing exists" rather than panicking.
	obs := Observed{ResolvedDigest: map[string]string{alpine.Key(): "sha256:aaa"}}
	p := Build([]Desired{alpine}, obs, Options{Targets: []Target{harbor}})
	if got := p.Items[0].Action; got != ActionPull {
		t.Errorf("Action = %q, want %q", got, ActionPull)
	}
}

func TestPlanHelpers(t *testing.T) {
	h := harness{
		resolved: map[string]string{
			alpine.Key():  "sha256:aaa",
			busybox.Key(): "sha256:bbb",
		},
		knownHash: map[string]string{busybox.Key(): "h2"},
		store:     map[string]bool{"sha256:bbb": true},
	}

	p := Build([]Desired{alpine, busybox}, h.observed(), Options{Targets: []Target{harbor}})

	c := p.Counts()
	if c[ActionPull] != 1 || c[ActionPush] != 1 {
		t.Errorf("Counts() = %v, want one pull and one push", c)
	}
	if len(p.Work()) != 2 {
		t.Errorf("len(Work()) = %d, want 2", len(p.Work()))
	}
	if !p.HasWork() {
		t.Error("HasWork() = false, want true")
	}
	if s := p.Summary(); !strings.Contains(s, "1 pull") || !strings.Contains(s, "1 push") {
		t.Errorf("Summary() = %q", s)
	}
}

func TestDesiredHelpers(t *testing.T) {
	if !(Desired{Ref: "x@sha256:abc"}).IsDigestPinned() {
		t.Error("IsDigestPinned() = false for a digest reference")
	}
	if (Desired{Ref: "x:1.0"}).IsDigestPinned() {
		t.Error("IsDigestPinned() = true for a tag reference")
	}
	if got, want := (Desired{Ref: "x:1", Platform: "linux/amd64"}).String(), "x:1 (linux/amd64)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := (Desired{Ref: "x:1"}).String(), "x:1"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
