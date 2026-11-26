package main

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func write(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func urlsB64(urls []string) string {
	b, _ := json.Marshal(urls)
	return base64.StdEncoding.EncodeToString(b)
}

// chunkServer stores PUT bodies in memory and serves them back on GET.
func chunkServer(t *testing.T) (*httptest.Server, map[string][]byte) {
	store := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			store[r.URL.Path] = b
		case http.MethodGet:
			b, ok := store[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Write(b)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, store
}

func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestPackUploadHydrateRoundTrip(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	big := make([]byte, 70<<20) // > one 64 MiB chunk after compression (random = incompressible)
	_, _ = rand.Read(big)
	write(t, filepath.Join(src, "data/big.bin"), big)
	write(t, filepath.Join(src, "repo/.git/HEAD"), []byte("ref: refs/heads/main\n"))
	write(t, filepath.Join(src, "repo/src/main.py"), []byte("print('hi')\n"))
	if err := os.Symlink("src/main.py", filepath.Join(src, "repo/link.py")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(src, snapDir)
	var packed struct{ Chunks int }
	if err := json.Unmarshal([]byte(captureStdout(t, func() error { return pack(src, out) })), &packed); err != nil {
		t.Fatal(err)
	}
	if packed.Chunks < 2 {
		t.Fatalf("expected multiple chunks, got %d", packed.Chunks)
	}
	srv, _ := chunkServer(t)
	var urls []string
	for i := 0; i < packed.Chunks; i++ {
		urls = append(urls, srv.URL+"/ws/part."+strings.Repeat("0", 4)+string(rune('0'+i)))
	}
	if err := upload(out, urlsB64(urls)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("upload must remove local chunks")
	}
	if err := hydrate(dst, urlsB64(urls)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "data/big.bin"))
	if !bytes.Equal(got, big) {
		t.Fatal("big file differs after round trip")
	}
	if l, _ := os.Readlink(filepath.Join(dst, "repo/link.py")); l != "src/main.py" {
		t.Fatalf("symlink not preserved: %q", l)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "repo/.git/HEAD")); string(b) != "ref: refs/heads/main\n" {
		t.Fatal("dotfiles must be included")
	}
	if _, err := os.Stat(filepath.Join(dst, snapDir+"-in")); !os.IsNotExist(err) {
		t.Fatal("hydrate must clean its temp dir")
	}
}

func TestUploadRejectsChunkCountMismatch(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "f"), []byte("x"))
	out := filepath.Join(src, snapDir)
	captureStdout(t, func() error { return pack(src, out) })
	if err := upload(out, urlsB64([]string{"http://a/1", "http://a/2"})); err == nil {
		t.Fatal("expected mismatch error")
	}
}

// A malicious or corrupted snapshot must not write outside the workspace.
func TestHydrateRejectsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "../../escape.txt", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("evil"))
	tw.Close()
	zw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(buf.Bytes()) }))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "ws")
	_ = os.MkdirAll(dst, 0o755)
	err := hydrate(dst, urlsB64([]string{srv.URL + "/part.00000"}))
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("expected unsafe path error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(dst)), "escape.txt")); err == nil {
		t.Fatal("file escaped the workspace")
	}
}

func TestChunkNamesSortInOrder(t *testing.T) {
	names := []string{"part.00010", "part.00002", "part.00001"}
	sort.Strings(names)
	if names[0] != "part.00001" || names[2] != "part.00010" {
		t.Fatal("zero-padded chunk names must sort numerically")
	}
}

func TestGuardAndPrestop(t *testing.T) {
	marker := filepath.Join(t.TempDir(), ".armed")
	// Unarmed pod (warm pool, or released by the server): the hook must return immediately.
	t0 := time.Now()
	prestop(marker, 5*time.Second, 10*time.Millisecond)
	if time.Since(t0) > time.Second {
		t.Fatal("prestop must not hold an unarmed pod")
	}
	if err := guard("arm", marker); err != nil {
		t.Fatal(err)
	}
	// Armed: holds until disarmed.
	done := make(chan time.Duration)
	go func() { s := time.Now(); prestop(marker, 5*time.Second, 10*time.Millisecond); done <- time.Since(s) }()
	time.Sleep(200 * time.Millisecond)
	if err := guard("disarm", marker); err != nil {
		t.Fatal(err)
	}
	if d := <-done; d < 150*time.Millisecond || d > 2*time.Second {
		t.Fatalf("prestop should hold until disarm, held %s", d)
	}
	// Armed and never disarmed: bounded by max.
	_ = guard("arm", marker)
	t0 = time.Now()
	prestop(marker, 300*time.Millisecond, 10*time.Millisecond)
	if d := time.Since(t0); d < 250*time.Millisecond || d > 2*time.Second {
		t.Fatalf("prestop must give up after max, held %s", d)
	}
	if err := guard("disarm", marker); err != nil || guard("disarm", marker) != nil {
		t.Fatal("disarm must be idempotent")
	}
	if guard("bogus", marker) == nil {
		t.Fatal("unknown op must fail")
	}
}
