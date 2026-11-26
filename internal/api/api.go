// Package api serves the OpenSandbox Lifecycle API (specs/sandbox-lifecycle.yml, subset) so the unmodified
// OpenSandbox SDKs work against this server, plus the execd reverse proxy and a small admin API for tenants/keys.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/abhiroop93/sandbox/internal/execd"
	"github.com/abhiroop93/sandbox/internal/lifecycle"
	"github.com/abhiroop93/sandbox/internal/store"
)

const keyHeader = "OPEN-SANDBOX-API-KEY"

type Server struct {
	st             *store.Store
	lc             *lifecycle.Manager
	adminToken     string
	publicEndpoint string // host[:port] clients use to reach this server (no scheme); empty = request Host
	log            *slog.Logger
	authCache      sync.Map // key → cachedAuth
	proxyTransport *http.Transport
}

type cachedAuth struct {
	tenant string
	until  time.Time
}

func New(st *store.Store, lc *lifecycle.Manager, adminToken, publicEndpoint string, log *slog.Logger) *Server {
	return &Server{st: st, lc: lc, adminToken: adminToken, publicEndpoint: publicEndpoint, log: log,
		proxyTransport: &http.Transport{MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second,
			ResponseHeaderTimeout: 0, DisableCompression: true}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "healthy"})
	})

	// Lifecycle (tenant-authenticated)
	mux.Handle("POST /v1/sandboxes", s.auth(s.create))
	mux.Handle("GET /v1/sandboxes", s.auth(s.list))
	mux.Handle("GET /v1/sandboxes/{id}", s.auth(s.get))
	mux.Handle("DELETE /v1/sandboxes/{id}", s.auth(s.delete))
	mux.Handle("POST /v1/sandboxes/{id}/pause", s.auth(s.pause))
	mux.Handle("POST /v1/sandboxes/{id}/resume", s.auth(s.resume))
	mux.Handle("POST /v1/sandboxes/{id}/renew-expiration", s.auth(s.renew))
	mux.Handle("GET /v1/sandboxes/{id}/endpoints/{port}", s.auth(s.endpoint))
	mux.Handle("/v1/sandboxes/{id}/proxy/{port}", s.auth(s.proxy))
	mux.Handle("/v1/sandboxes/{id}/proxy/{port}/{path...}", s.auth(s.proxy))
	mux.HandleFunc("POST /v1/metrics/events", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })

	// Admin (operator-only; X-SANDBOX-ADMIN-TOKEN)
	mux.Handle("POST /admin/v1/tenants", s.admin(s.adminCreateTenant))
	mux.Handle("POST /admin/v1/tenants/{tenant}/keys", s.admin(s.adminCreateKey))
	mux.Handle("GET /admin/v1/keys", s.admin(s.adminListKeys))
	mux.Handle("DELETE /admin/v1/keys/{key}", s.admin(s.adminRevokeKey))
	mux.Handle("GET /admin/v1/usage", s.admin(s.adminUsage))
	return logReq(s.log, mux)
}

// ---------------------------------------------------------------- auth

type ctxKey struct{}

func tenantOf(r *http.Request) string { return r.Context().Value(ctxKey{}).(string) }

func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(keyHeader)
		if key == "" {
			writeErr(w, 401, "MISSING_API_KEY", "missing "+keyHeader+" header")
			return
		}
		var tenant string
		if c, ok := s.authCache.Load(key); ok && time.Now().Before(c.(cachedAuth).until) {
			tenant = c.(cachedAuth).tenant
		} else {
			t, err := s.st.Authenticate(r.Context(), key)
			if err != nil {
				writeErr(w, 401, "INVALID_API_KEY", "invalid API key")
				return
			}
			tenant = t
			s.authCache.Store(key, cachedAuth{tenant: t, until: time.Now().Add(30 * time.Second)}) // revocation lag ≤ 30s
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, tenant)))
	})
}

func (s *Server) admin(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-SANDBOX-ADMIN-TOKEN")
		if s.adminToken == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.adminToken)) != 1 {
			writeErr(w, 401, "UNAUTHORIZED", "admin token required")
			return
		}
		h(w, r)
	})
}

// ---------------------------------------------------------------- lifecycle handlers

