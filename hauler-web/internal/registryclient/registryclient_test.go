package registryclient

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// newTestRegistry starts an in-memory OCI registry and returns its host. Using
// a real registry rather than a mocked HTTP client means these tests exercise
// the actual manifest HEAD path, content negotiation included, without needing
// network access.
func newTestRegistry(t *testing.T) string {
	t.Helper()
	// A discarding logger, not nil: registry.Logger(nil) panics inside the
	// handler on the first request.
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing test registry url: %v", err)
	}
	return u.Host
}

// pushRandomImage publishes a throwaway image and returns its digest.
func pushRandomImage(t *testing.T, ref string) string {
	t.Helper()

	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("building random image: %v", err)
	}
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatalf("parsing %q: %v", ref, err)
	}
	if err := remote.Write(parsed, img); err != nil {
		t.Fatalf("pushing %q: %v", ref, err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatalf("digesting image: %v", err)
	}
	return d.String()
}

func testClient() *Client {
	return &Client{Insecure: true}
}

func TestResolveOneReturnsTheCurrentDigest(t *testing.T) {
	host := newTestRegistry(t)
	ref := host + "/team/app:v1"
	want := pushRandomImage(t, ref)

	got, err := testClient().ResolveOne(context.Background(), Reference{Ref: ref})
	if err != nil {
		t.Fatalf("ResolveOne() error = %v", err)
	}
	if got != want {
		t.Errorf("ResolveOne() = %q, want %q", got, want)
	}
}

// The whole dedupe story rests on noticing when a mutable tag starts pointing
// somewhere new.
func TestResolveOneSeesATagMove(t *testing.T) {
	host := newTestRegistry(t)
	ref := host + "/team/app:stable"

	before := pushRandomImage(t, ref)
	got, err := testClient().ResolveOne(context.Background(), Reference{Ref: ref})
	if err != nil {
		t.Fatalf("ResolveOne() error = %v", err)
	}
	if got != before {
		t.Fatalf("ResolveOne() = %q, want %q", got, before)
	}

	after := pushRandomImage(t, ref) // same tag, new content
	if before == after {
		t.Fatal("test setup produced identical digests")
	}

	got, err = testClient().ResolveOne(context.Background(), Reference{Ref: ref})
	if err != nil {
		t.Fatalf("ResolveOne() after move error = %v", err)
	}
	if got != after {
		t.Errorf("ResolveOne() = %q, want the moved digest %q", got, after)
	}
}

// A digest reference cannot drift, so resolution must not touch the network at
// all. That is what lets a fully pinned manifest reconcile inside an airgap.
func TestResolveOneShortCircuitsDigestReferences(t *testing.T) {
	const digest = "sha256:7fa7445dfbebae4f4b7ab0e6ef99276e96075ae42584af6286ba080750d6dfe5"
	// A host that would fail instantly if contacted.
	ref := "unreachable.invalid/distroless/base@" + digest

	got, err := testClient().ResolveOne(context.Background(), Reference{Ref: ref})
	if err != nil {
		t.Fatalf("ResolveOne() error = %v (it must not contact the registry)", err)
	}
	if got != digest {
		t.Errorf("ResolveOne() = %q, want %q", got, digest)
	}
}

func TestResolveOneRejectsBadInput(t *testing.T) {
	tests := []struct{ name, ref string }{
		{"empty", ""},
		{"whitespace", "   "},
		{"invalid characters", "NOT A REF!!"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := testClient().ResolveOne(context.Background(), Reference{Ref: tt.ref}); err == nil {
				t.Fatalf("ResolveOne(%q) error = nil, want an error", tt.ref)
			}
		})
	}
}

