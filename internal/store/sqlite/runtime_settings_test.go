package sqlite

import "testing"

func TestRuntimeSettingsRoundTrip(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()

	if value, found, err := store.GetRuntimeSetting(ctx, "presign.access_ttl_seconds"); err != nil || found || value != "" {
		t.Fatalf("missing setting = (%q, %v, %v)", value, found, err)
	}
	if err := store.SetRuntimeSetting(ctx, "presign.access_ttl_seconds", "10800"); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	if err := store.SetRuntimeSetting(ctx, "presign.access_ttl_seconds", "43200"); err != nil {
		t.Fatalf("update setting: %v", err)
	}
	value, found, err := store.GetRuntimeSetting(ctx, "presign.access_ttl_seconds")
	if err != nil || !found || value != "43200" {
		t.Fatalf("stored setting = (%q, %v, %v), want (43200, true, nil)", value, found, err)
	}
	if err := store.SetRuntimeSetting(ctx, " ", "value"); err == nil {
		t.Fatal("empty setting key was accepted")
	}
}
