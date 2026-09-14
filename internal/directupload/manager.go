// Package directupload implements capacity-reserved, browser-to-S3 uploads.
package directupload

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

type CreateRequest struct {
	Bucket, Key, ContentType, IdempotencyKey string
	Size                                     int64
}
type CreateResult struct {
	Session *core.UploadSession
	Upload  *backend.PresignedPut
}

type Manager struct {
	cfg      config.DirectUploadConfig
	store    core.DirectUploadStore
	metadata core.MetadataStore
	settings core.RuntimeSettingsStore
	fleet    Fleet
	buckets  map[string]struct{}
	log      *slog.Logger
}

// Fleet is the live backend registry surface direct uploads need.
type Fleet interface {
	GetBackend(name string) (backend.ObjectBackend, error)
	BackendOrder() []string
	MaxObjectSize(name string) int64
	IsBackendWritePaused(name string) bool
}

type staticFleet struct {
	backends    map[string]backend.ObjectBackend
	order       []string
	writePaused map[string]bool
}

func (f staticFleet) GetBackend(name string) (backend.ObjectBackend, error) {
	be, ok := f.backends[name]
	if !ok {
		return nil, fmt.Errorf("backend %s not found", name)
	}
	return be, nil
}
func (f staticFleet) BackendOrder() []string                { return append([]string(nil), f.order...) }
func (f staticFleet) MaxObjectSize(string) int64            { return 0 }
func (f staticFleet) IsBackendWritePaused(name string) bool { return f.writePaused[name] }

//nolint:gocritic // The manager owns an immutable configuration snapshot.
func New(cfg config.DirectUploadConfig, store core.MetadataStore, backends map[string]backend.ObjectBackend, order []string, buckets []config.BucketConfig) (*Manager, error) {
	return NewWithFleet(&cfg, store, staticFleet{backends: backends, order: order}, buckets)
}

// NewWithFleet binds the manager to a runtime registry so newly managed
// backends immediately become direct-upload candidates.
func NewWithFleet(cfg *config.DirectUploadConfig, store core.MetadataStore, fleet Fleet, buckets []config.BucketConfig) (*Manager, error) {
	direct, ok := store.(core.DirectUploadStore)
	if !ok {
		return nil, fmt.Errorf("configured store does not support direct uploads")
	}
	allowed := make(map[string]struct{}, len(buckets))
	for _, b := range buckets {
		allowed[b.Name] = struct{}{}
	}
	settings, _ := store.(core.RuntimeSettingsStore)
	return &Manager{cfg: *cfg, store: direct, metadata: store, settings: settings, fleet: fleet, buckets: allowed, log: slog.Default().With("component", "direct_upload")}, nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "du_" + hex.EncodeToString(b[:]), nil
}

//nolint:gocognit // Reservation, capability checks, and presigning form one fail-closed transaction boundary.
func (m *Manager) Create(ctx context.Context, r CreateRequest) (*CreateResult, error) {
	session, created, err := m.reserve(ctx, &r, m.cfg.MaxObjectSize)
	if err != nil {
		return nil, err
	}
	if session.UpstreamUploadID != "" {
		return nil, core.ErrUploadSessionConflict
	}
	be, err := m.fleet.GetBackend(session.BackendName)
	if err != nil {
		if created {
			_ = m.store.AbortUpload(ctx, session.ID, "selected backend is unavailable", false)
		}
		return nil, fmt.Errorf("selected backend unavailable")
	}
	direct, ok := be.(backend.DirectUploadBackend)
	if !ok {
		if created {
			_ = m.store.AbortUpload(ctx, session.ID, "backend cannot presign uploads", false)
		}
		return nil, fmt.Errorf("backend does not support direct upload")
	}
	presigned, err := direct.PresignPutObject(ctx, session.PhysicalKey, session.ContentType, session.ID, m.cfg.PresignTTL)
	if err != nil {
		if created {
			_ = m.store.AbortUpload(ctx, session.ID, "presign failed", false)
		}
		return nil, err
	}
	if session.Status == core.UploadReserved {
		if err := m.store.MarkUploadURLIssued(ctx, session.ID); err != nil {
			return nil, err
		}
		session.Status = core.UploadURLIssued
	}
	return &CreateResult{Session: session, Upload: presigned}, nil
}

