package hauler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Artifact is one entry of `hauler store info -o json`, mirroring the `item`
// struct in the hauler CLI's cmd/hauler/cli/store/info.go.
type Artifact struct {
	Reference string   `json:"reference"`
	Type      string   `json:"type"`
	Platform  string   `json:"platform"`
	Digest    string   `json:"digest,omitempty"`
	Layers    int      `json:"layers"`
	Size      int64    `json:"size"`
	Problems  []string `json:"problems,omitempty"`
}

// Artifact type values hauler emits.
const (
	TypeImage    = "image"
	TypeChart    = "chart"
	TypeFile     = "file"
	TypeSigs     = "sigs"
	TypeAtts     = "atts"
	TypeSbom     = "sbom"
	TypeReferrer = "referrer"
)

// StoreInfo is the top level of `hauler store info -o json`.
type StoreInfo struct {
	StorePath string     `json:"store-path"`
	StoreID   string     `json:"store-id"`
	Artifacts []Artifact `json:"artifacts"`
}

// Images returns only the image artifacts, excluding the cosign signatures,
// attestations, SBOMs, and OCI referrers that hauler stores alongside them.
// Those are real store contents but they are not things an operator declared
// in a manifest, so they must not appear as rows in the images table.
func (s *StoreInfo) Images() []Artifact {
	var out []Artifact
	for _, a := range s.Artifacts {
		if a.Type == TypeImage {
			out = append(out, a)
		}
	}
	return out
}

// HasDigest reports whether the store already holds an artifact with this
// digest. This is the local half of the dedupe check: a hit means the pull can
// be skipped even though a push may still be needed.
func (s *StoreInfo) HasDigest(digest string) bool {
	if digest == "" {
		return false
	}
	for _, a := range s.Artifacts {
		if a.Digest == digest {
			return true
		}
	}
	return false
}

// Problems returns every artifact hauler flagged as corrupt.
func (s *StoreInfo) Problems() []Artifact {
	var out []Artifact
	for _, a := range s.Artifacts {
		if len(a.Problems) > 0 {
			out = append(out, a)
		}
	}
	return out
}

// InfoOptions mirrors the subset of `hauler store info` flags used here.
type InfoOptions struct {
	// Check hashes every blob to verify store integrity. Slow on large
	// stores, so it is opt-in rather than part of the normal read path.
	Check bool
}

// Info reads the store inventory.
//
// hauler's logger writes to stdout, the same stream the JSON is printed on, so
// this runs at LogLevelSilent and additionally extracts the JSON object
// defensively. Relying on the log level alone would make parsing depend on a
// setting any future hauler release could reinterpret.
func (c *Client) Info(ctx context.Context, o InfoOptions) (*StoreInfo, error) {
	args := []string{"store", "info", "--output", "json", "--digests"}
	if o.Check {
		args = append(args, "--check")
	}

	res, err := c.runWithLogLevel(ctx, c.withStoreFlags(args), LogLevelSilent, nil)
	if err != nil {
		return nil, err
	}

	info, perr := parseStoreInfo(res.Stdout)
	if perr != nil {
		return nil, fmt.Errorf("parsing `hauler store info` output: %w", perr)
	}
	return info, nil
}

// InfoIfExists behaves like Info but returns an empty inventory when the store
// has no index yet. A first run against a fresh volume is the normal case, not
// an error.
func (c *Client) InfoIfExists(ctx context.Context, o InfoOptions) (*StoreInfo, error) {
	info, err := c.Info(ctx, o)
	if err == nil {
		return info, nil
	}
	var herr *Error
	if errors.As(err, &herr) && isEmptyStore(herr.Result) {
		return &StoreInfo{StorePath: c.StoreDir, Artifacts: nil}, nil
	}
	return nil, err
}

// isEmptyStore recognises hauler's "no index yet" failure. hauler exits
// non-zero with a message rather than emitting an empty inventory, so the
// string is the only signal available; it is matched loosely and the caller
// still falls through to a real error if it does not match.
func isEmptyStore(res *Result) bool {
	if res == nil {
		return false
	}
	combined := strings.ToLower(res.Stdout + "\n" + res.Stderr)
	return strings.Contains(combined, "store index not found") ||
		strings.Contains(combined, "no such file or directory") ||
		strings.Contains(combined, "index.json")
}

// parseStoreInfo pulls the JSON document out of hauler's stdout.
//
// `store info -o json` prints with json.MarshalIndent followed by fmt.Println,
// so the object always begins with a line that is exactly "{" and ends with a
// line that is exactly "}". Log lines, when not suppressed, are prefixed by a
// timestamp and never match either. Scanning for those anchors is resilient to
// stray output on either side.
func parseStoreInfo(stdout string) (*StoreInfo, error) {
	body, err := extractJSONObject(stdout)
	if err != nil {
		return nil, err
	}
	var info StoreInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		return nil, fmt.Errorf("%w (in %q)", err, truncate(body, 200))
	}
	return &info, nil
}

func extractJSONObject(s string) (string, error) {
	lines := strings.Split(s, "\n")

	start := -1
	for i, ln := range lines {
		if strings.TrimRight(ln, "\r") == "{" {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("no json object found in output %q", truncate(s, 200))
	}

	// Take the last closing brace at column zero: nested objects are indented
	// by MarshalIndent, so only the document's own terminator matches.
	end := -1
	for i := len(lines) - 1; i > start; i-- {
		if strings.TrimRight(lines[i], "\r") == "}" {
			end = i
			break
		}
	}
	if end < 0 {
		return "", fmt.Errorf("json object in output is unterminated")
	}

	return strings.Join(lines[start:end+1], "\n"), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Version reports the hauler binary's version. Every run records it, so a
// behaviour change after an upgrade can be correlated with the runs it
// affected.
func (c *Client) Version(ctx context.Context) (string, error) {
	res, err := c.runWithLogLevel(ctx, []string{"version", "--json"}, LogLevelSilent, nil)
	if err != nil {
		return "", err
	}
	body, jerr := extractJSONObject(res.Stdout)
	if jerr != nil {
		return "", fmt.Errorf("parsing `hauler version --json` output: %w", jerr)
	}
	var v struct {
		GitVersion string `json:"gitVersion"`
		GitCommit  string `json:"gitCommit"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return "", fmt.Errorf("parsing `hauler version --json` output: %w", err)
	}
	if v.GitVersion == "" {
		return "", errors.New("`hauler version --json` reported no gitVersion")
	}
	return v.GitVersion, nil
}
