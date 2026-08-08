package manifest

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The fixtures are verbatim copies of the hauler fork's
// testdata/hauler-manifest.yaml and testdata/hauler-manifest-pipeline.yaml.
// Parsing what upstream's own integration suite exercises is the cheapest
// guard against drifting from hauler's manifest semantics.
const (
	fixtureBasic    = "testdata/hauler-manifest.yaml"
	fixturePipeline = "testdata/hauler-manifest-pipeline.yaml"
)

func TestParseFileBasicFixture(t *testing.T) {
	f, err := ParseFile(fixtureBasic)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}

	if got, want := len(f.Documents), 3; got != want {
		t.Fatalf("len(Documents) = %d, want %d", got, want)
	}

	wantKinds := []string{KindImages, KindCharts, KindFiles}
	for i, want := range wantKinds {
		if got := f.Documents[i].Kind; got != want {
			t.Errorf("Documents[%d].Kind = %q, want %q", i, got, want)
		}
		if got, want := f.Documents[i].APIVersion, APIVersionContent; got != want {
			t.Errorf("Documents[%d].APIVersion = %q, want %q", i, got, want)
		}
	}

	if got, want := f.Documents[0].Name, "hauler-content-images-example"; got != want {
		t.Errorf("Documents[0].Name = %q, want %q", got, want)
	}

	images := f.Images()
	if got, want := len(images), 4; got != want {
		t.Fatalf("len(Images()) = %d, want %d", got, want)
	}

	wantRefs := []string{
		"ghcr.io/hauler-dev/library/busybox",
		"ghcr.io/hauler-dev/library/busybox:stable",
		"gcr.io/distroless/base@sha256:7fa7445dfbebae4f4b7ab0e6ef99276e96075ae42584af6286ba080750d6dfe5",
		"ghcr.io/kubewarden/audit-scanner:v1.30.0-rc1",
	}
	for i, want := range wantRefs {
		if got := images[i].Ref(); got != want {
			t.Errorf("Images()[%d].Ref() = %q, want %q", i, got, want)
		}
	}

	// Hyphenated and nested fields must survive the YAML->JSON->struct path.
	if got, want := images[1].Platform(), "linux/amd64"; got != want {
		t.Errorf("images[1].Platform() = %q, want %q", got, want)
	}
	kubewarden := images[3].Image
	if kubewarden.CertOidcIssuer != "https://token.actions.githubusercontent.com" {
		t.Errorf("CertOidcIssuer = %q", kubewarden.CertOidcIssuer)
	}
	if !strings.Contains(kubewarden.CertIdentityRegexp, "kubewarden/audit-scanner") {
		t.Errorf("CertIdentityRegexp = %q", kubewarden.CertIdentityRegexp)
	}
}

func TestParseFilePipelineFixture(t *testing.T) {
	f, err := ParseFile(fixturePipeline)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}
	if got, want := len(f.Documents), 3; got != want {
		t.Fatalf("len(Documents) = %d, want %d", got, want)
	}

	charts := f.Documents[1]
	if got, want := len(charts.Entries), 9; got != want {
		t.Fatalf("len(charts.Entries) = %d, want %d", got, want)
	}

	// The local .tgz chart exercises the boolean and list fields.
	var withDeps *Chart
	for _, e := range charts.Entries {
		if e.Chart != nil && e.Chart.Name == "chart-with-file-dependency-chart-1.0.0.tgz" {
			withDeps = e.Chart
		}
	}
	if withDeps == nil {
		t.Fatal("did not find chart-with-file-dependency-chart-1.0.0.tgz")
	}
	if !withDeps.AddDependencies {
		t.Error("AddDependencies = false, want true (json tag `add-dependencies`)")
	}
	if !withDeps.AddImages {
		t.Error("AddImages = false, want true (json tag `add-images`)")
	}
	if got, want := len(withDeps.ValuesFiles), 2; got != want {
		t.Errorf("len(ValuesFiles) = %d, want %d", got, want)
	}
}

