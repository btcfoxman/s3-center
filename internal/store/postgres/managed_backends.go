package postgres

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

func (s *Store) ListManagedBackends(ctx context.Context) ([]core.ManagedBackendRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, encrypted_config, created_at, updated_at FROM managed_backends ORDER BY created_at, name`)
	if err != nil {
		return nil, fmt.Errorf("list managed backends: %w", err)
	}
	defer rows.Close()
	var records []core.ManagedBackendRecord
	for rows.Next() {
		var record core.ManagedBackendRecord
		if err := rows.Scan(&record.Name, &record.EncryptedConfig, &record.CreatedAt, &record.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan managed backend: %w", err)
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) UpsertManagedBackend(ctx context.Context, name string, encryptedConfig []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO managed_backends (name, encrypted_config) VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET encrypted_config = EXCLUDED.encrypted_config, updated_at = NOW()`, name, encryptedConfig)
	if err != nil {
		return fmt.Errorf("upsert managed backend %q: %w", name, err)
	}
	return nil
}

func (s *Store) DeleteManagedBackend(ctx context.Context, name string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM managed_backends WHERE name = $1`, name); err != nil {
		return fmt.Errorf("delete managed backend %q: %w", name, err)
	}
	return nil
}
