-- +goose Up
-- +goose StatementBegin

-- ---------------------------------------------------------------------------
-- Desired state: what the manifests in git declare.
-- ---------------------------------------------------------------------------

-- A git repository holding hauler manifests. Git is the only way an image
-- enters the system; the UI never writes to these tables' downstream desired
-- state directly.
CREATE TABLE sources (
    id                BIGSERIAL PRIMARY KEY,
    name              TEXT        NOT NULL UNIQUE,
    url               TEXT        NOT NULL,
    branch            TEXT        NOT NULL DEFAULT 'main',
    path_glob         TEXT        NOT NULL DEFAULT '*.yaml',
    poll_interval     INTERVAL    NOT NULL DEFAULT '5 minutes',
    auth_kind         TEXT        NOT NULL DEFAULT 'none'
                          CHECK (auth_kind IN ('none', 'basic', 'token', 'ssh')),
    auth_secret_ref   TEXT,
    prune_orphans     BOOLEAN     NOT NULL DEFAULT FALSE,
    enabled           BOOLEAN     NOT NULL DEFAULT TRUE,
    -- 'helm' marks rows reconciled from the chart's values.yaml. The startup
    -- reconciler only prunes rows it owns, so Argo/Flux managing sources
    -- declaratively never fights entries created through the UI.
    managed_by        TEXT        NOT NULL DEFAULT 'ui'
                          CHECK (managed_by IN ('ui', 'helm')),
    last_commit_sha   TEXT,
    last_polled_at    TIMESTAMPTZ,
    last_error        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One manifest file as it existed at one commit.
CREATE TABLE manifest_revisions (
    id             BIGSERIAL PRIMARY KEY,
    source_id      BIGINT      NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    path           TEXT        NOT NULL,
    commit_sha     TEXT        NOT NULL,
    content_sha256 TEXT        NOT NULL,
    parsed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_id, path, commit_sha)
);

-- A flattened spec entry. raw_yaml holds the operator's original YAML node so
-- a scoped manifest can re-emit it verbatim -- rebuilding it from typed fields
-- risks silently dropping a cosign setting.
CREATE TABLE desired_items (
    id                    BIGSERIAL PRIMARY KEY,
    manifest_revision_id  BIGINT NOT NULL REFERENCES manifest_revisions(id) ON DELETE CASCADE,
    kind                  TEXT   NOT NULL CHECK (kind IN ('image', 'chart', 'file')),
    ref                   TEXT   NOT NULL,
    platform              TEXT   NOT NULL DEFAULT '',
    spec                  JSONB  NOT NULL DEFAULT '{}'::jsonb,
    raw_yaml              TEXT   NOT NULL,
    -- Digest of raw_yaml. A change here forces a re-pull even when the
    -- reference is untouched, which is how a tightened cosign policy actually
    -- gets applied.
    spec_hash             TEXT   NOT NULL,
    position              INT    NOT NULL DEFAULT 0
);

CREATE INDEX desired_items_revision_idx ON desired_items (manifest_revision_id);
CREATE INDEX desired_items_ref_idx      ON desired_items (ref, platform);

-- ---------------------------------------------------------------------------
-- Observed state: what actually happened.
-- ---------------------------------------------------------------------------

-- Canonical identity of a tracked image. Platform is part of the key: the
-- amd64 and arm64 builds of one reference are separately pulled and delivered.
CREATE TABLE images (
    id              BIGSERIAL PRIMARY KEY,
    ref             TEXT        NOT NULL,
    repository      TEXT        NOT NULL,
    tag             TEXT,
    platform        TEXT        NOT NULL DEFAULT '',
    pinned_digest   TEXT,
    last_spec_hash  TEXT,
    first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_desired_at TIMESTAMPTZ,
    UNIQUE (ref, platform)
);

CREATE INDEX images_repository_idx ON images (repository);

-- Every digest a reference has resolved to. A mutable tag moving here is what
-- tells the planner to re-pull.
CREATE TABLE image_digests (
    id                  BIGSERIAL PRIMARY KEY,
    image_id            BIGINT      NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    digest              TEXT        NOT NULL,
    resolved_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_by_run_id  BIGINT,
    UNIQUE (image_id, digest)
);

