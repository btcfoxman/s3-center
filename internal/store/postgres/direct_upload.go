package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

func scanUpload(row pgx.Row) (*core.UploadSession, error) {
	var u core.UploadSession
	var actual *int64
	var contentType, etag, upstreamUploadID, lastError *string
	err := row.Scan(&u.ID, &u.IdempotencyKey, &u.LogicalKey, &u.PhysicalKey,
		&u.BackendName, &u.QuotaGroup, &u.Status, &u.DeclaredBytes, &u.ReservedBytes,
		&actual, &contentType, &etag, &upstreamUploadID, &u.ExpiresAt, &u.CreatedAt, &u.UpdatedAt, &lastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, core.ErrUploadSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	if actual != nil {
		u.ActualBytes = *actual
	}
	if contentType != nil {
		u.ContentType = *contentType
	}
	if etag != nil {
		u.ETag = *etag
	}
	if upstreamUploadID != nil {
		u.UpstreamUploadID = *upstreamUploadID
	}
	if lastError != nil {
		u.LastError = *lastError
	}
	return &u, nil
}

const uploadColumns = `id, idempotency_key, logical_key, physical_key, backend_name, quota_group,
	status, declared_bytes, reserved_bytes, actual_bytes, content_type, etag, upstream_upload_id,
	expires_at, created_at, updated_at, last_error`

//nolint:gocognit // Keeping selection and reservation in one transaction is the oversubscription invariant.
func (s *Store) ReserveUpload(ctx context.Context, p *core.ReserveUploadParams) (*core.UploadSession, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin upload reservation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if existing, e := scanUpload(tx.QueryRow(ctx, `SELECT `+uploadColumns+` FROM upload_sessions WHERE idempotency_key=$1`, p.IdempotencyKey)); e == nil {
		return existing, nil
	} else if !errors.Is(e, core.ErrUploadSessionNotFound) {
		return nil, fmt.Errorf("read idempotent upload: %w", e)
	}

	type candidate struct {
		backend, group string
		remaining      int64
	}
	best := candidate{remaining: math.MaxInt64}
	for _, name := range p.Candidates {
		var group string
		var raw, staticBuffer, dynamicBuffer, reserved, drift, committed int64
		err := tx.QueryRow(ctx, `
			SELECT q.name, q.raw_limit_bytes, q.static_buffer_bytes, q.dynamic_buffer_bytes,
			       q.reserved_bytes, q.drift_bytes,
			       COALESCE((SELECT SUM(b.bytes_used+b.orphan_bytes)
			                 FROM backend_quotas b JOIN backend_quota_groups m ON m.backend_name=b.backend_name
			                 WHERE m.quota_group=q.name),0)
			FROM quota_groups q JOIN backend_quota_groups m ON m.quota_group=q.name
			WHERE m.backend_name=$1 FOR UPDATE OF q`, name).
			Scan(&group, &raw, &staticBuffer, &dynamicBuffer, &reserved, &drift, &committed)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("lock capacity for %s: %w", name, err)
		}
		remaining := int64(math.MaxInt64)
		if raw > 0 {
			remaining = raw - staticBuffer - dynamicBuffer - reserved - drift - committed - p.DeclaredBytes
		}
		if remaining >= 0 && (best.backend == "" || remaining < best.remaining) {
			best = candidate{name, group, remaining}
		}
	}
	if best.backend == "" {
		return nil, core.ErrNoSpaceAvailable
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `UPDATE quota_groups SET reserved_bytes=reserved_bytes+$1, updated_at=$2 WHERE name=$3`, p.DeclaredBytes, now, best.group); err != nil {
		return nil, fmt.Errorf("reserve group capacity: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO upload_sessions
		(id,idempotency_key,logical_key,physical_key,backend_name,quota_group,status,declared_bytes,reserved_bytes,content_type,expires_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$8,$9,$10,$11,$11)`, p.ID, p.IdempotencyKey, p.LogicalKey,
		p.PhysicalKey, best.backend, best.group, core.UploadReserved, p.DeclaredBytes, p.ContentType, p.ExpiresAt, now); err != nil {
		return nil, fmt.Errorf("insert upload session: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO capacity_reservations
		(id,session_id,quota_group,backend_name,bytes,status,expires_at,created_at,updated_at)
		VALUES($1,$1,$2,$3,$4,'active',$5,$6,$6)`, p.ID, best.group, best.backend, p.DeclaredBytes, p.ExpiresAt, now); err != nil {
		return nil, fmt.Errorf("insert capacity reservation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit upload reservation: %w", err)
	}
	return s.GetUploadSession(ctx, p.ID)
}

func (s *Store) MarkUploadURLIssued(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE upload_sessions SET status=$1,updated_at=NOW() WHERE id=$2 AND status=$3`, core.UploadURLIssued, id, core.UploadReserved)
	if err != nil {
		return fmt.Errorf("mark upload URL issued: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return core.ErrUploadSessionConflict
	}
	return nil
}

func (s *Store) SetUploadMultipartID(ctx context.Context, id, upstreamID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE upload_sessions SET upstream_upload_id=$1,updated_at=NOW()
		WHERE id=$2 AND upstream_upload_id IS NULL AND status=$3`, upstreamID, id, core.UploadReserved)
	if err != nil {
		return fmt.Errorf("set upstream multipart id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return core.ErrUploadSessionConflict
	}
	return nil
}

func (s *Store) GetUploadSession(ctx context.Context, id string) (*core.UploadSession, error) {
	u, err := scanUpload(s.pool.QueryRow(ctx, `SELECT `+uploadColumns+` FROM upload_sessions WHERE id=$1`, id))
	if err != nil && !errors.Is(err, core.ErrUploadSessionNotFound) {
		return nil, fmt.Errorf("get upload session: %w", err)
	}
	return u, err
}

//nolint:gocognit // The overwrite catalog switch must remain a single auditable transaction.
func (s *Store) CommitUpload(ctx context.Context, id string, actual int64, etag string) ([]core.DeletedCopy, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin upload commit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	u, err := scanUpload(tx.QueryRow(ctx, `SELECT `+uploadColumns+` FROM upload_sessions WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return nil, err
	}
	if u.Status == core.UploadCommitted {
		return nil, nil
	}
	if u.Status != core.UploadURLIssued && u.Status != core.UploadReserved && u.Status != core.UploadExpired {
		return nil, core.ErrUploadSessionConflict
	}
	// Match normal-write lock ordering (quota group before backend quota) so
	// account-shared capacity cannot race a direct-upload commit.
	if _, err := tx.Exec(ctx, `SELECT name FROM quota_groups WHERE name=$1 FOR UPDATE`, u.QuotaGroup); err != nil {
		return nil, fmt.Errorf("lock upload quota group: %w", err)
	}
	if actual != u.DeclaredBytes {
		return nil, core.ErrUploadSizeMismatch
	}

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, u.LogicalKey); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT backend_name,size_bytes,COALESCE(NULLIF(physical_key,''),object_key) FROM object_locations WHERE object_key=$1 FOR UPDATE`, u.LogicalKey)
	if err != nil {
		return nil, fmt.Errorf("lock overwritten copies: %w", err)
	}
	var displaced []core.DeletedCopy
	for rows.Next() {
		var d core.DeletedCopy
		if err := rows.Scan(&d.BackendName, &d.SizeBytes, &d.PhysicalKey); err != nil {
			rows.Close()
			return nil, err
		}
		displaced = append(displaced, d)
	}
	rows.Close()
	if _, err := tx.Exec(ctx, `DELETE FROM object_locations WHERE object_key=$1`, u.LogicalKey); err != nil {
		return nil, err
	}
	for _, d := range displaced {
		if _, err := tx.Exec(ctx, `UPDATE backend_quotas SET bytes_used=GREATEST(0,bytes_used-$1),updated_at=NOW() WHERE backend_name=$2`, d.SizeBytes, d.BackendName); err != nil {
			return nil, err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE backend_quotas SET bytes_used=bytes_used+$1,updated_at=NOW()
		WHERE backend_name=$2 AND (bytes_limit=0 OR bytes_used+orphan_bytes+$1<=bytes_limit)`, actual, u.BackendName)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, core.ErrNoSpaceAvailable
	}
	if _, err := tx.Exec(ctx, `INSERT INTO object_locations
		(object_key,backend_name,size_bytes,managed,physical_key,object_version,created_at)
		VALUES($1,$2,$3,TRUE,$4,$5,NOW())`, u.LogicalKey, u.BackendName, actual, u.PhysicalKey, u.ID); err != nil {
		return nil, err
	}
	if err := releasePGReservation(ctx, tx, u, "committed"); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE upload_sessions SET status=$1,actual_bytes=$2,etag=$3,updated_at=NOW() WHERE id=$4`, core.UploadCommitted, actual, etag, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return displaced, nil
}

func releasePGReservation(ctx context.Context, tx pgx.Tx, u *core.UploadSession, status string) error {
	tag, err := tx.Exec(ctx, `UPDATE capacity_reservations SET status=$1,updated_at=NOW() WHERE session_id=$2 AND status='active'`, status, u.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		_, err = tx.Exec(ctx, `UPDATE quota_groups SET reserved_bytes=GREATEST(0,reserved_bytes-$1),updated_at=NOW() WHERE name=$2`, u.ReservedBytes, u.QuotaGroup)
	}
	return err
}

func (s *Store) AbortUpload(ctx context.Context, id, reason string, expired bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	u, err := scanUpload(tx.QueryRow(ctx, `SELECT `+uploadColumns+` FROM upload_sessions WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if u.Status == core.UploadCommitted {
		return core.ErrUploadSessionConflict
	}
	if u.Status == core.UploadAborted || u.Status == core.UploadExpired {
		return nil
	}
	if err := releasePGReservation(ctx, tx, u, "released"); err != nil {
		return err
	}
	status := core.UploadAborted
	if expired {
		status = core.UploadExpired
	}
	_, err = tx.Exec(ctx, `UPDATE upload_sessions SET status=$1,last_error=$2,updated_at=NOW() WHERE id=$3`, status, reason, id)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ListExpiredUploads(ctx context.Context, before time.Time, limit int) ([]core.UploadSession, error) {
	return s.listUploads(ctx, `WHERE status IN ('reserved','url_issued') AND expires_at<$1 ORDER BY expires_at LIMIT $2`, before, limit)
}

func (s *Store) ListRecentUploads(ctx context.Context, limit int) ([]core.UploadSession, error) {
	return s.listUploads(ctx, `ORDER BY created_at DESC LIMIT $1`, limit)
}

func (s *Store) listUploads(ctx context.Context, suffix string, args ...any) ([]core.UploadSession, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+uploadColumns+` FROM upload_sessions `+suffix, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.UploadSession
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *Store) GetCapacityGroupStats(ctx context.Context) ([]core.CapacityGroupStat, error) {
	rows, err := s.pool.Query(ctx, `SELECT q.name,q.raw_limit_bytes,q.static_buffer_bytes,q.dynamic_buffer_bytes,
		COALESCE(SUM(b.bytes_used+b.orphan_bytes),0)::bigint,q.reserved_bytes,q.drift_bytes,q.billing_policy
		FROM quota_groups q LEFT JOIN backend_quota_groups m ON m.quota_group=q.name
		LEFT JOIN backend_quotas b ON b.backend_name=m.backend_name GROUP BY q.name ORDER BY q.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.CapacityGroupStat
	for rows.Next() {
		var v core.CapacityGroupStat
		if err := rows.Scan(&v.Name, &v.RawLimitBytes, &v.StaticBufferBytes, &v.DynamicBufferBytes, &v.CommittedBytes, &v.ReservedBytes, &v.DriftBytes, &v.BillingPolicy); err != nil {
			return nil, err
		}
		if v.RawLimitBytes == 0 {
			v.AvailableBytes = math.MaxInt64
		} else {
			v.AvailableBytes = v.RawLimitBytes - v.StaticBufferBytes - v.DynamicBufferBytes - v.CommittedBytes - v.ReservedBytes - v.DriftBytes
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

var _ core.DirectUploadStore = (*Store)(nil)
