package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *Store) GetRuntimeSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.pool.QueryRow(ctx,
		`SELECT setting_value FROM runtime_settings WHERE setting_key=$1`, strings.TrimSpace(key),
	).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get runtime setting: %w", err)
	}
	return value, true, nil
}

func (s *Store) SetRuntimeSetting(ctx context.Context, key, value string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("runtime setting key is required")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO runtime_settings (setting_key, setting_value)
		VALUES ($1, $2)
		ON CONFLICT (setting_key) DO UPDATE
		SET setting_value=EXCLUDED.setting_value, updated_at=NOW()`, key, value)
	if err != nil {
		return fmt.Errorf("set runtime setting: %w", err)
	}
	return nil
}
