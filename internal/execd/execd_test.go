package execd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeExecd serves /internal/init, /ready and /command on the execd port of a loopback IP.
func fakeExecd(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", Port))
	if err != nil {
		t.Skipf("execd port %d unavailable: %v", Port, err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return "127.0.0.1"
}

func TestBindSendsHashedTokenAndWaitsReady(t *testing.T) {
	var bound atomic.Bool
	var got Binding
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/init", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		bound.Store(true)
		w.WriteHeader(200)
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		if bound.Load() {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(503)
	})
	ip := fakeExecd(t, mux)
	if err := Bind(context.Background(), ip, "sb-1", 3, "secret-token", map[string]string{"A": "1"}, "team"); err != nil {
		t.Fatal(err)
	}
	if got.SandboxID != "sb-1" || got.Generation != 3 || got.Envs["A"] != "1" {
		t.Fatalf("binding not sent correctly: %+v", got)
	}
	if !strings.HasPrefix(got.AccessTokenHash, "sha256:") || strings.Contains(got.AccessTokenHash, "secret-token") {
		t.Fatalf("token must be sent only as a sha256 hash, got %q", got.AccessTokenHash)
	}
}

func TestBindRejectedFailsFast(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/init", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"reserved env key"}`, 400)
	})
	ip := fakeExecd(t, mux)
	t0 := time.Now()
	if err := Bind(context.Background(), ip, "sb", 1, "t", nil, "x"); err == nil {
		t.Fatal("expected error")
	}
	if time.Since(t0) > 5*time.Second {
		t.Fatal("a 400 from execd must not be retried until the deadline")
	}
}

func TestRunParsesSSEAndInjectsToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /command", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(TokenHeader) != "tok" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"type":"init","text":"x"}`)
		fmt.Fprintln(w, `data: {"type":"stdout","text":"{\"chunks\":3,"}`)
		fmt.Fprintln(w, `{"type":"stdout","text":"\"bytes\":9}\n"}`) // raw JSON line (no data: prefix) also accepted
		fmt.Fprintln(w, `data: {"type":"stderr","text":"warn"}`)
		fmt.Fprintln(w, `data: {"type":"execution_complete","exit_code":0}`)
	})
	ip := fakeExecd(t, mux)
	res, err := Run(context.Background(), ip, "tok", []string{"true"}, "/workspace", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout != "{\"chunks\":3,\"bytes\":9}\n" || res.Stderr != "warn" {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestRunNonZeroExit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /command", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `data: {"type":"error","error":{"ename":"CommandExecError","evalue":"2","exit_code":2}}`)
		fmt.Fprintln(w, `data: {"type":"execution_complete"}`)
	})
	ip := fakeExecd(t, mux)
	res, err := Run(context.Background(), ip, "tok", []string{"false"}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 2 {
		t.Fatalf("exit code should be 2, got %d", res.ExitCode)
	}
}