func TestResolveOneReportsMissingTags(t *testing.T) {
	host := newTestRegistry(t)
	_, err := testClient().ResolveOne(context.Background(), Reference{Ref: host + "/team/absent:v1"})
	if err == nil {
		t.Fatal("ResolveOne() on a missing tag error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "resolving") {
		t.Errorf("error = %q, want it to name the reference being resolved", err)
	}
}

// One unreachable registry must not take the rest of the plan down with it.
func TestResolvePartitionsSuccessesAndFailures(t *testing.T) {
	host := newTestRegistry(t)
	okRef := host + "/team/ok:v1"
	wantDigest := pushRandomImage(t, okRef)

	refs := []Reference{
		{Ref: okRef, Platform: "linux/amd64"},
		{Ref: host + "/team/missing:v1"},
		// Digest-pinned against a host that does not resolve: it must still
		// succeed, proving no network call was made.
		{Ref: "unreachable.invalid/distroless/base@sha256:7fa7445dfbebae4f4b7ab0e6ef99276e96075ae42584af6286ba080750d6dfe5"},
	}

	digests, failures := testClient().Resolve(context.Background(), refs)

	if got, want := len(digests), 2; got != want {
		t.Errorf("len(digests) = %d, want %d: %v", got, want, digests)
	}
	if got, want := len(failures), 1; got != want {
		t.Errorf("len(failures) = %d, want %d: %v", got, want, failures)
	}

	// Keys must match planner.Desired.Key() so the maps drop straight into
	// planner.Observed.
	if got := digests[okRef+"|linux/amd64"]; got != wantDigest {
		t.Errorf("digests[%q] = %q, want %q", okRef+"|linux/amd64", got, wantDigest)
	}
	if _, ok := failures[host+"/team/missing:v1|"]; !ok {
		t.Errorf("expected a failure recorded for the missing tag, got %v", failures)
	}
}

func TestResolveHandlesEmptyInput(t *testing.T) {
	digests, failures := testClient().Resolve(context.Background(), nil)
	if len(digests) != 0 || len(failures) != 0 {
		t.Errorf("Resolve(nil) = %v, %v, want two empty maps", digests, failures)
	}
}

func TestExists(t *testing.T) {
	host := newTestRegistry(t)
	repo := host + "/team/app"
	digest := pushRandomImage(t, repo+":v1")

	c := testClient()

	present, err := c.Exists(context.Background(), repo, digest)
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if !present {
		t.Error("Exists() = false for a digest that was just pushed")
	}

	// A digest that was never pushed must read as absent, not as an error.
	absent, err := c.Exists(context.Background(), repo,
		"sha256:0000000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatalf("Exists() on an absent digest error = %v, want a clean false", err)
	}
	if absent {
		t.Error("Exists() = true for a digest that was never pushed")
	}
}

func TestExistsRejectsBadInput(t *testing.T) {
	c := testClient()
	if _, err := c.Exists(context.Background(), "!!bad repo!!", "sha256:abc"); err == nil {
		t.Error("Exists() with a bad repository: expected an error")
	}
	if _, err := c.Exists(context.Background(), "example.com/x", "not-a-digest"); err == nil {
		t.Error("Exists() with a bad digest: expected an error")
	}
}

// An index (multi-platform image) resolves to the index digest. Tracking that
// is what answers "has this tag moved?" for a manifest list.
func TestResolveOneHandlesMultiPlatformIndexes(t *testing.T) {
	host := newTestRegistry(t)
	ref := host + "/team/multi:v1"

	idx, err := random.Index(256, 1, 2)
	if err != nil {
		t.Fatalf("building random index: %v", err)
	}
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(parsed, idx); err != nil {
		t.Fatalf("pushing index: %v", err)
	}
	want, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}

	got, err := testClient().ResolveOne(context.Background(), Reference{Ref: ref, Platform: "linux/amd64"})
	if err != nil {
		t.Fatalf("ResolveOne() error = %v", err)
	}
	if got != want.String() {
		t.Errorf("ResolveOne() = %q, want the index digest %q", got, want)
	}
}

func TestSplitHelpers(t *testing.T) {
	tests := []struct {
		ref      string
		wantRepo string
		wantTag  string
	}{
		{"alpine:3.20", "index.docker.io/library/alpine", "3.20"},
		{"ghcr.io/org/app:v1.2.3", "ghcr.io/org/app", "v1.2.3"},
		{"ghcr.io/org/app", "ghcr.io/org/app", "latest"},
		{
			"gcr.io/distroless/base@sha256:7fa7445dfbebae4f4b7ab0e6ef99276e96075ae42584af6286ba080750d6dfe5",
			"gcr.io/distroless/base",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			repo, err := SplitRepository(tt.ref)
			if err != nil {
				t.Fatalf("SplitRepository() error = %v", err)
			}
			if repo != tt.wantRepo {
				t.Errorf("SplitRepository() = %q, want %q", repo, tt.wantRepo)
			}
			if tag := SplitTag(tt.ref); tag != tt.wantTag {
				t.Errorf("SplitTag() = %q, want %q", tag, tt.wantTag)
			}
		})
	}

	if _, err := SplitRepository("!!bad!!"); err == nil {
		t.Error("SplitRepository() with a bad reference: expected an error")
	}
}

// Guard against the empty-image edge case producing a bogus digest.
func TestResolveOneOnAnEmptyImage(t *testing.T) {
	host := newTestRegistry(t)
	ref := host + "/team/empty:v1"

	img := mutate.MediaType(empty.Image, "application/vnd.oci.image.manifest.v1+json")
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(parsed, img); err != nil {
		t.Fatalf("pushing empty image: %v", err)
	}
	want, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}

	got, err := testClient().ResolveOne(context.Background(), Reference{Ref: ref})
	if err != nil {
		t.Fatalf("ResolveOne() error = %v", err)
	}
	if got != want.String() {
		t.Errorf("ResolveOne() = %q, want %q", got, want)
	}
	var _ v1.Hash // keep the v1 import meaningful if this test is trimmed
}
