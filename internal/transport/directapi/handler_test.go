package directapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

func TestTokenAllowsBucketAndPrefix(t *testing.T) {
	t.Parallel()
	tok := &token{
		buckets:  map[string]struct{}{"assets": {}},
		prefixes: []string{"tenant-a/", "shared/public/"},
	}
	tests := []struct {
		bucket, key string
		want        bool
	}{
		{bucket: "assets", key: "tenant-a/file.bin", want: true},
		{bucket: "assets", key: "shared/public/logo.svg", want: true},
		{bucket: "assets", key: "tenant-b/private.bin", want: false},
		{bucket: "private", key: "tenant-a/file.bin", want: false},
		{bucket: "assets", key: "", want: false},
	}
	for _, test := range tests {
		if got := tok.allows(test.bucket, test.key); got != test.want {
			t.Errorf("allows(%q, %q) = %v, want %v", test.bucket, test.key, got, test.want)
		}
	}
}

func TestDecodeRequestRejectsTrailingJSON(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads", strings.NewReader(`{"bucket":"assets"} {"bucket":"other"}`))
	w := httptest.NewRecorder()
	var value map[string]any
	if decodeRequest(w, req, &value) {
		t.Fatal("decodeRequest accepted multiple JSON values")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestAuthorizeUploadSessionRechecksResourcePolicy(t *testing.T) {
	t.Parallel()
	tok := &token{buckets: map[string]struct{}{"assets": {}}, prefixes: []string{"tenant-a/"}}

	allowed := httptest.NewRecorder()
	if !authorizeUploadSession(allowed, tok, &core.UploadSession{LogicalKey: "assets/tenant-a/video.mp4"}) {
		t.Fatal("expected authorized session")
	}

	denied := httptest.NewRecorder()
	if authorizeUploadSession(denied, tok, &core.UploadSession{LogicalKey: "assets/tenant-b/video.mp4"}) {
		t.Fatal("expected cross-prefix session to be denied")
	}
	if denied.Code != http.StatusForbidden {
		t.Fatalf("denied status = %d, want %d", denied.Code, http.StatusForbidden)
	}
}
