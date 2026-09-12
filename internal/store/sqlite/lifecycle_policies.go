package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

func scanLifecyclePolicy(row interface{ Scan(...any) error }) (*core.LifecyclePolicy, error) {
	var policy core.LifecyclePolicy
	var enabled int
	var created, updated string
	if err := row.Scan(&policy.ID, &policy.Prefix, &policy.ExpirationHours, &enabled, &created, &updated); err != nil {
		return nil, err
	}
	policy.Enabled = enabled != 0
	policy.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	policy.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return &policy, nil
}

func (s *Store) ListLifecyclePolicies(ctx context.Context) ([]core.LifecyclePolicy, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, prefix, expiration_hours, enabled, created_at, updated_at
		FROM lifecycle_policies ORDER BY prefix, id`)
	if err != nil {
		return nil, fmt.Errorf("list lifecycle policies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var policies []core.LifecyclePolicy
	for rows.Next() {
		policy, err := scanLifecyclePolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("scan lifecycle policy: %w", err)
		}
		policies = append(policies, *policy)
	}
	return policies, rows.Err()
}

func (s *Store) CreateLifecyclePolicy(ctx context.Context, prefix string, expirationHours int) (*core.LifecyclePolicy, error) {
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO lifecycle_policies (prefix, expiration_hours)
		VALUES (?, ?)
		ON CONFLICT(prefix) DO NOTHING
		RETURNING id, prefix, expiration_hours, enabled, created_at, updated_at`, prefix, expirationHours)
	policy, err := scanLifecyclePolicy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrLifecyclePolicyConflict
	}
	if err != nil {
		return nil, fmt.Errorf("create lifecycle policy: %w", err)
	}
	return policy, nil
}

func (s *Store) SetLifecyclePolicyEnabled(ctx context.Context, id int64, enabled bool) (*core.LifecyclePolicy, error) {
	row := s.db.QueryRowContext(ctx, `
		UPDATE lifecycle_policies
		SET enabled=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id=?
		RETURNING id, prefix, expiration_hours, enabled, created_at, updated_at`, enabled, id)
	policy, err := scanLifecyclePolicy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrLifecyclePolicyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update lifecycle policy: %w", err)
	}
	return policy, nil
}

func (s *Store) DeleteLifecyclePolicy(ctx context.Context, id int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM lifecycle_policies WHERE id=?`, id)
	if err != nil {
		return false, fmt.Errorf("delete lifecycle policy: %w", err)
	}
	count, err := result.RowsAffected()
	return count > 0, err
}
