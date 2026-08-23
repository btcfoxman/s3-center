ALTER TABLE object_locations ADD COLUMN physical_key TEXT;
ALTER TABLE object_locations ADD COLUMN object_version TEXT;

CREATE TABLE quota_groups (
    name TEXT PRIMARY KEY,
    raw_limit_bytes INTEGER NOT NULL,
    static_buffer_bytes INTEGER NOT NULL DEFAULT 0,
    dynamic_buffer_bytes INTEGER NOT NULL DEFAULT 0,
    reserved_bytes INTEGER NOT NULL DEFAULT 0,
    drift_bytes INTEGER NOT NULL DEFAULT 0,
    billing_policy TEXT NOT NULL DEFAULT 'free_capped',
    updated_at TEXT NOT NULL
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
    declared_bytes INTEGER NOT NULL,
    reserved_bytes INTEGER NOT NULL,
    actual_bytes INTEGER,
    content_type TEXT,
    etag TEXT,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    last_error TEXT
);

CREATE TABLE capacity_reservations (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL UNIQUE REFERENCES upload_sessions(id) ON DELETE CASCADE,
    quota_group TEXT NOT NULL REFERENCES quota_groups(name),
    backend_name TEXT NOT NULL REFERENCES backend_quotas(backend_name),
    bytes INTEGER NOT NULL,
    status TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX idx_upload_sessions_status_expiry ON upload_sessions(status, expires_at);
CREATE INDEX idx_upload_sessions_created ON upload_sessions(created_at DESC);
