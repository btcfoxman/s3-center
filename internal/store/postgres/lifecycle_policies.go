package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// ListLifecyclePolicies returns every console-managed retention rule in stable
// prefix order so the dashboard and worker observe the same policy set.
func (s *Store) ListLifecyclePolicies(ctx context.Context) ([]core.LifecyclePolicy, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, prefix, expiration_hours, enabled, created_at, updated_at
		FROM lifecycle_policies ORDER BY prefix, id`)
	if err != nil {
		return nil, fmt.Errorf("list lifecycle policies: %w", err)
	}
	defer rows.Close()

	var policies []core.LifecyclePolicy
	for rows.Next() {
		var policy core.LifecyclePolicy
		if err := rows.Scan(&policy.ID, &policy.Prefix, &policy.ExpirationHours, &policy.Enabled, &policy.CreatedAt, &policy.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan lifecycle policy: %w", err)
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

// CreateLifecyclePolicy inserts one enabled rule. Prefix uniqueness prevents
// competing retention windows from silently applying to the same directory.
func (s *Store) CreateLifecyclePolicy(ctx context.Context, prefix string, expirationHours int) (*core.LifecyclePolicy, error) {
	var policy core.LifecyclePolicy
	err := s.pool.QueryRow(ctx, `
		INSERT INTO lifecycle_policies (prefix, expiration_hours)
		VALUES ($1, $2)
		ON CONFLICT (prefix) DO NOTHING
		RETURNING id, prefix, expiration_hours, enabled, created_at, updated_at`, prefix, expirationHours).
		Scan(&policy.ID, &policy.Prefix, &policy.ExpirationHours, &policy.Enabled, &policy.CreatedAt, &policy.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, core.ErrLifecyclePolicyConflict
	}
	if err != nil {
		return nil, fmt.Errorf("create lifecycle policy: %w", err)
	}
	return &policy, nil
}

func (s *Store) SetLifecyclePolicyEnabled(ctx context.Context, id int64, enabled bool) (*core.LifecyclePolicy, error) {
	var policy core.LifecyclePolicy
	err := s.pool.QueryRow(ctx, `
		UPDATE lifecycle_policies SET enabled=$2, updated_at=NOW() WHERE id=$1
		RETURNING id, prefix, expiration_hours, enabled, created_at, updated_at`, id, enabled).
		Scan(&policy.ID, &policy.Prefix, &policy.ExpirationHours, &policy.Enabled, &policy.CreatedAt, &policy.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, core.ErrLifecyclePolicyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update lifecycle policy: %w", err)
	}
	return &policy, nil
}

func (s *Store) DeleteLifecyclePolicy(ctx context.Context, id int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM lifecycle_policies WHERE id=$1`, id)
	if err != nil {
		return false, fmt.Errorf("delete lifecycle policy: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