//nolint:gocognit // Validation, backend eligibility, and transactional reservation form one boundary.
func (m *Manager) reserve(ctx context.Context, r *CreateRequest, maxSize int64) (*core.UploadSession, bool, error) {
	if _, ok := m.buckets[r.Bucket]; !ok {
		return nil, false, fmt.Errorf("unknown bucket")
	}
	r.Key = strings.TrimPrefix(strings.TrimSpace(r.Key), "/")
	if r.Key == "" || strings.Contains(r.Key, "..") {
		return nil, false, fmt.Errorf("invalid object key")
	}
	if r.Size < 0 || r.Size > maxSize {
		return nil, false, fmt.Errorf("object size must be between 0 and %d", maxSize)
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return nil, false, fmt.Errorf("Idempotency-Key is required")
	}
	id, err := newID()
	if err != nil {
		return nil, false, err
	}
	logical := r.Bucket + "/" + r.Key
	physical := physicalObjectKey(id, r.Key)
	candidates := make([]string, 0, len(m.fleet.BackendOrder()))
	for _, name := range m.fleet.BackendOrder() {
		if m.fleet.IsBackendWritePaused(name) {
			continue
		}
		if limit := m.fleet.MaxObjectSize(name); limit == 0 || r.Size <= limit {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return nil, false, core.ErrNoSpaceAvailable
	}
	session, err := m.store.ReserveUpload(ctx, &core.ReserveUploadParams{ID: id, IdempotencyKey: r.IdempotencyKey, LogicalKey: logical, PhysicalKey: physical, DeclaredBytes: r.Size, ContentType: r.ContentType, ExpiresAt: time.Now().UTC().Add(m.cfg.SessionTTL), Candidates: candidates})
	if err != nil {
		return nil, false, err
	}
	created := session.ID == id
	if session.Status != core.UploadReserved && session.Status != core.UploadURLIssued {
		return nil, created, core.ErrUploadSessionConflict
	}
	if time.Now().UTC().After(session.ExpiresAt) {
		return nil, created, core.ErrUploadSessionExpired
	}
	return session, created, nil
}

// physicalObjectKey keeps a short, safe filename extension on newly-created
// final-backend objects. The session id remains the unique identity, while the
// suffix lets public media URLs retain their type without changing how legacy
// extensionless physical keys are read.
func physicalObjectKey(id, logicalKey string) string {
	extension := strings.ToLower(path.Ext(logicalKey))
	if len(extension) < 2 || len(extension) > 17 {
		extension = ""
	} else {
		for _, character := range extension[1:] {
			if (character < 'a' || character > 'z') &&
				(character < '0' || character > '9') {
				extension = ""
				break
			}
		}
	}
	return "__objects/" + id + extension
}

func (m *Manager) Get(ctx context.Context, id string) (*core.UploadSession, error) {
	return m.store.GetUploadSession(ctx, id)
}

func (m *Manager) Complete(ctx context.Context, id string) (*core.UploadSession, error) {
	u, err := m.store.GetUploadSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if u.Status == core.UploadCommitted {
		return u, nil
	}
	if time.Now().After(u.ExpiresAt) {
		return nil, core.ErrUploadSessionExpired
	}
	return m.completeVerified(ctx, u)
}

func (m *Manager) completeVerified(ctx context.Context, u *core.UploadSession) (*core.UploadSession, error) {
	be, lookupErr := m.fleet.GetBackend(u.BackendName)
	if lookupErr != nil {
		return nil, fmt.Errorf("backend %s unavailable", u.BackendName)
	}
	head, err := be.HeadObject(ctx, u.PhysicalKey)
	if err != nil {
		return nil, fmt.Errorf("verify uploaded object: %w", err)
	}
	if head.Size != u.DeclaredBytes {
		return nil, core.ErrUploadSizeMismatch
	}
	if marker := head.Metadata["upload-id"]; marker != u.ID {
		return nil, fmt.Errorf("upload session marker mismatch")
	}
	if u.ContentType != "" && head.ContentType != "" && head.ContentType != u.ContentType {
		return nil, fmt.Errorf("uploaded content type mismatch")
	}
	displaced, err := m.store.CommitUpload(ctx, u.ID, head.Size, head.ETag)
	if err != nil {
		return nil, err
	}
	for _, d := range displaced {
		key := d.PhysicalKey
		if key == "" {
			key = u.LogicalKey
		}
		m.deleteOrEnqueue(ctx, d.BackendName, key, "direct_overwrite_displaced", d.SizeBytes)
	}
	return m.store.GetUploadSession(ctx, u.ID)
}

func (m *Manager) Abort(ctx context.Context, id string) error {
	u, err := m.store.GetUploadSession(ctx, id)
	if err != nil {
		return err
	}
	if u.Status == core.UploadCommitted {
		return core.ErrUploadSessionConflict
	}
	if u.UpstreamUploadID != "" {
		be, lookupErr := m.fleet.GetBackend(u.BackendName)
		if lookupErr != nil {
			return lookupErr
		}
		direct, ok := be.(backend.DirectMultipartBackend)
		if !ok {
			return fmt.Errorf("backend does not support direct multipart")
		}
		if err := direct.AbortMultipartObject(ctx, u.PhysicalKey, u.UpstreamUploadID); err != nil && !backend.IsNotFound(err) {
			return err
		}
	} else {
		m.deleteOrEnqueue(ctx, u.BackendName, u.PhysicalKey, "direct_upload_aborted", u.DeclaredBytes)
	}
	return m.store.AbortUpload(ctx, id, "aborted by client", false)
}

func (m *Manager) deleteOrEnqueue(ctx context.Context, backendName, key, reason string, size int64) {
	be, err := m.fleet.GetBackend(backendName)
	if err != nil {
		return
	}
	err = be.DeleteObject(ctx, key)
	if err == nil || backend.IsNotFound(err) {
		return
	}
	if e := m.metadata.EnqueueCleanup(ctx, backendName, key, reason, size); e == nil && size > 0 {
		_ = m.metadata.IncrementOrphanBytes(ctx, backendName, size)
	}
}

//nolint:gocognit // Recovery distinguishes native multipart, completed direct PUTs, and abandoned sessions.
func (m *Manager) Reap(ctx context.Context) error {
	sessions, err := m.store.ListExpiredUploads(ctx, time.Now().UTC(), 100)
	if err != nil {
		return err
	}
	for i := range sessions {
		u := &sessions[i]
		be, lookupErr := m.fleet.GetBackend(u.BackendName)
		if lookupErr != nil {
			continue
		}
		if u.UpstreamUploadID != "" {
			head, headErr := be.HeadObject(ctx, u.PhysicalKey)
			if headErr == nil && head.Size == u.DeclaredBytes && head.Metadata["upload-id"] == u.ID {
				if _, e := m.completeVerified(ctx, u); e != nil {
					m.log.WarnContext(ctx, "expired multipart auto-commit failed", "id", u.ID, "error", e)
				}
				continue
			}
			if direct, ok := be.(backend.DirectMultipartBackend); ok {
				if e := direct.AbortMultipartObject(ctx, u.PhysicalKey, u.UpstreamUploadID); e != nil && !backend.IsNotFound(e) {
					m.log.WarnContext(ctx, "expired multipart abort failed", "id", u.ID, "error", e)
					continue
				}
				_ = m.store.AbortUpload(ctx, u.ID, "expired before multipart completed", true)
			}
			continue
		}
		head, e := be.HeadObject(ctx, u.PhysicalKey)
		switch {
		case e == nil && head.Size == u.DeclaredBytes:
			if _, e = m.completeVerified(ctx, u); e != nil {
				m.log.WarnContext(ctx, "expired upload auto-commit failed", "id", u.ID, "error", e)
			}
		case e != nil && backend.IsNotFound(e):
			_ = m.store.AbortUpload(ctx, u.ID, "expired before upload completed", true)
		default:
			m.deleteOrEnqueue(ctx, u.BackendName, u.PhysicalKey, "direct_upload_expired", u.DeclaredBytes)
			_ = m.store.AbortUpload(ctx, u.ID, "expired upload failed verification", true)
		}
	}
	return nil
}

func (m *Manager) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.cfg.ReaperInterval)
	defer ticker.Stop()
	for {
		if err := m.Reap(ctx); err != nil && !errors.Is(err, context.Canceled) {
			m.log.ErrorContext(ctx, "direct upload reaper failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func (m *Manager) Recent(ctx context.Context, limit int) ([]core.UploadSession, error) {
	return m.store.ListRecentUploads(ctx, limit)
}
func (m *Manager) Capacity(ctx context.Context) ([]core.CapacityGroupStat, error) {
	return m.store.GetCapacityGroupStats(ctx)
}
