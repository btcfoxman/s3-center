package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

type uploadRowScanner interface{ Scan(...any) error }

const uploadColumns = `id,idempotency_key,logical_key,physical_key,backend_name,quota_group,
	status,declared_bytes,reserved_bytes,actual_bytes,content_type,etag,upstream_upload_id,expires_at,created_at,updated_at,last_error`

func scanUpload(row uploadRowScanner) (*core.UploadSession, error) {
	var u core.UploadSession
	var actual sql.NullInt64
	var contentType, etag, upstreamUploadID, lastError sql.NullString
	var expires, created, updated string
	err := row.Scan(&u.ID, &u.IdempotencyKey, &u.LogicalKey, &u.PhysicalKey, &u.BackendName, &u.QuotaGroup,
		&u.Status, &u.DeclaredBytes, &u.ReservedBytes, &actual, &contentType, &etag, &upstreamUploadID, &expires, &created, &updated, &lastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrUploadSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	u.ActualBytes = actual.Int64
	u.ContentType = contentType.String
	u.ETag = etag.String
	u.UpstreamUploadID = upstreamUploadID.String
	u.LastError = lastError.String
	if u.ExpiresAt, err = parseTime(expires); err != nil {
		return nil, err
	}
	if u.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if u.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	return &u, nil
}

//nolint:gocognit // Keeping selection and reservation in one transaction is the oversubscription invariant.
func (s *Store) ReserveUpload(ctx context.Context, p *core.ReserveUploadParams) (*core.UploadSession, error) {
	var result *core.UploadSession
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if existing, e := scanUpload(tx.QueryRowContext(ctx, `SELECT `+uploadColumns+` FROM upload_sessions WHERE idempotency_key=?`, p.IdempotencyKey)); e == nil {
			result = existing
			return nil
		} else if !errors.Is(e, core.ErrUploadSessionNotFound) {
			return e
		}
		type candidate struct {
			backend, group string
			remaining      int64
		}
		best := candidate{remaining: math.MaxInt64}
		for _, name := range p.Candidates {
			var group string
			var raw, staticBuffer, dynamicBuffer, reserved, drift, committed int64
			e := tx.QueryRowContext(ctx, `SELECT q.name,q.raw_limit_bytes,q.static_buffer_bytes,q.dynamic_buffer_bytes,q.reserved_bytes,q.drift_bytes,
				COALESCE((SELECT SUM(b.bytes_used+b.orphan_bytes) FROM backend_quotas b JOIN backend_quota_groups x ON x.backend_name=b.backend_name WHERE x.quota_group=q.name),0)
				FROM quota_groups q JOIN backend_quota_groups m ON m.quota_group=q.name WHERE m.backend_name=?`, name).Scan(&group, &raw, &staticBuffer, &dynamicBuffer, &reserved, &drift, &committed)
			if errors.Is(e, sql.ErrNoRows) {
				continue
			}
			if e != nil {
				return e
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
			return core.ErrNoSpaceAvailable
		}
		stamp := now()
		if _, e := tx.ExecContext(ctx, `UPDATE quota_groups SET reserved_bytes=reserved_bytes+?,updated_at=? WHERE name=?`, p.DeclaredBytes, stamp, best.group); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO upload_sessions(id,idempotency_key,logical_key,physical_key,backend_name,quota_group,status,declared_bytes,reserved_bytes,content_type,expires_at,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.ID, p.IdempotencyKey, p.LogicalKey, p.PhysicalKey, best.backend, best.group, core.UploadReserved, p.DeclaredBytes, p.DeclaredBytes, nullableString(p.ContentType), formatTime(p.ExpiresAt), stamp, stamp); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO capacity_reservations(id,session_id,quota_group,backend_name,bytes,status,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,'active',?,?,?)`, p.ID, p.ID, best.group, best.backend, p.DeclaredBytes, formatTime(p.ExpiresAt), stamp, stamp); e != nil {
			return e
		}
		result = &core.UploadSession{ID: p.ID, IdempotencyKey: p.IdempotencyKey, LogicalKey: p.LogicalKey, PhysicalKey: p.PhysicalKey, BackendName: best.backend, QuotaGroup: best.group, Status: core.UploadReserved, DeclaredBytes: p.DeclaredBytes, ReservedBytes: p.DeclaredBytes, ContentType: p.ContentType, ExpiresAt: p.ExpiresAt}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reserve upload: %w", err)
	}
	return result, nil
}

func (s *Store) MarkUploadURLIssued(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE upload_sessions SET status=?,updated_at=? WHERE id=? AND status=?`, core.UploadURLIssued, now(), id, core.UploadReserved)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return core.ErrUploadSessionConflict
	}
	return nil
}
func (s *Store) SetUploadMultipartID(ctx context.Context, id, upstreamID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE upload_sessions SET upstream_upload_id=?,updated_at=? WHERE id=? AND upstream_upload_id IS NULL AND status=?`, upstreamID, now(), id, core.UploadReserved)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return core.ErrUploadSessionConflict
	}
	return nil
}
func (s *Store) GetUploadSession(ctx context.Context, id string) (*core.UploadSession, error) {
	return scanUpload(s.db.QueryRowContext(ctx, `SELECT `+uploadColumns+` FROM upload_sessions WHERE id=?`, id))
}

func releaseReservation(ctx context.Context, tx *sql.Tx, u *core.UploadSession, status string) error {
	res, err := tx.ExecContext(ctx, `UPDATE capacity_reservations SET status=?,updated_at=? WHERE session_id=? AND status='active'`, status, now(), u.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		_, err = tx.ExecContext(ctx, `UPDATE quota_groups SET reserved_bytes=MAX(0,reserved_bytes-?),updated_at=? WHERE name=?`, u.ReservedBytes, now(), u.QuotaGroup)
	}
	return err
}

//nolint:gocognit // The overwrite catalog switch must remain a single auditable transaction.
func (s *Store) CommitUpload(ctx context.Context, id string, actual int64, etag string) ([]core.DeletedCopy, error) {
	var displaced []core.DeletedCopy
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		u, err := scanUpload(tx.QueryRowContext(ctx, `SELECT `+uploadColumns+` FROM upload_sessions WHERE id=?`, id))
		if err != nil {
			return err
		}
		if u.Status == core.UploadCommitted {
			return nil
		}
		if u.Status != core.UploadURLIssued && u.Status != core.UploadReserved && u.Status != core.UploadExpired {
			return core.ErrUploadSessionConflict
		}
		if actual != u.DeclaredBytes {
			return core.ErrUploadSizeMismatch
		}
		rows, err := tx.QueryContext(ctx, `SELECT backend_name,size_bytes,COALESCE(NULLIF(physical_key,''),object_key) FROM object_locations WHERE object_key=?`, u.LogicalKey)
		if err != nil {
			return err
		}
		for rows.Next() {
			var d core.DeletedCopy
			if err := rows.Scan(&d.BackendName, &d.SizeBytes, &d.PhysicalKey); err != nil {
				rows.Close()
				return err
			}
			displaced = append(displaced, d)
		}
		rows.Close()
		if _, err = tx.ExecContext(ctx, `DELETE FROM object_locations WHERE object_key=?`, u.LogicalKey); err != nil {
			return err
		}
		for _, d := range displaced {
			if _, err = tx.ExecContext(ctx, `UPDATE backend_quotas SET bytes_used=MAX(0,bytes_used-?),updated_at=? WHERE backend_name=?`, d.SizeBytes, now(), d.BackendName); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE backend_quotas SET bytes_used=bytes_used+?,updated_at=? WHERE backend_name=? AND (bytes_limit=0 OR bytes_used+orphan_bytes+?<=bytes_limit)`, actual, now(), u.BackendName, actual)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return core.ErrNoSpaceAvailable
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO object_locations(object_key,backend_name,size_bytes,managed,physical_key,object_version,created_at) VALUES(?,?,?,?,?,?,?)`, u.LogicalKey, u.BackendName, actual, 1, u.PhysicalKey, u.ID, now()); err != nil {
			return err
		}
		if err := releaseReservation(ctx, tx, u, "committed"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE upload_sessions SET status=?,actual_bytes=?,etag=?,updated_at=? WHERE id=?`, core.UploadCommitted, actual, etag, now(), id)
		return err
	})
	return displaced, err
}

