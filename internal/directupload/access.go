package directupload

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// ErrDirectAccessUnsupported means the indexed bytes require an orchestrator
// transformation (for example envelope decryption or decompression) and are
// therefore unsafe to expose as a direct upstream object.
var ErrDirectAccessUnsupported = errors.New("object cannot be accessed directly")

// AccessRequest identifies a logical object and the upstream operation to
// authorize. Expires may be shorter than the configured maximum but never
// longer.
type AccessRequest struct {
	Bucket  string
	Key     string
	Method  string
	Expires time.Duration
}

// AccessResult is safe to return to an authorized client. Physical placement
// details remain internal; the presigned URL itself contains only the upstream
// bucket/key and a time-limited signature.
type AccessResult struct {
	LogicalKey string
	Backend    string
	SizeBytes  int64
	Request    *backend.PresignedRequest
}

// ListedObject is one logical index entry returned without touching object
// data on any upstream backend.
type ListedObject struct {
	Key       string    `json:"key"`
	SizeBytes int64     `json:"size_bytes"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListResult is the index-mode listing projection exposed by the control API.
type ListResult struct {
	Objects      []ListedObject
	IsTruncated  bool
	NextStartKey string
}

// PresignAccess resolves a logical key through the metadata index and mints a
// URL against one actual upstream copy. It never opens an object body.
//
//nolint:gocognit // Location failover and capability checks are one fail-closed authorization path.
func (m *Manager) PresignAccess(ctx context.Context, r *AccessRequest) (*AccessResult, error) {
	logical, err := m.logicalKey(r.Bucket, r.Key)
	if err != nil {
		return nil, err
	}
	method := strings.ToUpper(strings.TrimSpace(r.Method))
	if method != http.MethodGet && method != http.MethodHead {
		return nil, fmt.Errorf("method must be GET or HEAD")
	}
	expires := r.Expires
	if expires <= 0 {
		expires = m.cfg.PresignTTL
	}
	if expires > m.cfg.PresignTTL {
		return nil, fmt.Errorf("expiry exceeds configured maximum of %s", m.cfg.PresignTTL)
	}

	locations, err := m.metadata.GetAllObjectLocations(ctx, logical)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i := range locations {
		location := &locations[i]
		if location.Encrypted || location.CompressionAlgorithm != "" {
			lastErr = ErrDirectAccessUnsupported
			continue
		}
		be, lookupErr := m.fleet.GetBackend(location.BackendName)
		if lookupErr != nil {
			lastErr = lookupErr
			continue
		}
		direct, ok := be.(backend.DirectObjectBackend)
		if !ok {
			lastErr = ErrDirectAccessUnsupported
			continue
		}
		physicalKey := location.PhysicalKey
		if physicalKey == "" {
			physicalKey = logical
		}
		var request *backend.PresignedRequest
		switch method {
		case http.MethodGet:
			request, err = direct.PresignGetObject(ctx, physicalKey, expires)
		case http.MethodHead:
			request, err = direct.PresignHeadObject(ctx, physicalKey, expires)
		}
		if err != nil {
			lastErr = err
			continue
		}
		return &AccessResult{LogicalKey: logical, Backend: location.BackendName, SizeBytes: location.SizeBytes, Request: request}, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("no directly accessible copy: %w", lastErr)
	}
	return nil, core.ErrObjectNotFound
}

// ListIndex returns logical entries from the database only; the prefix and
// continuation key exposed to callers never include the virtual bucket name.
func (m *Manager) ListIndex(ctx context.Context, bucket, prefix, startAfter string, maxKeys int) (*ListResult, error) {
	bucket = strings.TrimSpace(bucket)
	if _, ok := m.buckets[bucket]; !ok {
		return nil, fmt.Errorf("unknown bucket")
	}
	prefix = strings.TrimPrefix(strings.TrimSpace(prefix), "/")
	startAfter = strings.TrimPrefix(strings.TrimSpace(startAfter), "/")
	if strings.Contains(prefix, "..") || strings.Contains(startAfter, "..") {
		return nil, fmt.Errorf("invalid object prefix")
	}
	if maxKeys <= 0 || maxKeys > 1000 {
		maxKeys = 1000
	}
	bucketPrefix := bucket + "/"
	page, err := m.metadata.ListObjects(ctx, bucketPrefix+prefix, prefixKey(bucketPrefix, startAfter), maxKeys)
	if err != nil {
		return nil, err
	}
	result := &ListResult{Objects: make([]ListedObject, 0, len(page.Objects)), IsTruncated: page.IsTruncated}
	for i := range page.Objects {
		entry := &page.Objects[i]
		key := strings.TrimPrefix(entry.ObjectKey, bucketPrefix)
		result.Objects = append(result.Objects, ListedObject{Key: key, SizeBytes: entry.SizeBytes, UpdatedAt: entry.CreatedAt})
	}
	result.NextStartKey = strings.TrimPrefix(page.NextContinuationToken, bucketPrefix)
	return result, nil
}

func prefixKey(bucketPrefix, key string) string {
	if key == "" {
		return ""
	}
	return bucketPrefix + key
}

func (m *Manager) logicalKey(bucket, key string) (string, error) {
	bucket = strings.TrimSpace(bucket)
	if _, ok := m.buckets[bucket]; !ok {
		return "", fmt.Errorf("unknown bucket")
	}
	key = strings.TrimPrefix(strings.TrimSpace(key), "/")
	if key == "" || strings.Contains(key, "..") {
		return "", fmt.Errorf("invalid object key")
	}
	return bucket + "/" + key, nil
}

// LogicalObjectKey validates a public bucket/key pair and returns the internal
// namespaced key used by metadata-coordinated control operations.
func (m *Manager) LogicalObjectKey(bucket, key string) (string, error) {
	return m.logicalKey(bucket, key)
}
