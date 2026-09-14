package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func (s *Store) GetRuntimeSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT setting_value FROM runtime_settings WHERE setting_key=?`, strings.TrimSpace(key),
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
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
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runtime_settings (setting_key, setting_value)
		VALUES (?, ?)
		ON CONFLICT(setting_key) DO UPDATE SET
			setting_value=excluded.setting_value,
			updated_at=strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`, key, value)
	if err != nil {
		return fmt.Errorf("set runtime setting: %w", err)
	}
	return nil
}