func (s *Store) AbortUpload(ctx context.Context, id, reason string, expired bool) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		u, err := scanUpload(tx.QueryRowContext(ctx, `SELECT `+uploadColumns+` FROM upload_sessions WHERE id=?`, id))
		if err != nil {
			return err
		}
		if u.Status == core.UploadCommitted {
			return core.ErrUploadSessionConflict
		}
		if u.Status == core.UploadAborted || u.Status == core.UploadExpired {
			return nil
		}
		if err := releaseReservation(ctx, tx, u, "released"); err != nil {
			return err
		}
		status := core.UploadAborted
		if expired {
			status = core.UploadExpired
		}
		_, err = tx.ExecContext(ctx, `UPDATE upload_sessions SET status=?,last_error=?,updated_at=? WHERE id=?`, status, reason, now(), id)
		return err
	})
}

func (s *Store) ListExpiredUploads(ctx context.Context, before time.Time, limit int) ([]core.UploadSession, error) {
	return s.listUploads(ctx, `WHERE status IN ('reserved','url_issued') AND expires_at<? ORDER BY expires_at LIMIT ?`, formatTime(before), limit)
}
func (s *Store) ListRecentUploads(ctx context.Context, limit int) ([]core.UploadSession, error) {
	return s.listUploads(ctx, `ORDER BY created_at DESC LIMIT ?`, limit)
}
func (s *Store) listUploads(ctx context.Context, suffix string, args ...any) ([]core.UploadSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+uploadColumns+` FROM upload_sessions `+suffix, args...)
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
	rows, err := s.db.QueryContext(ctx, `SELECT q.name,q.raw_limit_bytes,q.static_buffer_bytes,q.dynamic_buffer_bytes,COALESCE(SUM(b.bytes_used+b.orphan_bytes),0),q.reserved_bytes,q.drift_bytes,q.billing_policy FROM quota_groups q LEFT JOIN backend_quota_groups m ON m.quota_group=q.name LEFT JOIN backend_quotas b ON b.backend_name=m.backend_name GROUP BY q.name ORDER BY q.name`)
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
