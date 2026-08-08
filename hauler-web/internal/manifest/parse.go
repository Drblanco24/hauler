package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// EntryKind identifies which spec list an Entry came from.
type EntryKind string

const (
	EntryImage EntryKind = "image"
	EntryChart EntryKind = "chart"
	EntryFile  EntryKind = "file"
)

// Entry is one element of a manifest's spec list, held in two forms at once.
//
// The typed view (Image/Chart/File) drives planning. The Node is the
// operator's original YAML, kept so that a scoped manifest can re-emit it
// byte-for-byte. Never rebuild an entry from the typed view: hauler's Image
// carries seven cosign verification fields today and may carry more after any
// upstream release, and silently dropping one would turn a verified pull into
// an unverified one.
type Entry struct {
	Kind  EntryKind
	Image *Image
	Chart *Chart
	File  *File

	// Node is the original sequence element from the source document.
	Node *yaml.Node

	// DocIndex is the 0-based index of the document this entry came from
	// within its file.
	DocIndex int

	// Index is the 0-based position of this entry within its spec list.
	Index int
}

// Ref is the entry's primary identifier: an image reference, a chart name, or
// a file path.
func (e Entry) Ref() string {
	switch {
	case e.Image != nil:
		return e.Image.Name
	case e.Chart != nil:
		return e.Chart.Name
	case e.File != nil:
		return e.File.Path
	default:
		return ""
	}
}

// Platform is the entry's requested platform, or "" for "all platforms".
func (e Entry) Platform() string {
	switch {
	case e.Image != nil:
		return e.Image.Platform
	case e.Chart != nil:
		return e.Chart.Platform
	default:
		return ""
	}
}

// RawYAML renders the entry's original node. This is what gets persisted to
// desired_items.raw_yaml, and what a scoped manifest re-emits.
func (e Entry) RawYAML() (string, error) {
	if e.Node == nil {
		return "", fmt.Errorf("entry %q has no source node", e.Ref())
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(e.Node); err != nil {
		return "", fmt.Errorf("rendering entry %q: %w", e.Ref(), err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("rendering entry %q: %w", e.Ref(), err)
	}
	return buf.String(), nil
}

// SpecHash is a stable digest of the entry's declared spec. A change to any
// field -- including a cosign identity regexp that does not affect the
// reference -- produces a different hash, which is what tells the planner the
// entry must be re-processed even though its ref is unchanged.
func (e Entry) SpecHash() (string, error) {
	raw, err := e.RawYAML()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:]), nil
}

// Document is one YAML document from a manifest file.
type Document struct {
	APIVersion  string
	Kind        string
	Name        string
	Annotations map[string]string

	// Index is the 0-based position of this document within its file.
	Index int

	// Entries are the document's spec list, in source order.
	Entries []Entry

	// node is the document's root mapping node, retained for scoped output.
	node *yaml.Node
}

// Manifest is a parsed manifest file. It is named Manifest rather than File
// because File is already hauler's content type for a fetched file.
type Manifest struct {
	Path      string
	Documents []Document
}

// Images returns every image entry across all documents in the file.
func (f *Manifest) Images() []Entry {
	var out []Entry
	for _, d := range f.Documents {
		for _, e := range d.Entries {
			if e.Kind == EntryImage {
				out = append(out, e)
			}
		}
	}
	return out
}

// ParseFile reads and parses a manifest from disk.
func ParseFile(path string) (*Manifest, error) {
	fi, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fi.Close()
	return Parse(fi, path)
}

// Parse reads a multi-document manifest. path is used only for error messages.
//
// Documents whose apiVersion is not a recognised hauler group are rejected
// rather than skipped: a typo'd apiVersion in a GitOps repo means images the
// operator believes are being mirrored silently are not, which is exactly the
// failure this system exists to prevent.
func Parse(r io.Reader, path string) (*Manifest, error) {
	out := &Manifest{Path: path}
	dec := yaml.NewDecoder(r)

	for i := 0; ; i++ {
		var node yaml.Node
		err := dec.Decode(&node)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", path, i, err)
		}

		root := documentRoot(&node)
		if root == nil {
			// An empty document (a stray `---`) is not an error.
			continue
		}

		doc, err := parseDocument(root, i)
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", path, i, err)
		}
		out.Documents = append(out.Documents, *doc)
	}

	return out, nil
}

