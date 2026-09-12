-- +goose Up

CREATE TABLE lifecycle_policies (
    id BIGSERIAL PRIMARY KEY,
    prefix TEXT NOT NULL UNIQUE,
    expiration_hours INTEGER NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (prefix <> ''),
    CHECK (expiration_hours > 0)
);

CREATE INDEX idx_lifecycle_policies_enabled
    ON lifecycle_policies (enabled, prefix);

-- +goose Down

DROP TABLE lifecycle_policies;
