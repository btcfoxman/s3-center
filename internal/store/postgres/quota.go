// -------------------------------------------------------------------------------
// Quota Operations
//
// Author: Alex Freidah
//
// Implements the Postgres engine bindings for the backend_quotas table:
// per-backend bytes_used, bytes_limit, and orphan_bytes tracking. Carries
// the read-side eligibility queries the BackendManager uses for write
// routing (GetBackendWithSpace, GetLeastUtilizedBackend) and the per-tx
// increment / decrement primitives core/ uses to keep quota in lockstep
// with object_locations changes. Increment is guarded so the UPDATE
// touches zero rows when the limit would be exceeded, surfacing as
// ErrNoSpaceAvailable.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
)

// SyncQuotaLimits ensures the backend_quotas table has entries for all configured
// backends with their quota limits. Creates new entries or updates existing limits.
// All updates happen in a single transaction for atomicity.
//
//nolint:gocognit // Backend and shared-group quota state must be synchronized as one ordered operation.
func (s *Store) SyncQuotaLimits(ctx context.Context, backends []config.BackendConfig) error {
	if err := s.withTx(ctx, func(qtx *db.Queries) error {
		for i := range backends {
			err := qtx.UpsertQuotaLimit(ctx, db.UpsertQuotaLimitParams{
				BackendName: backends[i].Name,
				BytesLimit:  backends[i].QuotaBytes,
			})
			if err != nil {
				return fmt.Errorf("failed to sync quota for backend %s: %w", backends[i].Name, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin quota group sync: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for i := range backends {
		b := backends[i]
		group := b.QuotaGroup
		if group == "" {
			group = b.Name
		}
		billingPolicy := b.BillingPolicy
		if billingPolicy == "" {
			billingPolicy = "free_capped"
		}
		raw := b.RawLimitBytes
		if raw == 0 {
			raw = b.QuotaBytes
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO quota_groups (name, raw_limit_bytes, static_buffer_bytes, dynamic_buffer_bytes, billing_policy)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (name) DO UPDATE SET
				raw_limit_bytes=EXCLUDED.raw_limit_bytes,
				static_buffer_bytes=EXCLUDED.static_buffer_bytes,
				dynamic_buffer_bytes=EXCLUDED.dynamic_buffer_bytes,
				billing_policy=EXCLUDED.billing_policy, updated_at=NOW()`,
			group, raw, b.StaticBufferBytes, b.DynamicBufferBytes, billingPolicy); err != nil {
			return fmt.Errorf("sync quota group %s: %w", group, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO backend_quota_groups (backend_name, quota_group) VALUES ($1,$2)
			ON CONFLICT (backend_name) DO UPDATE SET quota_group=EXCLUDED.quota_group`, b.Name, group); err != nil {
			return fmt.Errorf("map backend %s to quota group: %w", b.Name, err)
		}
	}
	return tx.Commit(ctx)
}

// GetBackendWithSpace finds a backend with enough quota for the given size.
// Returns the backend name or ErrNoSpaceAvailable if none have enough space.
func (s *Store) GetBackendWithSpace(ctx context.Context, size int64, backendOrder []string) (string, error) {
	for _, name := range backendOrder {
		available, err := s.queries.GetBackendAvailableSpace(ctx, name)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("failed to check quota for %s: %w", name, err)
		}

		if available >= size {
			return name, nil
		}
	}

	return "", core.ErrNoSpaceAvailable
}

// GetLeastUtilizedBackend finds the backend with the lowest utilization ratio
// that has enough space for the given size. Used by the "spread" routing strategy.
func (s *Store) GetLeastUtilizedBackend(ctx context.Context, size int64, eligible []string) (string, error) {
	row, err := s.queries.GetLeastUtilizedBackend(ctx, db.GetLeastUtilizedBackendParams{
		BackendNames: eligible,
		MinSize:      size,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", core.ErrNoSpaceAvailable
	}
	if err != nil {
		return "", fmt.Errorf("failed to find least utilized backend: %w", err)
	}
	return row.BackendName, nil
}

// GetQuotaStats returns quota statistics for all backends.
func (s *Store) GetQuotaStats(ctx context.Context) (map[string]core.QuotaStat, error) {
	rows, err := s.queries.GetAllQuotaStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query quota stats: %w", err)
	}

	stats := make(map[string]core.QuotaStat, len(rows))
	for _, row := range rows {
		stats[row.BackendName] = core.QuotaStat{
			BackendName: row.BackendName,
			BytesUsed:   row.BytesUsed,
			BytesLimit:  row.BytesLimit,
			OrphanBytes: row.OrphanBytes,
			UpdatedAt:   row.UpdatedAt.Time,
		}
	}

	return stats, nil
}

// GetObjectCounts returns the number of objects stored on each backend.
func (s *Store) GetObjectCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.queries.GetObjectCountsByBackend(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query object counts: %w", err)
	}

	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		counts[row.BackendName] = row.ObjectCount
	}
	return counts, nil
}

// GetUnverifiedObjectCounts returns the number of objects per backend whose
// content_hash column is NULL (objects predating integrity verification
// or otherwise not yet checksummed). Drives the dashboard's "needs
// backfill" column.
func (s *Store) GetUnverifiedObjectCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.queries.GetUnverifiedObjectCountsByBackend(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query unverified object counts: %w", err)
	}
	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		counts[row.BackendName] = row.ObjectCount
	}
	return counts, nil
}

// GetActiveMultipartCounts returns the number of in-progress multipart uploads
// per backend.
func (s *Store) GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.queries.GetActiveMultipartCountsByBackend(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query multipart counts: %w", err)
	}

	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		counts[row.BackendName] = row.UploadCount
	}
	return counts, nil
}
