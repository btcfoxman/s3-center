package directupload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/sqlite"
)

type fakeDirectBackend struct {
	*backendtest.InMemory
	mu            sync.Mutex
	multipart     map[string]multipartFixture
	completedSize int64
	lastPresigned string
	lastExpiry    time.Duration
}

type multipartFixture struct {
	key         string
	contentType string
	sessionID   string
}

func newFakeDirectBackend() *fakeDirectBackend {
	return &fakeDirectBackend{InMemory: backendtest.NewInMemory(), multipart: make(map[string]multipartFixture)}
}

func (f *fakeDirectBackend) PresignPutObject(_ context.Context, key, _ string, _ string, expires time.Duration) (*backend.PresignedPut, error) {
	return &backend.PresignedPut{Method: http.MethodPut, URL: "https://upstream.invalid/" + key, ExpiresAt: time.Now().Add(expires)}, nil
}

func (f *fakeDirectBackend) PresignGetObject(_ context.Context, key string, expires time.Duration) (*backend.PresignedRequest, error) {
	f.mu.Lock()
	f.lastPresigned = key
	f.lastExpiry = expires
	f.mu.Unlock()
	return &backend.PresignedRequest{Method: http.MethodGet, URL: "https://upstream.invalid/" + key, ExpiresAt: time.Now().Add(expires)}, nil
}

func (f *fakeDirectBackend) PresignHeadObject(_ context.Context, key string, expires time.Duration) (*backend.PresignedRequest, error) {
	return &backend.PresignedRequest{Method: http.MethodHead, URL: "https://upstream.invalid/" + key, ExpiresAt: time.Now().Add(expires)}, nil
}

func (f *fakeDirectBackend) CreateMultipartObject(_ context.Context, key, contentType, sessionID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "upstream-" + sessionID
	f.multipart[id] = multipartFixture{key: key, contentType: contentType, sessionID: sessionID}
	return id, nil
}

func (f *fakeDirectBackend) PresignUploadPart(_ context.Context, key, uploadID string, partNumber int, expires time.Duration) (*backend.PresignedRequest, error) {
	f.mu.Lock()
	_, ok := f.multipart[uploadID]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown multipart upload")
	}
	return &backend.PresignedRequest{Method: http.MethodPut, URL: fmt.Sprintf("https://upstream.invalid/%s?partNumber=%d", key, partNumber), ExpiresAt: time.Now().Add(expires)}, nil
}

func (f *fakeDirectBackend) CompleteMultipartObject(ctx context.Context, key, uploadID string, _ []backend.CompletedPart) (string, error) {
	f.mu.Lock()
	fixture, ok := f.multipart[uploadID]
	size := f.completedSize
	if ok {
		delete(f.multipart, uploadID)
	}
	f.mu.Unlock()
	if !ok || fixture.key != key {
		return "", fmt.Errorf("unknown multipart upload")
	}
	etag, err := f.PutObject(ctx, key, bytes.NewReader(make([]byte, size)), size, fixture.contentType, map[string]string{"upload-id": fixture.sessionID})
	return etag, err
}

func (f *fakeDirectBackend) AbortMultipartObject(_ context.Context, _ string, uploadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.multipart, uploadID)
	return nil
}

func newManagerFixture(t *testing.T) (*Manager, *sqlite.Store, *fakeDirectBackend) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.NewStore(ctx, &config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "metadata.db")}, nil)
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(store.Close)
	backendConfig := config.BackendConfig{
		Name: "r2-one", QuotaBytes: 1 << 30, QuotaGroup: "cf-account-one",
		RawLimitBytes: 1 << 30, BillingPolicy: "free_capped",
	}
	if err := store.SyncQuotaLimits(ctx, []config.BackendConfig{backendConfig}); err != nil {
		t.Fatalf("sync quota limits: %v", err)
	}
	fake := newFakeDirectBackend()
	cfg := config.DirectUploadConfig{
		Enabled: true, PresignTTL: 10 * time.Minute, AccessPresignTTL: 3 * time.Hour, SessionTTL: 10 * time.Minute,
		MaxObjectSize: 256 << 20, MaxMultipartObjectSize: 5 << 40,
	}
	manager, err := NewWithFleet(&cfg, store, staticFleet{
		backends: map[string]backend.ObjectBackend{"r2-one": fake}, order: []string{"r2-one"},
	}, []config.BucketConfig{{Name: "assets"}})
	if err != nil {
		t.Fatalf("create direct manager: %v", err)
	}
	return manager, store, fake
}

