package directupload

import (
	"context"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// MultipartCreateResult identifies the control-plane session. The actual
// upstream upload id is intentionally kept server-side.
type MultipartCreateResult struct {
	Session *core.UploadSession
}

// CreateMultipart reserves the declared final size and starts a native
// multipart upload on the selected upstream backend. No part bodies touch the
// orchestrator.
//
//nolint:gocognit // Reservation recovery and upstream creation must remain a single idempotent workflow.
func (m *Manager) CreateMultipart(ctx context.Context, r CreateRequest) (*MultipartCreateResult, error) {
	session, created, err := m.reserve(ctx, &r, m.cfg.MaxMultipartObjectSize)
	if err != nil {
		return nil, err
	}
	if session.UpstreamUploadID == "" && session.Status != core.UploadReserved {
		return nil, core.ErrUploadSessionConflict
	}
	be, err := m.fleet.GetBackend(session.BackendName)
	if err != nil {
		return nil, fmt.Errorf("selected backend unavailable")
	}
	direct, ok := be.(backend.DirectMultipartBackend)
	if !ok {
		if created {
			_ = m.store.AbortUpload(ctx, session.ID, "backend cannot create multipart upload", false)
		}
		return nil, fmt.Errorf("backend does not support direct multipart")
	}
	if session.UpstreamUploadID == "" {
		upstreamID, createErr := direct.CreateMultipartObject(ctx, session.PhysicalKey, session.ContentType, session.ID)
		if createErr != nil {
			if created {
				_ = m.store.AbortUpload(ctx, session.ID, "create multipart failed", false)
			}
			return nil, createErr
		}
		if err := m.store.SetUploadMultipartID(ctx, session.ID, upstreamID); err != nil {
			_ = direct.AbortMultipartObject(ctx, session.PhysicalKey, upstreamID)
			return nil, err
		}
		session.UpstreamUploadID = upstreamID
	}
	if session.Status == core.UploadReserved {
		if err := m.store.MarkUploadURLIssued(ctx, session.ID); err != nil {
			return nil, err
		}
		session.Status = core.UploadURLIssued
	}
	return &MultipartCreateResult{Session: session}, nil
}

// PresignMultipartPart returns one narrowly-scoped upstream UploadPart URL.
func (m *Manager) PresignMultipartPart(ctx context.Context, id string, partNumber int) (*backend.PresignedRequest, error) {
	if partNumber < 1 || partNumber > 10000 {
		return nil, fmt.Errorf("part_number must be between 1 and 10000")
	}
	session, err := m.store.GetUploadSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if session.Status != core.UploadURLIssued || session.UpstreamUploadID == "" {
		return nil, core.ErrUploadSessionConflict
	}
	if time.Now().UTC().After(session.ExpiresAt) {
		return nil, core.ErrUploadSessionExpired
	}
	be, err := m.fleet.GetBackend(session.BackendName)
	if err != nil {
		return nil, err
	}
	direct, ok := be.(backend.DirectMultipartBackend)
	if !ok {
		return nil, fmt.Errorf("backend does not support direct multipart")
	}
	return direct.PresignUploadPart(ctx, session.PhysicalKey, session.UpstreamUploadID, partNumber, m.cfg.PresignTTL)
}

// CompleteMultipart asks the upstream provider to assemble client-uploaded
// parts, verifies the declared size/session marker, then atomically commits the
// logical index and quota counters.
func (m *Manager) CompleteMultipart(ctx context.Context, id string, parts []backend.CompletedPart) (*core.UploadSession, error) {
	if err := validateCompletedParts(parts); err != nil {
		return nil, err
	}
	session, err := m.store.GetUploadSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if session.Status == core.UploadCommitted {
		return session, nil
	}
	if session.Status != core.UploadURLIssued || session.UpstreamUploadID == "" {
		return nil, core.ErrUploadSessionConflict
	}
	if time.Now().UTC().After(session.ExpiresAt) {
		return nil, core.ErrUploadSessionExpired
	}
	be, err := m.fleet.GetBackend(session.BackendName)
	if err != nil {
		return nil, err
	}
	direct, ok := be.(backend.DirectMultipartBackend)
	if !ok {
		return nil, fmt.Errorf("backend does not support direct multipart")
	}
	if _, err := direct.CompleteMultipartObject(ctx, session.PhysicalKey, session.UpstreamUploadID, parts); err != nil {
		// A retry after the upstream assembled successfully but before our DB
		// commit sees NoSuchUpload. Head + marker verification below is the
		// authoritative recovery path for that split outcome.
		head, headErr := be.HeadObject(ctx, session.PhysicalKey)
		if headErr != nil || head.Size != session.DeclaredBytes || head.Metadata["upload-id"] != session.ID {
			return nil, err
		}
	}
	return m.completeVerified(ctx, session)
}

func validateCompletedParts(parts []backend.CompletedPart) error {
	if len(parts) == 0 || len(parts) > 10000 {
		return fmt.Errorf("parts must contain between 1 and 10000 entries")
	}
	previous := 0
	for i := range parts {
		if parts[i].PartNumber <= previous || parts[i].PartNumber > 10000 || parts[i].ETag == "" {
			return fmt.Errorf("parts must have ascending unique part_number values and non-empty etags")
		}
		previous = parts[i].PartNumber
	}
	return nil
}
