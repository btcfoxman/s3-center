CREATE TABLE IF NOT EXISTS runtime_settings (
    setting_key TEXT PRIMARY KEY CHECK (setting_key <> ''),
    setting_value TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
