package reconcile

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Drblanco24/hauler-web/internal/db"
	"github.com/Drblanco24/hauler-web/internal/gitsource"
)

// repoFor builds the git checkout for a source. Each source gets its own
// directory under the work dir, keyed by name, so two sources never fight over
// one checkout.
func (r *Reconciler) repoFor(src db.Source) *gitsource.Repo {
	return &gitsource.Repo{
		URL:    src.URL,
		Branch: src.Branch,
		Dir:    filepath.Join(r.WorkDir, "sources", sanitize(src.Name)),
	}
}

// sanitize makes a source name safe as a single path segment.
func sanitize(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "source"
	}
	return b.String()
}

// registryTargetRef renders a registry target's config as the `hauler store
// copy` destination, plus its transport flags.
//
// The url may be given with or without a scheme and with or without a project;
// operators write both, and guessing wrong produces a confusing push to the
// registry root.
func registryTargetRef(t db.Target) (target string, insecure, plainHTTP bool) {
	url := configString(t.Config, "url")
	if url == "" {
		return "", false, false
	}
	insecure = configBool(t.Config, "insecure")
	plainHTTP = configBool(t.Config, "plain_http")

	url = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://"), "/")
	if project := strings.Trim(configString(t.Config, "project"), "/"); project != "" {
		url = url + "/" + project
	}
	return "registry://" + url, insecure, plainHTTP
}

func configString(cfg map[string]any, key string) string {
	if cfg == nil {
		return ""
	}
	if v, ok := cfg[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func configBool(cfg map[string]any, key string) bool {
	if cfg == nil {
		return false
	}
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

// archiveFilename names a haul for a commit. The commit is in the name so an
// operator can tell which manifest revision an archive corresponds to without
// consulting the database.
func archiveFilename(commitSHA string) string {
	return fmt.Sprintf("haul-%s.tar.zst", short(commitSHA))
}
