package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

func (s *Store) ListManagedBackends(ctx context.Context) ([]core.ManagedBackendRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, encrypted_config, created_at, updated_at FROM managed_backends ORDER BY created_at, name`)
	if err != nil {
		return nil, fmt.Errorf("list managed backends: %w", err)
	}
	defer rows.Close()
	var records []core.ManagedBackendRecord
	for rows.Next() {
		var record core.ManagedBackendRecord
		var created, updated string
		if err := rows.Scan(&record.Name, &record.EncryptedConfig, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan managed backend: %w", err)
		}
		record.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		record.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) UpsertManagedBackend(ctx context.Context, name string, encryptedConfig []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO managed_backends (name, encrypted_config, created_at, updated_at)
		VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		ON CONFLICT(name) DO UPDATE SET encrypted_config=excluded.encrypted_config, updated_at=excluded.updated_at`, name, encryptedConfig)
	if err != nil {
		return fmt.Errorf("upsert managed backend %q: %w", name, err)
	}
	return nil
}

func (s *Store) DeleteManagedBackend(ctx context.Context, name string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM managed_backends WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete managed backend %q: %w", name, err)
	}
	return nil
}
