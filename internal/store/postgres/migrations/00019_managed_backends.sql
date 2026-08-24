-- +goose Up

CREATE TABLE managed_backends (
    name TEXT PRIMARY KEY,
    encrypted_config BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down

DROP TABLE managed_backends;