type createReq struct {
	Image struct {
		URI string `json:"uri"`
	} `json:"image"`
	Timeout    *int64                   `json:"timeout"`
	Entrypoint []string                 `json:"entrypoint"`
	Env        map[string]string        `json:"env"`
	Metadata   map[string]string        `json:"metadata"`
	Network    *lifecycle.NetworkPolicy `json:"networkPolicy"`
	Volumes    []any                    `json:"volumes"`
	SnapshotID *string                  `json:"snapshotId"`
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "INVALID_REQUEST", "invalid JSON: "+err.Error())
		return
	}
	if req.Image.URI == "" {
		writeErr(w, 400, "INVALID_REQUEST", "image.uri is required")
		return
	}
	if len(req.Volumes) > 0 || req.SnapshotID != nil {
		writeErr(w, 400, "NOT_SUPPORTED", "volumes and snapshotId are not supported by this server yet; /workspace persists across pause/resume")
		return
	}
	spec := lifecycle.CreateSpec{Image: req.Image.URI, Entrypoint: req.Entrypoint, Env: req.Env, Metadata: req.Metadata, Network: req.Network}
	if len(spec.Entrypoint) == 0 {
		spec.Entrypoint = []string{"tail", "-f", "/dev/null"}
	}
	if req.Timeout != nil {
		d := time.Duration(*req.Timeout) * time.Second
		spec.Timeout = &d
	}
	sb, err := s.lc.Create(r.Context(), tenantOf(r), spec)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, view(sb)) // SDK requires exactly 202
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	sb, err := s.st.Get(r.Context(), tenantOf(r), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, view(sb))
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 200 {
		size = 20
	}
	md := map[string]string{}
	if m := q.Get("metadata"); m != "" {
		if vals, err := url.ParseQuery(m); err == nil {
			for k, v := range vals {
				md[k] = v[0]
			}
		}
	}
	states := q["state"]
	if states == nil {
		states = []string{}
	}
	items, total, err := s.st.List(r.Context(), tenantOf(r), states, md, size, (page-1)*size)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]any, 0, len(items))
	for _, sb := range items {
		out = append(out, view(sb))
	}
	pages := int(math.Ceil(float64(total) / float64(size)))
	writeJSON(w, 200, map[string]any{"items": out, "pagination": map[string]any{
		"page": page, "pageSize": size, "totalItems": total, "totalPages": pages, "hasNextPage": page < pages}})
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	if err := s.lc.Delete(r.Context(), tenantOf(r), r.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrConflict) { // already terminated: idempotent delete
			w.WriteHeader(204)
			return
		}
		s.fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	if err := s.lc.Pause(r.Context(), tenantOf(r), r.PathValue("id")); err != nil {
		s.stateConflict(w, r, err, "pause", "Running")
		return
	}
	w.WriteHeader(202)
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	if err := s.lc.Resume(r.Context(), tenantOf(r), r.PathValue("id")); err != nil {
		s.stateConflict(w, r, err, "resume", "Paused")
		return
	}
	w.WriteHeader(202)
}

func (s *Server) renew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ExpiresAt.IsZero() {
		writeErr(w, 400, "INVALID_REQUEST", "expiresAt (RFC3339) is required")
		return
	}
	sb, err := s.lc.Renew(r.Context(), tenantOf(r), r.PathValue("id"), req.ExpiresAt)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"expiresAt": sb.ExpiresAt.Format(time.RFC3339Nano)})
}

// endpoint returns the proxy URL for a sandbox port. While the sandbox is not Running the SDK retries only on
// 404 + KUBERNETES::POD_IP_NOT_AVAILABLE, which is exactly how async create/resume are surfaced.
func (s *Server) endpoint(w http.ResponseWriter, r *http.Request) {
	sb, err := s.st.Get(r.Context(), tenantOf(r), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	switch sb.State {
	case store.Running:
	case store.Pending, store.Resuming:
		writeErr(w, 404, "KUBERNETES::POD_IP_NOT_AVAILABLE", "sandbox is "+sb.State)
		return
	case store.Failed:
		writeErr(w, 409, "SANDBOX_FAILED", sb.StateMessage)
		return
	default:
		writeErr(w, 409, "INVALID_STATE", "sandbox is "+sb.State)
		return
	}
	port := r.PathValue("port")
	host := s.publicEndpoint
	if host == "" {
		host = r.Host
	}
	writeJSON(w, 200, map[string]any{"endpoint": fmt.Sprintf("%s/v1/sandboxes/%s/proxy/%s", host, sb.ID, port), "headers": map[string]string{}})
}

// proxy streams traffic to a port inside the sandbox. It strips client credentials and injects the
// per-generation execd token, so neither the API key reaches the sandbox nor the execd token the client.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request) {
	sb, err := s.st.Get(r.Context(), tenantOf(r), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if sb.State != store.Running || sb.PodIP == nil {
		writeErr(w, 404, "KUBERNETES::POD_IP_NOT_AVAILABLE", "sandbox is "+sb.State)
		return
	}
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeErr(w, 400, "INVALID_REQUEST", "invalid port")
		return
	}
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", *sb.PodIP, port)}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
			pr.Out.URL.Path = "/" + r.PathValue("path")
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = r.URL.RawQuery
			pr.Out.Host = target.Host
			for _, h := range []string{keyHeader, "Authorization", "Cookie", "OpenSandbox-Secure-Access", execd.TokenHeader} {
				pr.Out.Header.Del(h)
			}
			if port == execd.Port && sb.ExecdToken != nil {
				pr.Out.Header.Set(execd.TokenHeader, *sb.ExecdToken)
			}
			pr.SetXForwarded()
		},
		Transport:     s.proxyTransport,
		FlushInterval: -1, // stream SSE / chunked output immediately
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeErr(w, 502, "BACKEND_CONNECTION_FAILED", err.Error())
		},
	}
	rp.ServeHTTP(w, r)
}

