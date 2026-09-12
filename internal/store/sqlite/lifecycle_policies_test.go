package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

func TestLifecyclePolicyStoreRoundTrip(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()

	created, err := store.CreateLifecyclePolicy(ctx, "assets/tmp/", 36)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || created.Prefix != "assets/tmp/" || created.ExpirationHours != 36 || !created.Enabled {
		t.Fatalf("created policy = %+v", created)
	}
	if _, err := store.CreateLifecyclePolicy(ctx, "assets/tmp/", 12); !errors.Is(err, core.ErrLifecyclePolicyConflict) {
		t.Fatalf("duplicate create error = %v, want conflict", err)
	}

	disabled, err := store.SetLifecyclePolicyEnabled(ctx, created.ID, false)
	if err != nil || disabled.Enabled {
		t.Fatalf("disabled policy = %+v, err=%v", disabled, err)
	}
	policies, err := store.ListLifecyclePolicies(ctx)
	if err != nil || len(policies) != 1 || policies[0].Enabled {
		t.Fatalf("listed policies = %+v, err=%v", policies, err)
	}

	if deleted, err := store.DeleteLifecyclePolicy(ctx, created.ID); err != nil || !deleted {
		t.Fatalf("delete = %v, err=%v", deleted, err)
	}
	if deleted, err := store.DeleteLifecyclePolicy(ctx, created.ID); err != nil || deleted {
		t.Fatalf("second delete = %v, err=%v", deleted, err)
	}
	if _, err := store.SetLifecyclePolicyEnabled(ctx, created.ID, true); !errors.Is(err, core.ErrLifecyclePolicyNotFound) {
		t.Fatalf("missing update error = %v, want not found", err)
	}
}
