-- -----------------------------------------------------------------------------
-- Quota Queries
--
-- Author: Alex Freidah
--
-- sqlc-input definitions for backend_quotas - the per-backend bytes_used,
-- bytes_limit, and orphan_bytes counters. The increment query is guarded
-- (WHERE bytes_used + delta <= bytes_limit) so an INSERT exceeding the
-- limit touches zero rows and the engine returns ErrNoSpaceAvailable. Lock
-- acquisition order across multi-row updates is enforced at the call site
-- (sorted backend_name) to avoid deadlock.
-- -----------------------------------------------------------------------------

-- name: UpsertQuotaLimit :exec
INSERT INTO backend_quotas (backend_name, bytes_limit, bytes_used, updated_at)
VALUES ($1, $2, 0, NOW())
ON CONFLICT (backend_name) DO UPDATE SET
    bytes_limit = $2,
    updated_at = NOW();

-- name: GetBackendAvailableSpace :one
-- Both the physical backend cap and its shared provider/account group cap must
-- admit the write. This prevents two buckets in one provider account from each
-- consuming the account's full free-tier allowance.
WITH multipart_inflight AS (
    SELECT mu.backend_name, SUM(mp.size_bytes) AS inflight
    FROM multipart_uploads mu
    JOIN multipart_parts mp ON mp.upload_id = mu.upload_id
    GROUP BY mu.backend_name
), backend_space AS (
    SELECT q.backend_name,
           CASE WHEN q.bytes_limit = 0 THEN 9223372036854775807
                ELSE q.bytes_limit - q.bytes_used - q.orphan_bytes - COALESCE(mi.inflight, 0)
           END::bigint AS available
    FROM backend_quotas q
    LEFT JOIN multipart_inflight mi ON mi.backend_name = q.backend_name
    WHERE q.backend_name = $1
), group_space AS (
    SELECT m.backend_name,
           CASE WHEN g.raw_limit_bytes = 0 THEN 9223372036854775807
                ELSE g.raw_limit_bytes - g.static_buffer_bytes - g.dynamic_buffer_bytes
                     - g.reserved_bytes - g.drift_bytes
                     - COALESCE((
                         SELECT SUM(b.bytes_used + b.orphan_bytes + COALESCE(mi.inflight, 0))
                         FROM backend_quotas b
                         JOIN backend_quota_groups bm ON bm.backend_name = b.backend_name
                         LEFT JOIN multipart_inflight mi ON mi.backend_name = b.backend_name
                         WHERE bm.quota_group = g.name
                       ), 0)
           END::bigint AS available
    FROM backend_quota_groups m
    JOIN quota_groups g ON g.name = m.quota_group
    WHERE m.backend_name = $1
)
SELECT LEAST(b.available, COALESCE(g.available, b.available))::bigint AS available
FROM backend_space b
LEFT JOIN group_space g ON g.backend_name = b.backend_name;

-- name: GetLeastUtilizedBackend :one
-- Finds the backend with the lowest utilization ratio that has enough space.
WITH multipart_inflight AS (
    SELECT mu.backend_name, SUM(mp.size_bytes) AS inflight
    FROM multipart_uploads mu
    JOIN multipart_parts mp ON mp.upload_id = mu.upload_id
    GROUP BY mu.backend_name
), group_usage AS (
    SELECT bm.quota_group,
           COALESCE(SUM(b.bytes_used + b.orphan_bytes + COALESCE(mi.inflight, 0)), 0)::bigint AS committed
    FROM backend_quotas b
    JOIN backend_quota_groups bm ON bm.backend_name = b.backend_name
    LEFT JOIN multipart_inflight mi ON mi.backend_name = b.backend_name
    GROUP BY bm.quota_group
), candidates AS (
    SELECT q.backend_name, q.bytes_limit, q.bytes_used, q.orphan_bytes,
           LEAST(
               CASE WHEN q.bytes_limit = 0 THEN 9223372036854775807
                    ELSE q.bytes_limit - q.bytes_used - q.orphan_bytes - COALESCE(mi.inflight, 0)
               END,
               CASE WHEN g.raw_limit_bytes = 0 THEN 9223372036854775807
                    ELSE g.raw_limit_bytes - g.static_buffer_bytes - g.dynamic_buffer_bytes
                         - g.reserved_bytes - g.drift_bytes - COALESCE(gu.committed, 0)
               END
           )::bigint AS available
    FROM backend_quotas q
    LEFT JOIN multipart_inflight mi ON mi.backend_name = q.backend_name
    JOIN backend_quota_groups bm ON bm.backend_name = q.backend_name
    JOIN quota_groups g ON g.name = bm.quota_group
    LEFT JOIN group_usage gu ON gu.quota_group = g.name
    WHERE q.backend_name = ANY(@backend_names::text[])
)
SELECT q.backend_name, q.available
FROM candidates q
WHERE q.available >= @min_size::bigint
ORDER BY CASE WHEN q.bytes_limit = 0 THEN 0
              ELSE (q.bytes_used + q.orphan_bytes)::float8 / q.bytes_limit::float8
         END ASC