// stateConflict explains a 409 with the sandbox's current state (pause/resume are async: e.g. resume while Pausing).
func (s *Server) stateConflict(w http.ResponseWriter, r *http.Request, err error, op, needs string) {
	if !errors.Is(err, store.ErrConflict) {
		s.fail(w, err)
		return
	}
	cur := "unknown"
	if sb, gerr := s.st.Get(r.Context(), tenantOf(r), r.PathValue("id")); gerr == nil {
		cur = sb.State
	}
	hint := ""
	if cur == store.Pausing || cur == store.Resuming || cur == store.Pending {
		hint = "; the previous operation is still in progress, poll GET /v1/sandboxes/{id} until state is " + needs
	}
	writeErr(w, 409, "INVALID_STATE", fmt.Sprintf("cannot %s: sandbox is %s, must be %s%s", op, cur, needs, hint))
}

// ---------------------------------------------------------------- admin handlers

func (s *Server) adminCreateTenant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID         string `json:"id"`
		MaxRunning int    `json:"maxRunning"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeErr(w, 400, "INVALID_REQUEST", "id is required")
		return
	}
	if req.MaxRunning <= 0 {
		req.MaxRunning = 50
	}
	if err := s.st.CreateTenant(r.Context(), req.ID, req.MaxRunning); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"id": req.ID, "maxRunning": req.MaxRunning})
}

func (s *Server) adminCreateKey(w http.ResponseWriter, r *http.Request) {
	id, key, err := s.st.CreateKey(r.Context(), r.PathValue("tenant"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "TENANT_NOT_FOUND", err.Error()+" (create it first: POST /admin/v1/tenants)")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"keyId": id, "apiKey": key, "tenant": r.PathValue("tenant"),
		"note": "store this key now; it cannot be retrieved again"})
}

func (s *Server) adminListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.st.ListKeys(r.Context(), r.URL.Query().Get("tenant"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, keys)
}

func (s *Server) adminRevokeKey(w http.ResponseWriter, r *http.Request) {
	if err := s.st.RevokeKey(r.Context(), r.PathValue("key")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, 404, "KEY_NOT_FOUND", "no active key "+r.PathValue("key"))
			return
		}
		s.fail(w, err)
		return
	}
	s.authCache.Range(func(k, _ any) bool { s.authCache.Delete(k); return true }) // revoke applies now on this replica
	w.WriteHeader(204)
}

func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request) {
	u, err := s.st.Usage(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, u)
}

// ---------------------------------------------------------------- helpers

func view(sb *store.Sandbox) map[string]any {
	st := map[string]any{"state": sb.State, "lastTransitionAt": sb.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	if sb.StateMessage != "" {
		st["message"] = sb.StateMessage
		st["reason"] = strings.ToUpper(sb.State)
	}
	v := map[string]any{"id": sb.ID, "image": map[string]string{"uri": sb.Image}, "status": st,
		"entrypoint": sb.Entrypoint, "metadata": sb.Metadata, "createdAt": sb.CreatedAt.UTC().Format(time.RFC3339Nano)}
	if len(sb.Entrypoint) == 0 { // a nil []string inside `any` is not == nil; the SDK needs a JSON list here
		v["entrypoint"] = []string{}
	}
	if sb.ExpiresAt != nil {
		v["expiresAt"] = sb.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return v
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, 404, "SANDBOX_NOT_FOUND", err.Error())
	case errors.Is(err, store.ErrConflict):
		writeErr(w, 409, "INVALID_STATE", "operation not allowed in the sandbox's current state")
	case errors.Is(err, store.ErrQuota):
		w.Header().Set("Retry-After", "30")
		writeErr(w, 429, "QUOTA_EXCEEDED", err.Error())
	case errors.Is(err, lifecycle.ErrInvalid):
		writeErr(w, 400, "INVALID_REQUEST", err.Error())
	default:
		s.log.Error("request failed", "err", err)
		writeErr(w, 500, "INTERNAL_ERROR", err.Error())
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, c, msg string) {
	writeJSON(w, code, map[string]string{"code": c, "message": msg})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(c int) { r.code = c; r.ResponseWriter.WriteHeader(c) }
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func logReq(log *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		h.ServeHTTP(rec, r)
		if !strings.HasSuffix(r.URL.Path, "/ping") {
			log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.code, "ms", time.Since(t0).Milliseconds())
		}
	})
}