func TestAccessPresignTTLCanBeChangedAndPersists(t *testing.T) {
	t.Parallel()
	manager, store, fake := newManagerFixture(t)
	ctx := context.Background()

	ttl, overridden, err := manager.AccessPresignTTL(ctx)
	if err != nil || overridden || ttl != 3*time.Hour {
		t.Fatalf("initial TTL = (%v, %v, %v), want (3h, false, nil)", ttl, overridden, err)
	}
	if err := manager.SetAccessPresignTTL(ctx, 12*time.Hour); err != nil {
		t.Fatalf("set access TTL: %v", err)
	}
	ttl, overridden, err = manager.AccessPresignTTL(ctx)
	if err != nil || !overridden || ttl != 12*time.Hour {
		t.Fatalf("updated TTL = (%v, %v, %v), want (12h, true, nil)", ttl, overridden, err)
	}

	if _, err := store.ImportObject(ctx, "assets/reports/today.pdf", "r2-one", 12, false, nil); err != nil {
		t.Fatalf("import object: %v", err)
	}
	if _, err := manager.PresignAccess(ctx, &AccessRequest{
		Bucket: "assets", Key: "reports/today.pdf", Method: http.MethodGet,
	}); err != nil {
		t.Fatalf("presign with configured default: %v", err)
	}
	fake.mu.Lock()
	lastExpiry := fake.lastExpiry
	fake.mu.Unlock()
	if lastExpiry != 12*time.Hour {
		t.Fatalf("backend expiry = %v, want 12h", lastExpiry)
	}
	if _, err := manager.PresignAccess(ctx, &AccessRequest{
		Bucket: "assets", Key: "reports/today.pdf", Method: http.MethodGet, Expires: 13 * time.Hour,
	}); err == nil || !strings.Contains(err.Error(), "configured maximum") {
		t.Fatalf("13h request error = %v, want configured maximum rejection", err)
	}

	reloaded, err := NewWithFleet(&config.DirectUploadConfig{AccessPresignTTL: 3 * time.Hour}, store, staticFleet{
		backends: map[string]backend.ObjectBackend{"r2-one": fake}, order: []string{"r2-one"},
	}, []config.BucketConfig{{Name: "assets"}})
	if err != nil {
		t.Fatalf("reload manager: %v", err)
	}
	if ttl, overridden, err = reloaded.AccessPresignTTL(ctx); err != nil || !overridden || ttl != 12*time.Hour {
		t.Fatalf("persisted TTL = (%v, %v, %v), want (12h, true, nil)", ttl, overridden, err)
	}
}

func TestSetAccessPresignTTLValidatesHourRange(t *testing.T) {
	t.Parallel()
	manager, _, _ := newManagerFixture(t)
	for _, ttl := range []time.Duration{0, 30 * time.Minute, 90 * time.Minute, 169 * time.Hour} {
		if err := manager.SetAccessPresignTTL(t.Context(), ttl); err == nil {
			t.Errorf("SetAccessPresignTTL(%v) succeeded", ttl)
		}
	}
}

func TestAccessPresignTTLRejectsCorruptStoredValue(t *testing.T) {
	t.Parallel()
	manager, store, _ := newManagerFixture(t)
	if err := store.SetRuntimeSetting(t.Context(), accessPresignTTLSettingKey, "9223372036854775807"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.AccessPresignTTL(t.Context()); !errors.Is(err, ErrAccessSettingsUnavailable) {
		t.Fatalf("AccessPresignTTL error = %v, want ErrAccessSettingsUnavailable", err)
	}
}

func TestPhysicalObjectKeyPreservesOnlySafeExtensions(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"videos/result.MP4":               "__objects/du_test.mp4",
		"archive/file.tar.gz":             "__objects/du_test.gz",
		"extensionless":                   "__objects/du_test",
		"unsafe/file.bad-suffix":          "__objects/du_test",
		"too-long/file.abcdefghijklmnopq": "__objects/du_test",
	}
	for logicalKey, expected := range tests {
		if actual := physicalObjectKey("du_test", logicalKey); actual != expected {
			t.Fatalf("physicalObjectKey(%q) = %q, want %q", logicalKey, actual, expected)
		}
	}
}

