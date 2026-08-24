package sqlite

import (
	"bytes"
	"context"
	"testing"
)

func TestManagedBackendStoreRoundTrip(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	payload := []byte{1, 2, 3, 4}
	if err := store.UpsertManagedBackend(context.Background(), "r2-extra", payload); err != nil {
		t.Fatal(err)
	}
	records, err := store.ListManagedBackends(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Name != "r2-extra" || !bytes.Equal(records[0].EncryptedConfig, payload) {
		t.Fatalf("records = %+v", records)
	}
	if err := store.DeleteManagedBackend(context.Background(), "r2-extra"); err != nil {
		t.Fatal(err)
	}
	records, err = store.ListManagedBackends(context.Background())
	if err != nil || len(records) != 0 {
		t.Fatalf("records after delete = %+v, err=%v", records, err)
	}
}