// This is the load-bearing test for the whole passthrough design. A scoped
// manifest must reproduce the operator's cosign configuration exactly; a
// dropped certificate-identity-regexp silently downgrades a verified pull.
func TestScopePreservesCosignFieldsVerbatim(t *testing.T) {
	f, err := ParseFile(fixtureBasic)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}

	const target = "ghcr.io/kubewarden/audit-scanner:v1.30.0-rc1"
	out, ok, err := f.Scope(func(e Entry) bool {
		return e.Kind == EntryImage && e.Ref() == target
	})
	if err != nil {
		t.Fatalf("Scope() error = %v", err)
	}
	if !ok {
		t.Fatal("Scope() kept nothing, want one image")
	}

	// Re-parse the generated manifest and compare the typed entry against the
	// original. Comparing structs rather than bytes is the assertion that
	// matters: hauler reads the file, not the formatting.
	reparsed, err := Parse(strings.NewReader(string(out)), "scoped")
	if err != nil {
		t.Fatalf("re-parsing scoped output failed: %v\n---\n%s", err, out)
	}

	got := reparsed.Images()
	if len(got) != 1 {
		t.Fatalf("scoped manifest has %d images, want 1\n---\n%s", len(got), out)
	}

	var orig *Image
	for _, e := range f.Images() {
		if e.Ref() == target {
			orig = e.Image
		}
	}
	if *got[0].Image != *orig {
		t.Errorf("scoped image differs from source\n got: %+v\nwant: %+v\n---\n%s", *got[0].Image, *orig, out)
	}

	// The scoped document must keep its identity, or hauler logs a manifest
	// the operator cannot correlate with anything in their repo.
	if reparsed.Documents[0].Name != f.Documents[0].Name {
		t.Errorf("metadata.name = %q, want %q", reparsed.Documents[0].Name, f.Documents[0].Name)
	}
	if reparsed.Documents[0].APIVersion != APIVersionContent {
		t.Errorf("apiVersion = %q", reparsed.Documents[0].APIVersion)
	}

	// And the other three images must be gone.
	if strings.Contains(string(out), "busybox") {
		t.Errorf("scoped manifest leaked an unselected image:\n%s", out)
	}
}

func TestScopeDropsEmptyDocumentsAndReportsNothingKept(t *testing.T) {
	f, err := ParseFile(fixtureBasic)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}

	// Keep only images: the Charts and Files documents must not appear.
	out, ok, err := f.Scope(func(e Entry) bool { return e.Kind == EntryImage })
	if err != nil {
		t.Fatalf("Scope() error = %v", err)
	}
	if !ok {
		t.Fatal("Scope() kept nothing, want the images document")
	}
	if strings.Contains(string(out), "kind: Charts") || strings.Contains(string(out), "kind: Files") {
		t.Errorf("scoped manifest should contain only the Images document:\n%s", out)
	}

	// Keeping nothing must be reported, not rendered as an empty manifest --
	// `hauler store sync` on an empty file is a confusing no-op.
	_, ok, err = f.Scope(func(Entry) bool { return false })
	if err != nil {
		t.Fatalf("Scope() error = %v", err)
	}
	if ok {
		t.Error("Scope() with no matches returned ok = true, want false")
	}
}

func TestScopeAcrossMultipleDocumentsOfSameKind(t *testing.T) {
	const src = `
apiVersion: content.hauler.cattle.io/v1
kind: Images
metadata:
  name: first
spec:
  images:
    - name: alpine:3.20
    - name: busybox:stable
---
apiVersion: content.hauler.cattle.io/v1
kind: Images
metadata:
  name: second
spec:
  images:
    - name: nginx:1.27
`
	f, err := Parse(strings.NewReader(src), "inline")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	out, ok, err := f.Scope(func(e Entry) bool {
		return e.Ref() == "busybox:stable" || e.Ref() == "nginx:1.27"
	})
	if err != nil || !ok {
		t.Fatalf("Scope() = ok %v, err %v", ok, err)
	}

	reparsed, err := Parse(strings.NewReader(string(out)), "scoped")
	if err != nil {
		t.Fatalf("re-parse error = %v\n---\n%s", err, out)
	}
	if got, want := len(reparsed.Documents), 2; got != want {
		t.Fatalf("len(Documents) = %d, want %d\n---\n%s", got, want, out)
	}
	if got, want := len(reparsed.Documents[0].Entries), 1; got != want {
		t.Errorf("first doc kept %d entries, want %d", got, want)
	}
	if got := reparsed.Documents[0].Name; got != "first" {
		t.Errorf("first doc name = %q, want %q", got, "first")
	}
	if got := reparsed.Documents[1].Name; got != "second" {
		t.Errorf("second doc name = %q, want %q", got, "second")
	}
}

