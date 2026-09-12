CREATE TABLE IF NOT EXISTS lifecycle_policies (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    prefix TEXT NOT NULL UNIQUE CHECK (prefix <> ''),
    expiration_hours INTEGER NOT NULL CHECK (expiration_hours > 0),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_lifecycle_policies_enabled
    ON lifecycle_policies(enabled, prefix);
