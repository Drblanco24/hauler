// Package registryclient resolves image references to digests without
// transferring image data.
//
// This is what makes "don't pull again" cheap: a HEAD against the manifest
// endpoint costs one round trip and tells us whether a mutable tag still
// points where it did last run. Only references whose digest actually moved
// get handed to hauler.
package registryclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"golang.org/x/sync/errgroup"
)

// Client resolves references against upstream registries.
type Client struct {
	// Keychain supplies registry credentials. Defaults to
	// authn.DefaultKeychain, which reads the docker config pointed at by
	// DOCKER_CONFIG -- the same mechanism the hauler subprocess uses, so
	// both halves authenticate identically.
	Keychain authn.Keychain

	// Insecure allows plain HTTP and skips TLS verification. Airgap
	// environments run internal registries with private CAs often enough
	// that this needs to be reachable, but it is off by default.
	Insecure bool

	// Concurrency bounds in-flight resolutions. Zero uses
	// DefaultConcurrency.
	Concurrency int
}

// DefaultConcurrency bounds parallel HEAD requests. Registries rate-limit, and
// a manifest with a thousand images should not look like an attack.
const DefaultConcurrency = 8

func (c *Client) keychain() authn.Keychain {
	if c.Keychain != nil {
		return c.Keychain
	}
	return authn.DefaultKeychain
}

func (c *Client) concurrency() int {
	if c.Concurrency > 0 {
		return c.Concurrency
	}
	return DefaultConcurrency
}

// Reference is a reference to resolve, with the platform it was requested for.
type Reference struct {
	Ref      string
	Platform string
}

// Key matches planner.Desired.Key so results can be looked up directly.
func (r Reference) Key() string { return r.Ref + "|" + r.Platform }

// Resolution is the outcome for one reference.
type Resolution struct {
	Reference Reference

	// Digest is the manifest digest the reference currently points at. For a
	// reference that names a platform, this is still the index digest --
	// hauler resolves the per-platform manifest itself, and tracking the
	// index digest is what makes "has the tag moved?" answerable.
	Digest string

	// Err is set when resolution failed. One unreachable registry must not
	// fail the whole plan, so failures are returned per reference rather
	// than aborting.
	Err error
}

// Resolve looks up every reference concurrently. The returned maps are keyed
// by Reference.Key() and are shaped for planner.Observed.
func (c *Client) Resolve(ctx context.Context, refs []Reference) (digests map[string]string, failures map[string]error) {
	digests = make(map[string]string, len(refs))
	failures = make(map[string]error, len(refs))
	if len(refs) == 0 {
		return digests, failures
	}

	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(c.concurrency())

	for _, ref := range refs {
		g.Go(func() error {
			digest, err := c.ResolveOne(gctx, ref)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures[ref.Key()] = err
			} else {
				digests[ref.Key()] = digest
			}
			// Never propagate: a per-reference failure is data, not a reason
			// to cancel every other lookup.
			return nil
		})
	}
	// The goroutines never return an error, so this only surfaces a
	// cancelled context.
	_ = g.Wait()

	return digests, failures
}

// ResolveOne returns the digest a single reference points at.
func (c *Client) ResolveOne(ctx context.Context, ref Reference) (string, error) {
	if strings.TrimSpace(ref.Ref) == "" {
		return "", errors.New("empty reference")
	}

	parsed, err := name.ParseReference(ref.Ref, c.parseOptions()...)
	if err != nil {
		return "", fmt.Errorf("parsing reference %q: %w", ref.Ref, err)
	}

	// A digest reference cannot drift, so there is nothing to ask the
	// registry. Skipping the round trip also means a fully digest-pinned
	// manifest reconciles with no network access at all -- which matters
	// when hauler-web itself runs inside the airgap.
	if d, ok := parsed.(name.Digest); ok {
		return d.DigestStr(), nil
	}

	desc, err := remote.Head(parsed,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(c.keychain()),
	)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", ref.Ref, err)
	}
	return desc.Digest.String(), nil
}

// Exists reports whether a digest is present in a repository. This is the
// generic destination check used before the Harbor API integration lands, and
// it keeps working for any OCI registry afterwards.
func (c *Client) Exists(ctx context.Context, repository, digest string) (bool, error) {
	repo, err := name.NewRepository(repository, c.parseOptions()...)
	if err != nil {
		return false, fmt.Errorf("parsing repository %q: %w", repository, err)
	}
	hash, err := v1.NewHash(digest)
	if err != nil {
		return false, fmt.Errorf("parsing digest %q: %w", digest, err)
	}

	_, err = remote.Head(repo.Digest(hash.String()),
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(c.keychain()),
	)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking %s@%s: %w", repository, digest, err)
	}
	return true, nil
}

func (c *Client) parseOptions() []name.Option {
	if c.Insecure {
		return []name.Option{name.Insecure}
	}
	return nil
}

// isNotFound distinguishes "the registry says no" from "we could not ask".
// Treating a network error as absence would cause an endless re-push loop.
func isNotFound(err error) bool {
	var terr *transportError
	if errors.As(err, &terr) {
		return terr.NotFound()
	}
	// go-containerregistry surfaces registry errors through its transport
	// package; match on the documented status text as a fallback.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "manifest_unknown") ||
		strings.Contains(msg, "name_unknown") ||
		strings.Contains(msg, "status code 404")
}

// transportError is an interface assertion helper for errors that carry an
// HTTP status.
type transportError struct {
	code int
}

func (e *transportError) Error() string { return fmt.Sprintf("status %d", e.code) }
func (e *transportError) NotFound() bool {
	return e.code == 404
}

// SplitRepository returns the repository portion of a reference, without its
// tag or digest. Used to key the destination inventory.
func SplitRepository(ref string) (string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("parsing reference %q: %w", ref, err)
	}
	return parsed.Context().Name(), nil
}

// SplitTag returns the tag portion of a reference, or "" for a digest
// reference.
func SplitTag(ref string) string {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return ""
	}
	if t, ok := parsed.(name.Tag); ok {
		return t.TagStr()
	}
	return ""
}