func TestScopeIsRepeatableAndNonMutating(t *testing.T) {
	f, err := ParseFile(fixtureBasic)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}

	keep := func(e Entry) bool { return e.Kind == EntryImage && strings.Contains(e.Ref(), "busybox") }

	first, _, err := f.Scope(keep)
	if err != nil {
		t.Fatalf("first Scope() error = %v", err)
	}
	second, _, err := f.Scope(keep)
	if err != nil {
		t.Fatalf("second Scope() error = %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("Scope() is not repeatable -- the source tree was mutated\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	// Scoping must not have disturbed the parsed entries either.
	if got, want := len(f.Images()), 4; got != want {
		t.Errorf("len(Images()) after scoping = %d, want %d", got, want)
	}
}

func TestSpecHashChangesWithAnyField(t *testing.T) {
	parseOne := func(t *testing.T, src string) Entry {
		t.Helper()
		f, err := Parse(strings.NewReader(src), "inline")
		if err != nil {
			t.Fatalf("Parse() error = %v", err)
		}
		imgs := f.Images()
		if len(imgs) != 1 {
			t.Fatalf("expected 1 image, got %d", len(imgs))
		}
		return imgs[0]
	}

	const base = `
apiVersion: content.hauler.cattle.io/v1
kind: Images
metadata:
  name: t
spec:
  images:
    - name: alpine:3.20
`
	const withPlatform = `
apiVersion: content.hauler.cattle.io/v1
kind: Images
metadata:
  name: t
spec:
  images:
    - name: alpine:3.20
      platform: linux/amd64
`
	const withCosign = `
apiVersion: content.hauler.cattle.io/v1
kind: Images
metadata:
  name: t
spec:
  images:
    - name: alpine:3.20
      certificate-oidc-issuer: https://token.actions.githubusercontent.com
`

	h1, err := parseOne(t, base).SpecHash()
	if err != nil {
		t.Fatal(err)
	}
	h2, err := parseOne(t, withPlatform).SpecHash()
	if err != nil {
		t.Fatal(err)
	}
	h3, err := parseOne(t, withCosign).SpecHash()
	if err != nil {
		t.Fatal(err)
	}

	if h1 == h2 {
		t.Error("adding a platform did not change the spec hash")
	}
	// The reference is identical here; only verification config changed. If
	// this hash collided, a tightened cosign policy would never be re-applied.
	if h1 == h3 {
		t.Error("adding a cosign issuer did not change the spec hash")
	}
	if h2 == h3 {
		t.Error("two different specs produced the same hash")
	}

	// And it must be stable across parses of identical input.
	h1again, err := parseOne(t, base).SpecHash()
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h1again {
		t.Errorf("spec hash is not stable: %s vs %s", h1, h1again)
	}
}

func TestParseRejectsBadDocuments(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "missing apiVersion",
			src:  "kind: Images\nspec:\n  images:\n    - name: alpine\n",
			want: "apiVersion",
		},
		{
			name: "missing kind",
			src:  "apiVersion: content.hauler.cattle.io/v1\nspec: {}\n",
			want: "kind",
		},
		{
			name: "wrong group",
			src:  "apiVersion: apps/v1\nkind: Images\nspec: {}\n",
			want: "unrecognized apiVersion",
		},
		{
			// v2.0.0 removed v1alpha; accepting it silently would produce a
			// manifest hauler then rejects, deep inside a run.
			name: "legacy v1alpha1",
			src:  "apiVersion: content.hauler.cattle.io/v1alpha1\nkind: Images\nspec: {}\n",
			want: "unrecognized apiVersion",
		},
		{
			name: "unsupported kind",
			src:  "apiVersion: content.hauler.cattle.io/v1\nkind: Widgets\nspec: {}\n",
			want: "unsupported kind",
		},
		{
			name: "image without a name",
			src:  "apiVersion: content.hauler.cattle.io/v1\nkind: Images\nspec:\n  images:\n    - platform: linux/amd64\n",
			want: "missing required field [name]",
		},
		{
			name: "file without a path",
			src:  "apiVersion: content.hauler.cattle.io/v1\nkind: Files\nspec:\n  files:\n    - name: install.sh\n",
			want: "missing required field [path]",
		},
		{
			name: "top level sequence",
			src:  "- name: alpine\n",
			want: "expected a mapping",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.src), "inline")
			if err == nil {
				t.Fatalf("Parse() error = nil, want one containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse() error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestParseAcceptsEmptyAndCollectionDocuments(t *testing.T) {
	const src = `---
apiVersion: content.hauler.cattle.io/v1
kind: Images
metadata:
  name: empty-is-fine
spec:
  images: []
---
apiVersion: collection.hauler.cattle.io/v1
kind: Images
metadata:
  name: collection
spec:
  images:
    - name: alpine:3.20
`
	f, err := Parse(strings.NewReader(src), "inline")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got, want := len(f.Documents), 2; got != want {
		t.Fatalf("len(Documents) = %d, want %d", got, want)
	}
	if got := len(f.Documents[0].Entries); got != 0 {
		t.Errorf("empty spec produced %d entries, want 0", got)
	}
	if got, want := len(f.Images()), 1; got != want {
		t.Errorf("len(Images()) = %d, want %d", got, want)
	}
}

// Comments carry operator intent ("pinned for CVE-2024-x"). Losing them in a
// scoped manifest makes the generated file harder to debug when a sync fails.
func TestScopePreservesComments(t *testing.T) {
	const src = `apiVersion: content.hauler.cattle.io/v1
kind: Images
metadata:
  name: t
spec:
  images:
    # pinned deliberately -- do not bump
    - name: alpine:3.20
    - name: busybox:stable
`
	f, err := Parse(strings.NewReader(src), "inline")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	out, ok, err := f.Scope(func(e Entry) bool { return e.Ref() == "alpine:3.20" })
	if err != nil || !ok {
		t.Fatalf("Scope() = ok %v, err %v", ok, err)
	}
	if !strings.Contains(string(out), "do not bump") {
		t.Errorf("scoped manifest dropped the entry comment:\n%s", out)
	}
}

func TestEntryRawYAMLRoundTrips(t *testing.T) {
	f, err := ParseFile(fixtureBasic)
	if err != nil {
		t.Fatalf("ParseFile() error = %v", err)
	}
	for _, e := range f.Images() {
		raw, err := e.RawYAML()
		if err != nil {
			t.Fatalf("RawYAML() error = %v", err)
		}
		var back Image
		if err := yaml.Unmarshal([]byte(raw), &struct{}{}); err != nil {
			t.Fatalf("RawYAML() produced invalid yaml for %q: %v\n%s", e.Ref(), err, raw)
		}
		// Decode through the same JSON-tag path the parser uses.
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(raw), &node); err != nil {
			t.Fatalf("re-parsing RawYAML for %q: %v", e.Ref(), err)
		}
		if err := decodeJSONish(documentRoot(&node), &back); err != nil {
			t.Fatalf("decoding RawYAML for %q: %v", e.Ref(), err)
		}
		if back != *e.Image {
			t.Errorf("RawYAML round-trip changed %q\n got: %+v\nwant: %+v", e.Ref(), back, *e.Image)
		}
	}
}
