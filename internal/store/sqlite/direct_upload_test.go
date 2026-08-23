package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

//nolint:gocognit // One scenario intentionally proves the reservation lifecycle end to end.
func TestDirectUploadReservationAndCommit(t *testing.T) {
	ctx := context.Background()
	s, err := NewStore(ctx, &config.DatabaseConfig{Driver: "sqlite", Path: filepath.Join(t.TempDir(), "direct.db")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	backends := []config.BackendConfig{{Name: "r2", QuotaBytes: 800, QuotaGroup: "r2-account", RawLimitBytes: 1000, StaticBufferBytes: 100, DynamicBufferBytes: 100, BillingPolicy: "free_capped"}}
	if err := s.SyncQuotaLimits(ctx, backends); err != nil {
		t.Fatal(err)
	}

	p := &core.ReserveUploadParams{ID: "du_one", IdempotencyKey: "idem-one", LogicalKey: "assets/demo.bin", PhysicalKey: "__objects/du_one", DeclaredBytes: 700, ContentType: "application/octet-stream", ExpiresAt: time.Now().Add(time.Minute), Candidates: []string{"r2"}}
	u, err := s.ReserveUpload(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if u.BackendName != "r2" || u.Status != core.UploadReserved {
		t.Fatalf("unexpected session: %#v", u)
	}
	again, err := s.ReserveUpload(ctx, &core.ReserveUploadParams{ID: "different", IdempotencyKey: "idem-one", DeclaredBytes: 1, Candidates: []string{"r2"}})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != u.ID {
		t.Fatalf("idempotency returned %q, want %q", again.ID, u.ID)
	}
	_, err = s.ReserveUpload(ctx, &core.ReserveUploadParams{ID: "du_two", IdempotencyKey: "idem-two", LogicalKey: "assets/two", PhysicalKey: "__objects/du_two", DeclaredBytes: 101, ExpiresAt: time.Now().Add(time.Minute), Candidates: []string{"r2"}})
	if !errors.Is(err, core.ErrNoSpaceAvailable) {
		t.Fatalf("oversubscription error=%v", err)
	}
	if err := s.MarkUploadURLIssued(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitUpload(ctx, u.ID, 700, "etag-1"); err != nil {
		t.Fatal(err)
	}
	locs, err := s.GetAllObjectLocations(ctx, "assets/demo.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(locs) != 1 || locs[0].PhysicalKey != "__objects/du_one" || locs[0].ObjectVersion != "du_one" {
		t.Fatalf("unexpected committed location: %#v", locs)
	}
	stats, err := s.GetCapacityGroupStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].ReservedBytes != 0 || stats[0].CommittedBytes != 700 {
		t.Fatalf("unexpected capacity stats: %#v", stats)
	}
}
