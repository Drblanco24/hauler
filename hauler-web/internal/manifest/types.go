// Package manifest reads hauler content manifests
// (apiVersion: content.hauler.cattle.io/v1) and writes back scoped subsets of
// them.
//
// The types below are a deliberate copy of the hauler CLI's
// pkg/apis/hauler.cattle.io/v1 package rather than an import. Importing them
// would drag hauler's entire transitive graph -- helm v4, cosign v3,
// containerd v2, apimachinery -- into a service whose only interaction with
// hauler is exec'ing its binary. The v1 content API is stable (hauler v2.0.0
// removed v1alpha and its conversion shims), so a copy is cheap to keep
// current.
//
// Field tags are `json`, matching the originals, because hauler decodes
// manifests through k8s.io/apimachinery/pkg/util/yaml -- which converts YAML
// to JSON and then uses encoding/json. Parsing here goes through the same
// YAML->JSON->struct path (see decodeJSONish) so that field-name matching,
// case-insensitivity, and unknown-field tolerance behave exactly as they do
// inside hauler. Anything hauler accepts, hauler-web must also accept.
package manifest

// Group and version strings recognised in a manifest's apiVersion.
// Mirrors hauler's pkg/consts.
const (
	ContentGroup    = "content.hauler.cattle.io"
	CollectionGroup = "collection.hauler.cattle.io"
	Version         = "v1"

	APIVersionContent    = ContentGroup + "/" + Version
	APIVersionCollection = CollectionGroup + "/" + Version
)

// Content kinds. Mirrors hauler's pkg/consts.
const (
	KindImages = "Images"
	KindCharts = "Charts"
	KindFiles  = "Files"
)

// Image mirrors hauler's v1.Image.
//
// Note the non-omitempty tags on Key, Tlog, and the certificate fields: they
// are copied verbatim from upstream. hauler-web never re-serialises this
// struct back into a manifest -- scoped manifests re-emit the operator's
// original YAML node -- so the tags matter only for decoding.
type Image struct {
	// Name is the full location for the image, can be referenced by tags or digests
	Name string `json:"name"`

	// Key is the path to the cosign public key used for verifying image signatures
	Key string `json:"key"`

	// Tlog enables transparency log verification
	Tlog bool `json:"use-tlog-verify"`

	// cosign keyless validation options
	CertIdentity                 string `json:"certificate-identity"`
	CertIdentityRegexp           string `json:"certificate-identity-regexp"`
	CertOidcIssuer               string `json:"certificate-oidc-issuer"`
	CertOidcIssuerRegexp         string `json:"certificate-oidc-issuer-regexp"`
	CertGithubWorkflowRepository string `json:"certificate-github-workflow-repository"`

	// Platform of the image to be pulled. If not specified, all platforms are pulled.
	Platform      string `json:"platform"`
	Rewrite       string `json:"rewrite"`
	ExcludeExtras bool   `json:"exclude-extras"`
	Local         bool   `json:"local"`
}

// Chart mirrors hauler's v1.Chart.
type Chart struct {
	Name        string   `json:"name,omitempty"`
	RepoURL     string   `json:"repoURL,omitempty"`
	Version     string   `json:"version,omitempty"`
	Rewrite     string   `json:"rewrite,omitempty"`
	ValuesFiles []string `json:"valuesFiles,omitempty"`
	Platform    string   `json:"platform,omitempty"`

	AddImages       bool `json:"add-images,omitempty"`
	AddDependencies bool `json:"add-dependencies,omitempty"`
	ExcludeExtras   bool `json:"exclude-extras,omitempty"`

	Verify  bool   `json:"verify,omitempty"`
	Keyring string `json:"keyring,omitempty"`

	// Auth (HTTP repos only). Credentials are referenced by env-var name;
	// raw values must NOT appear in manifests.
	UsernameEnv        string `json:"usernameEnv,omitempty"`
	PasswordEnv        string `json:"passwordEnv,omitempty"`
	PassCredentialsAll bool   `json:"passCredentialsAll,omitempty"`

	CertFile              string `json:"certFile,omitempty"`
	KeyFile               string `json:"keyFile,omitempty"`
	CaFile                string `json:"caFile,omitempty"`
	InsecureSkipTLSVerify bool   `json:"insecureSkipTLSVerify,omitempty"`
	PlainHTTP             bool   `json:"plainHTTP,omitempty"`
}

// File mirrors hauler's v1.File.
type File struct {
	Path string `json:"path"`
	Name string `json:"name,omitempty"`
}

// typeMeta and objectMeta cover the subset of the Kubernetes metadata types
// that hauler manifests actually use. Copying these three fields is cheaper
// than depending on apimachinery.
type typeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

type objectMeta struct {
	Name        string            `json:"name,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// docHeader decodes just enough of a document to route it.
type docHeader struct {
	typeMeta
	Metadata objectMeta `json:"metadata,omitempty"`
	Spec     struct {
		Images []Image `json:"images,omitempty"`
		Charts []Chart `json:"charts,omitempty"`
		Files  []File  `json:"files,omitempty"`
	} `json:"spec,omitempty"`
}
