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

The headless reconciler works end to end: it pulls from upstream, pushes to a
destination registry, and records every digest in Postgres, so a second run
over unchanged inputs does nothing. That last property is verified mechanically
(see *Verification* below), not by inspection.

Not built yet: the web UI, OIDC and API tokens, S3 archives with retention, the
job queue, the Helm chart, and the Harbor API integration.

| Component | State |
| --- | --- |
| `internal/config` — env-first configuration | done |
| `internal/manifest` — parsing and scoping | done |
| `internal/hauler` — hauler CLI wrapper | done |
| `internal/planner` — desired-vs-observed diff | done |
| `internal/registryclient` — digest resolution | done |
| `internal/db` + `migrations/` — Postgres schema and queries | done |
| `internal/gitsource` — manifest repository checkout | done |
| `internal/reconcile` — the loop that moves images | done |
| `internal/storage`, `jobs`, `api`, `web`, `auth`, `harbor` | not started |
| Helm chart | not started |

## Commands

| Command | What it does |
| --- | --- |
| `plan` | Print what a reconcile would do. Changes nothing, needs no database. |
| `migrate` | Apply the database schema. |
| `reconcile` | One pass over every enabled source. Pulls, pushes, records. |
| `worker` | The same, on a poll interval. This is what the container runs. |

`serve` does not exist yet, which is why the image's `CMD` is `worker`.

## Try it

```bash
make build

# What would a reconcile do? Reads only; no database required.
bin/hauler-web plan --manifests ./manifests --store ./store

# Actually move things.
export HAULERWEB_DATABASE_URL=postgres://haulerweb:haulerweb@localhost:5432/haulerweb?sslmode=disable
bin/hauler-web migrate
bin/hauler-web reconcile
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

## Verification

Three tiers, all of which run in CI on every push.

```bash
make test               # unit: no database, network, or hauler binary
make test-integration   # + a real hauler binary and a real Postgres
make test-e2e           # + the full reconcile loop against live registries
```

The e2e suite is the one that matters. It runs the real hauler binary against
two in-process OCI registries and a real Postgres, and asserts:

1. a first reconcile pulls the image and it appears in the destination registry;
2. **a second reconcile over unchanged inputs pulls nothing** — the whole point
   of the system, and a regression that would otherwise be invisible because a
   redundant re-pull still reports success;
3. moving a tag upstream causes exactly one re-pull;
4. adding one image to a manifest pulls only that image;
5. an unresolvable reference degrades the run to `partial` without blocking the
   healthy images;
6. a second worker refuses to write a store another already holds.

It needs no Docker and no internet: go-containerregistry serves both registries
over loopback, which hauler reaches without any insecure flag. That is the right
property for a test of an airgap tool.

```bash
HAULERWEB_TEST_DATABASE_URL=postgres://...  \
HAULERWEB_TEST_HAULER_BIN=/usr/local/bin/hauler \
  make test-e2e
```

## Configuration

Every setting is an environment variable so no config file is needed in
Kubernetes. Precedence is flag > `HAULERWEB_*` > default, and invalid values are
rejected at startup rather than clamped.

| Variable | Default | Purpose |
| --- | --- | --- |
| `HAULERWEB_DATABASE_URL` | — | Postgres DSN (required by `migrate`, `reconcile`, `worker`) |
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
