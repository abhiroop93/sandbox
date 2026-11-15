package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abhiroop93/sandbox/internal/lifecycle"
	"github.com/abhiroop93/sandbox/internal/store"
)

// The OpenSandbox SDK raises KeyError if these are missing (create/get response models).
func TestViewHasFieldsTheSDKRequires(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	sb := &store.Sandbox{ID: "id1", Image: "python:3.12", State: store.Running, Entrypoint: []string{"tail", "-f", "/dev/null"},
		Metadata: map[string]string{"k": "v"}, CreatedAt: time.Now(), UpdatedAt: time.Now(), ExpiresAt: &exp}
	b, _ := json.Marshal(view(sb))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"id", "status", "entrypoint", "createdAt", "expiresAt", "image", "metadata"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing %q in %s", k, b)
		}
	}
	if m["status"].(map[string]any)["state"] != "Running" {
		t.Fatal("status.state must be set")
	}
	if _, err := time.Parse(time.RFC3339Nano, m["createdAt"].(string)); err != nil {
		t.Fatal("createdAt must be RFC3339")
	}
	if m["image"].(map[string]any)["uri"] != "python:3.12" {
		t.Fatal("image must be {uri}")
	}
}

func TestViewNilEntrypointIsEmptyList(t *testing.T) {
	b, _ := json.Marshal(view(&store.Sandbox{ID: "x", State: store.Pending}))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["entrypoint"].([]any); !ok {
		t.Fatalf("entrypoint must be a list, got %s", b)
	}
}

// Error mapping drives SDK behaviour (429 retryable with Retry-After; 404 POD_IP_NOT_AVAILABLE retried on endpoints).
func TestErrorMapping(t *testing.T) {
	s := &Server{log: slog.New(slog.DiscardHandler)}
	cases := []struct {
		err  error
		code int
		c    string
	}{
		{store.ErrNotFound, 404, "SANDBOX_NOT_FOUND"},
		{store.ErrConflict, 409, "INVALID_STATE"},
		{fmt.Errorf("%w: 20/20", store.ErrQuota), 429, "QUOTA_EXCEEDED"},
		{fmt.Errorf("%w: fqdn", lifecycle.ErrInvalid), 400, "INVALID_REQUEST"},
		{errors.New("boom"), 500, "INTERNAL_ERROR"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		s.fail(w, c.err)
		var body map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != c.code || body["code"] != c.c || body["message"] == "" {
			t.Fatalf("%v → %d %v", c.err, w.Code, body)
		}
		if c.code == 429 && w.Header().Get("Retry-After") == "" {
			t.Fatal("429 must carry Retry-After")
		}
	}
}

func TestAuthRejectsMissingKeyAndAdminRequiresToken(t *testing.T) {
	s := &Server{log: slog.New(slog.DiscardHandler), adminToken: "adm"}
	h := s.Handler()
	for _, tc := range []struct {
		method, path string
		hdr          map[string]string
		want         int
	}{
		{"GET", "/v1/sandboxes", nil, 401},
		{"POST", "/v1/sandboxes", nil, 401},
		{"GET", "/admin/v1/keys", nil, 401},
		{"GET", "/admin/v1/keys", map[string]string{"X-SRVM-ADMIN-TOKEN": "wrong"}, 401},
		{"GET", "/health", nil, 200},
		{"POST", "/v1/metrics/events", nil, 204},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		for k, v := range tc.hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("%s %s → %d, want %d", tc.method, tc.path, w.Code, tc.want)
		}
	}
	// empty admin token on the server disables the admin API entirely
	s2 := &Server{log: slog.New(slog.DiscardHandler)}
	req := httptest.NewRequest("GET", "/admin/v1/keys", nil)
	req.Header.Set("X-SRVM-ADMIN-TOKEN", "")
	w := httptest.NewRecorder()
	s2.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("admin API must be closed when no admin token is configured")
	}
}
