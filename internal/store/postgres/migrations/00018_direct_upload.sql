-- +goose Up

ALTER TABLE object_locations
    ADD COLUMN physical_key TEXT,
    ADD COLUMN object_version TEXT;

CREATE TABLE quota_groups (
    name TEXT PRIMARY KEY,
    raw_limit_bytes BIGINT NOT NULL,
    static_buffer_bytes BIGINT NOT NULL DEFAULT 0,
    dynamic_buffer_bytes BIGINT NOT NULL DEFAULT 0,
    reserved_bytes BIGINT NOT NULL DEFAULT 0,
    drift_bytes BIGINT NOT NULL DEFAULT 0,
    billing_policy TEXT NOT NULL DEFAULT 'free_capped',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (raw_limit_bytes >= 0),
    CHECK (reserved_bytes >= 0),
    CHECK (billing_policy IN ('free_capped', 'metered'))
);

CREATE TABLE backend_quota_groups (
    backend_name TEXT PRIMARY KEY REFERENCES backend_quotas(backend_name) ON DELETE CASCADE,
    quota_group TEXT NOT NULL REFERENCES quota_groups(name)
);

CREATE TABLE upload_sessions (
    id TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    logical_key TEXT NOT NULL,
    physical_key TEXT NOT NULL,
    backend_name TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    quota_group TEXT NOT NULL REFERENCES quota_groups(name),
    status TEXT NOT NULL,
    declared_bytes BIGINT NOT NULL,
    reserved_bytes BIGINT NOT NULL,
    actual_bytes BIGINT,
    content_type TEXT,
    etag TEXT,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error TEXT,
    CHECK (status IN ('reserved', 'url_issued', 'committing', 'committed', 'aborted', 'expired', 'failed'))
);

CREATE TABLE capacity_reservations (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL UNIQUE REFERENCES upload_sessions(id) ON DELETE CASCADE,
    quota_group TEXT NOT NULL REFERENCES quota_groups(name),
    backend_name TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    bytes BIGINT NOT NULL,
    status TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (status IN ('active', 'released', 'committed'))
);

CREATE INDEX idx_upload_sessions_status_expiry ON upload_sessions(status, expires_at);
CREATE INDEX idx_upload_sessions_created ON upload_sessions(created_at DESC);

-- +goose Down

DROP TABLE IF EXISTS capacity_reservations;
DROP TABLE IF EXISTS upload_sessions;
DROP TABLE IF EXISTS backend_quota_groups;
DROP TABLE IF EXISTS quota_groups;
ALTER TABLE object_locations DROP COLUMN IF EXISTS object_version, DROP COLUMN IF EXISTS physical_key;