CREATE TABLE runs (
    id             BIGSERIAL PRIMARY KEY,
    source_id      BIGINT      REFERENCES sources(id) ON DELETE SET NULL,
    commit_sha     TEXT,
    trigger        TEXT        NOT NULL
                       CHECK (trigger IN ('schedule', 'manual', 'api', 'retry')),
    status         TEXT        NOT NULL DEFAULT 'queued'
                       CHECK (status IN ('queued', 'running', 'succeeded', 'partial', 'failed', 'cancelled')),
    force_repull   BOOLEAN     NOT NULL DEFAULT FALSE,
    store_id       TEXT,
    hauler_version TEXT,
    stats          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    log_s3_key     TEXT,
    -- The tail is kept inline so the UI renders a run without an object-store
    -- round-trip; the full log lives at log_s3_key.
    log_tail       TEXT,
    error          TEXT,
    queued_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ
);

CREATE INDEX runs_source_queued_idx ON runs (source_id, queued_at DESC);
CREATE INDEX runs_status_idx        ON runs (status) WHERE status IN ('queued', 'running');

CREATE TABLE run_items (
    id          BIGSERIAL PRIMARY KEY,
    run_id      BIGINT      NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    image_id    BIGINT      REFERENCES images(id) ON DELETE SET NULL,
    ref         TEXT        NOT NULL,
    platform    TEXT        NOT NULL DEFAULT '',
    action      TEXT        NOT NULL
                    CHECK (action IN ('pull', 'push', 'skip_present', 'prune', 'archive', 'error')),
    status      TEXT        NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'skipped')),
    reason      TEXT,
    digest      TEXT,
    size_bytes  BIGINT,
    layers      INT,
    duration_ms BIGINT,
    error       TEXT,
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);

CREATE INDEX run_items_run_idx   ON run_items (run_id);
CREATE INDEX run_items_image_idx ON run_items (image_id);

-- ---------------------------------------------------------------------------
-- Targets and delivery.
-- ---------------------------------------------------------------------------