LIMIT 1;

-- name: LockQuotaGroupForBackend :one
-- Serializes normal writes with direct-upload reservations and with writes to
-- another bucket in the same provider account.
SELECT g.name
FROM backend_quota_groups m
JOIN quota_groups g ON g.name = m.quota_group
WHERE m.backend_name = $1
FOR UPDATE OF g;

-- name: GetAllQuotaStats :many
SELECT backend_name, bytes_used, bytes_limit, orphan_bytes, updated_at
FROM backend_quotas;

-- name: GetObjectCountsByBackend :many
SELECT backend_name, COUNT(*) AS object_count
FROM object_locations
GROUP BY backend_name;

-- name: GetActiveMultipartCountsByBackend :many
SELECT backend_name, COUNT(*) AS upload_count
FROM multipart_uploads
GROUP BY backend_name;

-- name: GetUnverifiedObjectCountsByBackend :many
SELECT backend_name, COUNT(*) AS object_count
FROM object_locations
WHERE content_hash IS NULL
GROUP BY backend_name;

-- name: IncrementQuota :execrows
UPDATE backend_quotas AS target
SET bytes_used = target.bytes_used + @amount, updated_at = NOW()
WHERE target.backend_name = @backend_name
  AND (target.bytes_limit = 0 OR target.bytes_used + target.orphan_bytes + @amount <= target.bytes_limit)
  AND (
      NOT EXISTS (SELECT 1 FROM backend_quota_groups WHERE backend_name = @backend_name)
      OR EXISTS (
          SELECT 1
          FROM backend_quota_groups m
          JOIN quota_groups g ON g.name = m.quota_group
          WHERE m.backend_name = @backend_name
            AND (
                g.raw_limit_bytes = 0
                OR COALESCE((
                       SELECT SUM(b.bytes_used + b.orphan_bytes)
                       FROM backend_quotas b
                       JOIN backend_quota_groups bm ON bm.backend_name = b.backend_name
                       WHERE bm.quota_group = g.name
                   ), 0) + g.reserved_bytes + g.drift_bytes + @amount
                   <= g.raw_limit_bytes - g.static_buffer_bytes - g.dynamic_buffer_bytes
            )
      )
  );

-- name: DecrementQuota :exec
UPDATE backend_quotas
SET bytes_used = GREATEST(0, bytes_used - @amount), updated_at = NOW()
WHERE backend_name = @backend_name;

-- name: AdjustBackendBytesUsed :exec
-- Applies a signed delta to bytes_used without the bytes_limit guard
-- IncrementQuota uses, because the on-disk byte count is reality and
-- the counter must follow it whether or not the limit would otherwise
-- be exceeded. GREATEST(0, ...) clamps so a delta-arithmetic bug
-- cannot drive the counter negative. Used by MarkObjectEncrypted and
-- MarkObjectDecrypted to keep bytes_used in step with size_bytes
-- after the bulk encrypt-existing / decrypt-existing rewrite path
-- changes the on-disk size.
UPDATE backend_quotas
SET bytes_used = GREATEST(0, bytes_used + @delta), updated_at = NOW()
WHERE backend_name = @backend_name;

-- name: GetObjectSizeBytes :one
-- Returns the current size_bytes of an object_locations row. Used
-- inside MarkObjectDecrypted so the caller can compute the size
-- delta against the row that is about to be overwritten without
-- needing the old ciphertext size to flow through the API.
SELECT size_bytes FROM object_locations
WHERE object_key = $1 AND backend_name = $2;

-- name: IncrementOrphanBytes :exec
UPDATE backend_quotas
SET orphan_bytes = orphan_bytes + @amount, updated_at = NOW()
WHERE backend_name = @backend_name;

-- name: DecrementOrphanBytes :exec
UPDATE backend_quotas
SET orphan_bytes = GREATEST(0, orphan_bytes - @amount), updated_at = NOW()
WHERE backend_name = @backend_name;

-- name: SumObjectSizesByBackend :many
-- Authoritative per-backend byte total from the object ledger. Used by usage
-- reconciliation to recompute bytes_used, which is otherwise an incrementally
-- maintained counter that drifts if any mutation path misses an adjustment.
SELECT backend_name, COALESCE(SUM(size_bytes), 0)::bigint AS total_bytes
FROM object_locations
GROUP BY backend_name;

-- name: SetBackendBytesUsed :exec
-- Overwrites bytes_used with an authoritative value (no bytes_limit guard,
-- because the recomputed total is reality and the counter must follow it).
UPDATE backend_quotas
SET bytes_used = @bytes_used, updated_at = NOW()
WHERE backend_name = @backend_name;

-- name: DeleteQuota :exec
DELETE FROM backend_quotas WHERE backend_name = $1;
