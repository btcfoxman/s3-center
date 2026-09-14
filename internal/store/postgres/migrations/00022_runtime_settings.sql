-- +goose Up

CREATE TABLE runtime_settings (
    setting_key TEXT PRIMARY KEY,
    setting_value TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (setting_key <> '')
);

-- +goose Down

DROP TABLE runtime_settings;