func TestCreateSkipsWritePausedBackend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := sqlite.NewStore(ctx, &config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "metadata.db")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	backends := []config.BackendConfig{
		{Name: "r2-paused", QuotaBytes: 1 << 30, QuotaGroup: "cf-paused", RawLimitBytes: 1 << 30, BillingPolicy: "free_capped"},
		{Name: "r2-active", QuotaBytes: 1 << 30, QuotaGroup: "cf-active", RawLimitBytes: 1 << 30, BillingPolicy: "free_capped"},
	}
	if err := store.SyncQuotaLimits(ctx, backends); err != nil {
		t.Fatal(err)
	}
	fleet := staticFleet{
		backends: map[string]backend.ObjectBackend{"r2-paused": newFakeDirectBackend(), "r2-active": newFakeDirectBackend()},
		order:    []string{"r2-paused", "r2-active"}, writePaused: map[string]bool{"r2-paused": true},
	}
	manager, err := NewWithFleet(&config.DirectUploadConfig{
		Enabled: true, PresignTTL: time.Hour, SessionTTL: time.Hour, MaxObjectSize: 1 << 20,
	}, store, fleet, []config.BucketConfig{{Name: "assets"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Create(ctx, CreateRequest{
		Bucket: "assets", Key: "new.bin", Size: 4, IdempotencyKey: "pause-routing-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session.BackendName != "r2-active" {
		t.Fatalf("selected backend = %q, want r2-active", result.Session.BackendName)
	}

	fleet.writePaused["r2-active"] = true
	manager.fleet = fleet
	_, err = manager.Create(ctx, CreateRequest{
		Bucket: "assets", Key: "blocked.bin", Size: 4, IdempotencyKey: "pause-routing-2",
	})
	if !errors.Is(err, core.ErrNoSpaceAvailable) {
		t.Fatalf("all-paused create error = %v, want ErrNoSpaceAvailable", err)
	}
}

func TestPresignAccessAndListUseMetadataIndexOnly(t *testing.T) {
	t.Parallel()
	manager, store, fake := newManagerFixture(t)
	ctx := context.Background()
	inserted, err := store.ImportObject(ctx, "assets/media/demo.mp4", "r2-one", 42, false, nil)
	if err != nil || !inserted {
		t.Fatalf("import indexed object: inserted=%v err=%v", inserted, err)
	}

	result, err := manager.PresignAccess(ctx, &AccessRequest{
		Bucket: "assets", Key: "media/demo.mp4", Method: http.MethodGet, Expires: time.Minute,
	})
	if err != nil {
		t.Fatalf("presign indexed object: %v", err)
	}
	if result.Request.Method != http.MethodGet || !strings.HasPrefix(result.Request.URL, "https://upstream.invalid/") {
		t.Fatalf("unexpected direct request: %+v", result.Request)
	}
	fake.mu.Lock()
	presignedKey := fake.lastPresigned
	fake.mu.Unlock()
	if presignedKey != "assets/media/demo.mp4" {
		t.Fatalf("presigned physical key = %q", presignedKey)
	}
	if fake.Has("assets/media/demo.mp4") {
		t.Fatal("test unexpectedly wrote object bytes while resolving access")
	}

	listed, err := manager.ListIndex(ctx, "assets", "media/", "", 100)
	if err != nil {
		t.Fatalf("list index: %v", err)
	}
	if len(listed.Objects) != 1 || listed.Objects[0].Key != "media/demo.mp4" || listed.Objects[0].SizeBytes != 42 {
		t.Fatalf("unexpected indexed list: %+v", listed)
	}
}

func TestDirectMultipartCommitsOnlyAfterUpstreamHeadVerification(t *testing.T) {
	t.Parallel()
	manager, store, fake := newManagerFixture(t)
	ctx := context.Background()
	fake.completedSize = 6

	created, err := manager.CreateMultipart(ctx, CreateRequest{
		Bucket: "assets", Key: "large/movie.bin", Size: 6,
		ContentType: "application/octet-stream", IdempotencyKey: "multipart-fixture-1",
	})
	if err != nil {
		t.Fatalf("create multipart: %v", err)
	}
	if created.Session.UpstreamUploadID == "" {
		t.Fatal("native upstream multipart id was not persisted")
	}
	part, err := manager.PresignMultipartPart(ctx, created.Session.ID, 1)
	if err != nil {
		t.Fatalf("presign multipart part: %v", err)
	}
	if part.Method != http.MethodPut || !strings.Contains(part.URL, "partNumber=1") {
		t.Fatalf("unexpected part request: %+v", part)
	}

	completed, err := manager.CompleteMultipart(ctx, created.Session.ID, []backend.CompletedPart{{PartNumber: 1, ETag: `"part-etag"`}})
	if err != nil {
		t.Fatalf("complete multipart: %v", err)
	}
	if completed.Status != "committed" || completed.ActualBytes != 6 {
		t.Fatalf("unexpected completed session: %+v", completed)
	}
	locations, err := store.GetAllObjectLocations(ctx, "assets/large/movie.bin")
	if err != nil || len(locations) != 1 {
		t.Fatalf("get committed location: locations=%+v err=%v", locations, err)
	}
	if !strings.HasPrefix(locations[0].PhysicalKey, "__objects/du_") || !strings.HasSuffix(locations[0].PhysicalKey, ".bin") {
		t.Fatalf("unexpected physical key %q", locations[0].PhysicalKey)
	}
	quota, err := store.GetQuotaStats(ctx)
	if err != nil {
		t.Fatalf("get quota stats: %v", err)
	}
	if quota["r2-one"].BytesUsed != 6 {
		t.Fatalf("committed quota = %d, want 6", quota["r2-one"].BytesUsed)
	}
}
