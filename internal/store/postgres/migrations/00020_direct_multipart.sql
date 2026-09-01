-- +goose Up

ALTER TABLE upload_sessions ADD COLUMN upstream_upload_id TEXT;

-- +goose Down

ALTER TABLE upload_sessions DROP COLUMN IF EXISTS upstream_upload_id;
