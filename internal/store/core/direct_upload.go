package core

import (
	"context"
	"time"
)

const (
	UploadReserved  = "reserved"
	UploadURLIssued = "url_issued"
	UploadCommitted = "committed"
	UploadAborted   = "aborted"
	UploadExpired   = "expired"
	UploadFailed    = "failed"
)

type UploadSession struct {
	ID             string
	IdempotencyKey string
	LogicalKey     string
	PhysicalKey    string
	BackendName    string
	QuotaGroup     string
	Status         string
	DeclaredBytes  int64
	ReservedBytes  int64
	ActualBytes    int64
	ContentType    string
	ETag           string
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastError      string
}

type CapacityGroupStat struct {
	Name               string
	RawLimitBytes      int64
	StaticBufferBytes  int64
	DynamicBufferBytes int64
	CommittedBytes     int64
	ReservedBytes      int64
	DriftBytes         int64
	AvailableBytes     int64
	BillingPolicy      string
}

type ReserveUploadParams struct {
	ID             string
	IdempotencyKey string
	LogicalKey     string
	PhysicalKey    string
	DeclaredBytes  int64
	ContentType    string
	ExpiresAt      time.Time
	Candidates     []string
}

// DirectUploadStore is implemented by both database drivers. Reserve and
// commit are transactional so concurrent creates cannot oversubscribe a shared
// provider quota group and an overwrite never exposes half-written metadata.
type DirectUploadStore interface {
	ReserveUpload(context.Context, *ReserveUploadParams) (*UploadSession, error)
	MarkUploadURLIssued(context.Context, string) error
	GetUploadSession(context.Context, string) (*UploadSession, error)
	CommitUpload(context.Context, string, int64, string) ([]DeletedCopy, error)
	AbortUpload(context.Context, string, string, bool) error
	ListExpiredUploads(context.Context, time.Time, int) ([]UploadSession, error)
	ListRecentUploads(context.Context, int) ([]UploadSession, error)
	GetCapacityGroupStats(context.Context) ([]CapacityGroupStat, error)
}