CREATE TABLE targets (
    id             BIGSERIAL PRIMARY KEY,
    name           TEXT        NOT NULL UNIQUE,
    kind           TEXT        NOT NULL CHECK (kind IN ('registry', 'archive')),
    -- registry: {url, project, insecure, plain_http, credential_secret_ref}
    -- archive:  {s3_endpoint, s3_bucket, s3_prefix, chunk_size, credential_secret_ref}
    config         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    retention_days INT         CHECK (retention_days IS NULL OR retention_days >= 0),
    enabled        BOOLEAN     NOT NULL DEFAULT TRUE,
    managed_by     TEXT        NOT NULL DEFAULT 'ui' CHECK (managed_by IN ('ui', 'helm')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Proof that a specific digest reached a specific target. The unique
-- constraint is the dedupe primitive the whole system turns on: if a row
-- exists here, the planner skips the work.
CREATE TABLE deliveries (
    id           BIGSERIAL PRIMARY KEY,
    run_id       BIGINT      REFERENCES runs(id) ON DELETE SET NULL,
    target_id    BIGINT      NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
    image_id     BIGINT      NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    digest       TEXT        NOT NULL,
    target_ref   TEXT,
    status       TEXT        NOT NULL DEFAULT 'succeeded'
                     CHECK (status IN ('succeeded', 'failed')),
    error        TEXT,
    delivered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (target_id, image_id, digest)
);

CREATE INDEX deliveries_digest_idx ON deliveries (target_id, digest);

CREATE TABLE archives (
    id          BIGSERIAL PRIMARY KEY,
    run_id      BIGINT      REFERENCES runs(id) ON DELETE SET NULL,
    target_id   BIGINT      NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
    filename    TEXT        NOT NULL,
    s3_bucket   TEXT        NOT NULL,
    s3_key      TEXT        NOT NULL,
    size_bytes  BIGINT,
    sha256      TEXT,
    chunk_count INT         NOT NULL DEFAULT 1,
    commit_sha  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ,
    -- Held archives are never swept, whatever the target's retention says.
    held        BOOLEAN     NOT NULL DEFAULT FALSE,
    deleted_at  TIMESTAMPTZ,
    UNIQUE (s3_bucket, s3_key)
);

-- Partial index: the retention sweeper only ever scans live, unheld archives.
CREATE INDEX archives_expiry_idx ON archives (expires_at)
    WHERE deleted_at IS NULL AND held = FALSE;

CREATE TABLE archive_contents (
    archive_id BIGINT NOT NULL REFERENCES archives(id) ON DELETE CASCADE,
    image_id   BIGINT NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    digest     TEXT   NOT NULL,
    PRIMARY KEY (archive_id, image_id, digest)
);

-- Cache of what already exists downstream. Populated from the registry v2 API
-- today and from Harbor's API in the Harbor phase, which is why the origin is
-- recorded rather than assumed.
CREATE TABLE registry_inventory (
    id          BIGSERIAL PRIMARY KEY,
    target_id   BIGINT      NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
    repository  TEXT        NOT NULL,
    tag         TEXT,
    digest      TEXT        NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    source      TEXT        NOT NULL DEFAULT 'registry-v2'
                    CHECK (source IN ('registry-v2', 'harbor-api')),
    UNIQUE (target_id, repository, digest)
);

CREATE INDEX registry_inventory_digest_idx ON registry_inventory (target_id, digest);

-- ---------------------------------------------------------------------------
-- Auth and operations.
-- ---------------------------------------------------------------------------

CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    issuer        TEXT        NOT NULL,
    subject       TEXT        NOT NULL,
    email         TEXT,
    name          TEXT,
    role          TEXT        NOT NULL DEFAULT 'viewer'
                      CHECK (role IN ('viewer', 'operator', 'admin')),
    disabled      BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ,
    UNIQUE (issuer, subject)
);

-- Sessions live in the database rather than in a signed cookie so an admin can
-- actually revoke one.
CREATE TABLE sessions (
    id         TEXT        PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_agent TEXT,
    ip         INET,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX sessions_user_idx    ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- Only the hash is stored; the plaintext token is shown once at creation.
CREATE TABLE api_tokens (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL,
    prefix       TEXT        NOT NULL UNIQUE,
    token_hash   TEXT        NOT NULL,
    scopes       TEXT[]      NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX api_tokens_user_idx ON api_tokens (user_id);

CREATE TABLE audit_events (
    id             BIGSERIAL PRIMARY KEY,
    actor_user_id  BIGINT      REFERENCES users(id) ON DELETE SET NULL,
    actor_token_id BIGINT      REFERENCES api_tokens(id) ON DELETE SET NULL,
    action         TEXT        NOT NULL,
    subject_type   TEXT,
    subject_id     TEXT,
    detail         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_at_idx    ON audit_events (at DESC);
CREATE INDEX audit_events_actor_idx ON audit_events (actor_user_id, at DESC);

-- Job queue. Postgres with FOR UPDATE SKIP LOCKED is enough here and saves
-- operating a Redis in an airgap.
CREATE TABLE jobs (
    id           BIGSERIAL PRIMARY KEY,
    kind         TEXT        NOT NULL,
    payload      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    status       TEXT        NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    run_after    TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts     INT         NOT NULL DEFAULT 0,
    max_attempts INT         NOT NULL DEFAULT 3,
    locked_by    TEXT,
    locked_at    TIMESTAMPTZ,
    last_error   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The claim query's index: only queued rows, ordered by when they may run.
CREATE INDEX jobs_claim_idx ON jobs (run_after, id) WHERE status = 'queued';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS registry_inventory;
DROP TABLE IF EXISTS archive_contents;
DROP TABLE IF EXISTS archives;
DROP TABLE IF EXISTS deliveries;
DROP TABLE IF EXISTS targets;
DROP TABLE IF EXISTS run_items;
DROP TABLE IF EXISTS runs;
DROP TABLE IF EXISTS image_digests;
DROP TABLE IF EXISTS images;
DROP TABLE IF EXISTS desired_items;
DROP TABLE IF EXISTS manifest_revisions;
DROP TABLE IF EXISTS sources;
-- +goose StatementEnd
