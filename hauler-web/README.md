# hauler-web

Stateful image movement on top of [Rancher Government Hauler](https://hauler.dev).

`hauler` is excellent at moving images into an airgap, but it is stateless: run
`hauler store sync -f manifest.yaml` twice and nothing records what was pulled,
at which digest, or whether it ever reached the destination registry.
`hauler-web` adds that memory.

- **Git stays the source of truth.** Images are declared in hauler manifests in
  a git repository. There is no "add an image" button; the UI observes,
  triggers, and retries.
- **Postgres records what happened.** Every pull, push, and archive is tracked
  per digest, so a reconcile only does the work that is actually outstanding.
- **hauler does the moving.** hauler-web shells out to a pinned `hauler`
  binary rather than importing it, so the two can be upgraded independently.

## Status

Early. The reconciliation core is implemented and tested; the database, web UI,
and Harbor integration are in progress.

| Component | State |
| --- | --- |
| `internal/config` — env-first configuration | done |
| `internal/manifest` — manifest parsing and scoping | done |
| `internal/hauler` — hauler CLI wrapper | done |
| `internal/planner` — desired-vs-observed diff | done |
| `internal/registryclient` — digest resolution | done |
| `migrations/` — Postgres schema | written, not yet wired |
| `internal/db`, `gitsource`, `jobs`, `api`, `web`, `harbor` | not started |

`hauler-web plan` works today and needs no database.

## Try it

```bash
make build

# What would a reconcile do?
bin/hauler-web plan --manifests ./manifests --store ./store
```

```
store ./store (24 artifacts)

ACTION        REFERENCE                          PLATFORM      DIGEST                  REASON
skip_present  gcr.io/distroless/base@sha256:7fa  -             sha256:7fa7445dfbeb...  already pulled and delivered at this digest
pull          ghcr.io/org/app:v1.4.0             linux/amd64   sha256:1c9a02f81b3e...  new entry

1 pull, 0 push, 1 skip
```

`plan` changes nothing. It parses the manifests, HEADs each reference upstream
to resolve its current digest, inspects the local hauler store, and prints the
difference.

## How dedupe works

For each declared image:

1. Resolve the reference to a digest with a registry `HEAD` — one round trip,
   no image data. A digest-pinned reference skips even that, so a fully pinned
   manifest reconciles with no network access at all.
2. If that digest has already been delivered to every enabled target, skip it.
3. If it is in the local store but not yet delivered, push only.
4. Otherwise pull, then deliver.

A moved tag, a new entry, or **any** change to the manifest entry — including a
cosign setting that leaves the reference untouched — forces a pull. That last
case matters: skipping it would leave a tightened verification policy silently
unapplied.

## Design notes

**Manifest passthrough.** Each spec entry is kept in two forms: a typed view for
planning, and its original `yaml.Node`. Generated manifests re-emit the
operator's YAML verbatim rather than re-serialising from structs, so a cosign
field this codebase does not model cannot be silently dropped on the way to
hauler. Decoding goes through YAML → JSON → `encoding/json` to match hauler's
own apimachinery-based path exactly.

**Why shell out.** Importing hauler would pull in helm v4, cosign v3,
containerd v2, and apimachinery to run a service whose real job is bookkeeping.
The contract instead is hauler's CLI plus its documented `store info -o json`
output. One wrinkle that shaped the code: hauler's logger writes to *stdout*,
the same stream that JSON is printed on, so commands whose output is parsed run
at `--log-level disabled` and additionally extract the JSON object defensively.

**Archives are snapshots.** A haul is the whole store at a commit, which is how
hauler is actually used for airgap transfer. A new archive is written only when
the store's contents change — a push-only run does not produce one.

## Development

```bash
make test               # unit tests: no database, network, or hauler binary
make test-integration   # additionally requires a real hauler binary

# The integration suite can seed a store from a haul archive so it needs no network:
HAULERWEB_TEST_HAULER_BIN=/usr/local/bin/hauler \
HAULERWEB_TEST_HAUL=/path/to/haul.tar.zst \
  make test-integration
```

## Configuration

Every setting is an environment variable so no config file is needed in
Kubernetes. Precedence is flag > `HAULERWEB_*` > default, and invalid values are
rejected at startup rather than clamped.

| Variable | Default | Purpose |
| --- | --- | --- |
| `HAULERWEB_DATABASE_URL` | — | Postgres DSN (required once the DB lands) |
| `HAULERWEB_LISTEN_ADDR` | `:8080` | Web/API bind address |
| `HAULERWEB_STORE_DIR` | `/var/lib/hauler-web/store` | Persistent hauler store |
| `HAULERWEB_TEMP_DIR` | OS default | hauler `--tempdir`; archive scratch space |
| `HAULERWEB_WORK_DIR` | `/var/lib/hauler-web/work` | Git checkouts, generated manifests |
| `HAULERWEB_HAULER_BIN` | `hauler` | hauler executable |
| `HAULERWEB_CONCURRENCY` | `5` | hauler `-j` |
| `HAULERWEB_POLL_INTERVAL` | `5m` | Default git poll cadence |
| `HAULERWEB_HAULER_TIMEOUT` | `6h` | Bound on a single hauler invocation |
| `HAULERWEB_LOG_LEVEL` | `info` | `trace`, `debug`, `info`, `warn`, `error` |

## License

Apache 2.0.