func parseDocument(root *yaml.Node, index int) (*Document, error) {
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected a mapping at the top level, got %s", nodeKindName(root.Kind))
	}

	var hdr docHeader
	if err := decodeJSONish(root, &hdr); err != nil {
		return nil, err
	}

	if hdr.APIVersion == "" {
		return nil, fmt.Errorf("missing required manifest field [apiVersion]")
	}
	if hdr.Kind == "" {
		return nil, fmt.Errorf("missing required manifest field [kind]")
	}
	if hdr.APIVersion != APIVersionContent && hdr.APIVersion != APIVersionCollection {
		return nil, fmt.Errorf("unrecognized apiVersion [%s]... valid versions are [%s, %s]",
			hdr.APIVersion, APIVersionContent, APIVersionCollection)
	}

	doc := &Document{
		APIVersion:  hdr.APIVersion,
		Kind:        hdr.Kind,
		Name:        hdr.Metadata.Name,
		Annotations: hdr.Metadata.Annotations,
		Index:       index,
		node:        root,
	}

	var specKey string
	var count int
	switch hdr.Kind {
	case KindImages:
		specKey, count = "images", len(hdr.Spec.Images)
	case KindCharts:
		specKey, count = "charts", len(hdr.Spec.Charts)
	case KindFiles:
		specKey, count = "files", len(hdr.Spec.Files)
	default:
		return nil, fmt.Errorf("unsupported kind [%s]... valid kinds are [Images, Charts, Files]", hdr.Kind)
	}

	seq := specSequence(root, specKey)
	if seq == nil {
		// A kind with an empty or absent spec list is valid -- an operator
		// commenting out every image should not break the reconcile.
		return doc, nil
	}
	if len(seq.Content) != count {
		return nil, fmt.Errorf("internal: decoded %d spec.%s entries but found %d yaml nodes",
			count, specKey, len(seq.Content))
	}

	for i, n := range seq.Content {
		e := Entry{Node: n, DocIndex: index, Index: i}
		switch hdr.Kind {
		case KindImages:
			img := hdr.Spec.Images[i]
			e.Kind, e.Image = EntryImage, &img
			if img.Name == "" {
				return nil, fmt.Errorf("spec.images[%d]: missing required field [name]", i)
			}
		case KindCharts:
			ch := hdr.Spec.Charts[i]
			e.Kind, e.Chart = EntryChart, &ch
			if ch.Name == "" {
				return nil, fmt.Errorf("spec.charts[%d]: missing required field [name]", i)
			}
		case KindFiles:
			f := hdr.Spec.Files[i]
			e.Kind, e.File = EntryFile, &f
			if f.Path == "" {
				return nil, fmt.Errorf("spec.files[%d]: missing required field [path]", i)
			}
		}
		doc.Entries = append(doc.Entries, e)
	}

	return doc, nil
}

// Scope renders a manifest containing only the entries for which keep returns
// true, preserving each entry's original YAML and each document's metadata and
// annotations. Documents left with no entries are omitted entirely; if nothing
// is kept, Scope returns an empty slice and false.
//
// The result is what gets handed to `hauler store sync -f`.
func (f *Manifest) Scope(keep func(Entry) bool) ([]byte, bool, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)

	wrote := false
	for _, doc := range f.Documents {
		var kept []*yaml.Node
		for _, e := range doc.Entries {
			if keep(e) {
				kept = append(kept, e.Node)
			}
		}
		if len(kept) == 0 {
			continue
		}

		clone := cloneNode(doc.node)
		seq := specSequence(clone, specKeyForKind(doc.Kind))
		if seq == nil {
			return nil, false, fmt.Errorf("document %d: cannot locate spec.%s to scope",
				doc.Index, specKeyForKind(doc.Kind))
		}
		// The kept nodes come from the original tree, not the clone. That is
		// intentional and safe: they are only read during encoding, and using
		// them directly guarantees the output is byte-identical to the source.
		seq.Content = kept

		if err := enc.Encode(clone); err != nil {
			return nil, false, fmt.Errorf("encoding document %d: %w", doc.Index, err)
		}
		wrote = true
	}

	// Close before the wrote check would fail: yaml.v3 rejects Close on an
	// encoder that never emitted a document ("expected STREAM-START").
	if !wrote {
		return nil, false, nil
	}
	if err := enc.Close(); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), true, nil
}

func specKeyForKind(kind string) string {
	switch kind {
	case KindImages:
		return "images"
	case KindCharts:
		return "charts"
	case KindFiles:
		return "files"
	default:
		return ""
	}
}

// documentRoot unwraps a DocumentNode to its single content node. Decoding
// into a *yaml.Node yields a DocumentNode, but being tolerant of a bare
// mapping keeps the helper usable on hand-built nodes in tests.
func documentRoot(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		return n.Content[0]
	}
	if n.Kind == 0 {
		return nil
	}
	return n
}

// specSequence locates spec.<key> and returns its sequence node, or nil.
func specSequence(root *yaml.Node, key string) *yaml.Node {
	if key == "" {
		return nil
	}
	spec := mapValue(root, "spec")
	if spec == nil || spec.Kind != yaml.MappingNode {
		return nil
	}
	seq := mapValue(spec, key)
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	return seq
}

// mapValue returns the value node for key in a mapping node.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	// Mapping content alternates key, value, key, value...
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func cloneNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	c := *n
	c.Content = nil
	if len(n.Content) > 0 {
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			c.Content[i] = cloneNode(child)
		}
	}
	if n.Alias != nil {
		c.Alias = cloneNode(n.Alias)
	}
	return &c
}

// decodeJSONish decodes a YAML node using encoding/json semantics, so that
// `json` struct tags, case-insensitive field matching, and unknown-field
// tolerance behave exactly as they do inside hauler (which decodes through
// k8s.io/apimachinery/pkg/util/yaml, itself a YAML->JSON->encoding/json
// pipeline).
//
// Doing this with the yaml.v3 tags instead would diverge on every hyphenated
// field -- `use-tlog-verify`, `exclude-extras`, `add-images` -- because yaml.v3
// lowercases Go field names by default and would look for `tlog`, not
// `use-tlog-verify`.
func decodeJSONish(node *yaml.Node, out any) error {
	var intermediate any
	if err := node.Decode(&intermediate); err != nil {
		return err
	}
	// yaml.v3 decodes mappings into map[string]any when every key is a
	// string, so this marshals cleanly. Non-string keys are not valid in a
	// hauler manifest and surface here as a json error.
	buf, err := json.Marshal(intermediate)
	if err != nil {
		return fmt.Errorf("converting yaml to json: %w", err)
	}
	if err := json.Unmarshal(buf, out); err != nil {
		return fmt.Errorf("decoding manifest: %w", err)
	}
	return nil
}

func nodeKindName(k yaml.Kind) string {
	switch k {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	default:
		return "unknown"
	}
}

// NormalizeRef trims whitespace from a reference. Manifests are hand-edited
// and a trailing space on an image name produces a confusing registry error
// several layers down.
func NormalizeRef(s string) string { return strings.TrimSpace(s) }
