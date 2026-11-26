-- Schema (docs/architecture.md D6). Applied idempotently at server start.

CREATE TABLE IF NOT EXISTS tenants (
    id          TEXT PRIMARY KEY,                 -- e.g. "search-agents"
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    max_running INT NOT NULL DEFAULT 50           -- quota: concurrent RUNNING/RESUMING sandboxes
);

CREATE TABLE IF NOT EXISTS api_keys (
    id          TEXT PRIMARY KEY,                 -- public key id (prefix shown to humans)
    tenant_id   TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    key_hash    TEXT NOT NULL UNIQUE,             -- sha256(hex) of the full key; plaintext never stored
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS sandboxes (
    id            TEXT PRIMARY KEY,               -- uuid, OpenSandbox-compatible id
    tenant_id     TEXT NOT NULL REFERENCES tenants(id),
    cell_id       TEXT NOT NULL DEFAULT 'cell-1', -- D10 (cells)
    template      TEXT NOT NULL,                  -- agent-sandbox SandboxTemplate / WarmPool name
    image         TEXT NOT NULL,
    entrypoint    JSONB NOT NULL DEFAULT '[]',
    env           JSONB NOT NULL DEFAULT '{}',
    metadata      JSONB NOT NULL DEFAULT '{}',
    network       JSONB,                          -- requested network policy (null = template default)
    state         TEXT NOT NULL,                  -- Pending|Running|Pausing|Paused|Resuming|Stopping|Terminated|Failed
    state_message TEXT NOT NULL DEFAULT '',
    generation    BIGINT NOT NULL DEFAULT 0,      -- fencing token, bumped on every (re)start (D6)
    claim_name    TEXT,                           -- current SandboxClaim (null when paused)
    pod_name      TEXT,
    pod_ip        TEXT,
    execd_token   TEXT,                           -- per-generation execd access token (server-side only)
    snapshot_key  TEXT,                           -- object-store prefix of latest workspace snapshot
    snapshot_bytes BIGINT,
    snapshot_digest TEXT NOT NULL DEFAULT '',     -- digest of the committed archive
    autosave_at   TIMESTAMPTZ,                    -- next requested autosave; null disables/schedules none
    expires_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sandboxes_tenant_state ON sandboxes (tenant_id, state);
CREATE INDEX IF NOT EXISTS sandboxes_expiry ON sandboxes (expires_at) WHERE state IN ('Running', 'Paused');

CREATE TABLE IF NOT EXISTS usage_events (            -- append-only metering (D9)
    id          BIGSERIAL PRIMARY KEY,
    sandbox_id  TEXT NOT NULL,
    tenant_id   TEXT NOT NULL,
    event       TEXT NOT NULL,                    -- created|paused|resumed|terminated|expired|failed|lost|restarted|autosaved
    generation  BIGINT NOT NULL,
    at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    detail      JSONB NOT NULL DEFAULT '{}'
);

-- One current desired lifecycle operation per sandbox. Revision fences a worker
-- when pause/delete/eviction supersedes earlier work. Leases are in Postgres so
-- progress survives process restarts without another service.
CREATE TABLE IF NOT EXISTS lifecycle_jobs (
    sandbox_id  TEXT PRIMARY KEY REFERENCES sandboxes(id) ON DELETE CASCADE,
    revision    BIGINT NOT NULL,
    kind        TEXT NOT NULL,                    -- start|pause|autosave|delete
    generation  BIGINT NOT NULL,
    payload     JSONB NOT NULL DEFAULT '{}',
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts    INT NOT NULL DEFAULT 0,
    owner       TEXT,
    lease_until TIMESTAMPTZ,
    last_error  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS lifecycle_jobs_available ON lifecycle_jobs (available_at, lease_until);

-- Idempotent upgrades for databases created before queue/autosave support.
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS snapshot_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS autosave_at TIMESTAMPTZ;
