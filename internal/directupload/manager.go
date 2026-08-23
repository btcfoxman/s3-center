// Package directupload implements capacity-reserved, browser-to-S3 uploads.
package directupload

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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
	backends map[string]backend.ObjectBackend
	order    []string
	buckets  map[string]struct{}
	log      *slog.Logger
}

//nolint:gocritic // The manager owns an immutable configuration snapshot.
func New(cfg config.DirectUploadConfig, store core.MetadataStore, backends map[string]backend.ObjectBackend, order []string, buckets []config.BucketConfig) (*Manager, error) {
	direct, ok := store.(core.DirectUploadStore)
	if !ok {
		return nil, fmt.Errorf("configured store does not support direct uploads")
	}
	allowed := make(map[string]struct{}, len(buckets))
	for _, b := range buckets {
		allowed[b.Name] = struct{}{}
	}
	return &Manager{cfg: cfg, store: direct, metadata: store, backends: backends, order: order, buckets: allowed, log: slog.Default().With("component", "direct_upload")}, nil
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
	if _, ok := m.buckets[r.Bucket]; !ok {
		return nil, fmt.Errorf("unknown bucket")
	}
	r.Key = strings.TrimPrefix(strings.TrimSpace(r.Key), "/")
	if r.Key == "" || strings.Contains(r.Key, "..") {
		return nil, fmt.Errorf("invalid object key")
	}
	if r.Size < 0 || r.Size > m.cfg.MaxObjectSize {
		return nil, fmt.Errorf("object size must be between 0 and %d", m.cfg.MaxObjectSize)
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return nil, fmt.Errorf("Idempotency-Key is required")
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	logical := r.Bucket + "/" + r.Key
	physical := "__objects/" + id
	session, err := m.store.ReserveUpload(ctx, &core.ReserveUploadParams{ID: id, IdempotencyKey: r.IdempotencyKey, LogicalKey: logical, PhysicalKey: physical, DeclaredBytes: r.Size, ContentType: r.ContentType, ExpiresAt: time.Now().UTC().Add(m.cfg.SessionTTL), Candidates: m.order})
	if err != nil {
		return nil, err
	}
	created := session.ID == id
	if session.Status != core.UploadReserved && session.Status != core.UploadURLIssued {
		return nil, core.ErrUploadSessionConflict
	}
	if time.Now().UTC().After(session.ExpiresAt) {
		return nil, core.ErrUploadSessionExpired
	}
	be, ok := m.backends[session.BackendName]
	if !ok {
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
	be, ok := m.backends[u.BackendName]
	if !ok {
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
	m.deleteOrEnqueue(ctx, u.BackendName, u.PhysicalKey, "direct_upload_aborted", u.DeclaredBytes)
	return m.store.AbortUpload(ctx, id, "aborted by client", false)
}

func (m *Manager) deleteOrEnqueue(ctx context.Context, backendName, key, reason string, size int64) {
	be, ok := m.backends[backendName]
	if !ok {
		return
	}
	err := be.DeleteObject(ctx, key)
	if err == nil || backend.IsNotFound(err) {
		return
	}
	if e := m.metadata.EnqueueCleanup(ctx, backendName, key, reason, size); e == nil && size > 0 {
		_ = m.metadata.IncrementOrphanBytes(ctx, backendName, size)
	}
}

func (m *Manager) Reap(ctx context.Context) error {
	sessions, err := m.store.ListExpiredUploads(ctx, time.Now().UTC(), 100)
	if err != nil {
		return err
	}
	for i := range sessions {
		u := &sessions[i]
		be, ok := m.backends[u.BackendName]
		if !ok {
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
