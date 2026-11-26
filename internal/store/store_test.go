package store

// Integration tests against a real Postgres. Skipped unless TEST_DATABASE_URL is set, e.g.:
//   kubectl -n sandbox-system port-forward svc/postgres 15432:5432 &
//   PW=$(kubectl -n sandbox-system get secret sandbox-postgres -o jsonpath='{.data.POSTGRES_PASSWORD}' | base64 -d)
//   TEST_DATABASE_URL="postgres://sandbox:$PW@127.0.0.1:15432/sandbox?sslmode=disable" go test ./internal/store/

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func open(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	s, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func tenant(t *testing.T, s *Store, max int) string {
	id := "test-" + uuid.NewString()[:8]
	if err := s.CreateTenant(context.Background(), id, max); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // don't leave Pending rows behind: they look like interrupted operations to the server
		ctx := context.Background()
		_, _ = s.db.Exec(ctx, `DELETE FROM usage_events WHERE tenant_id = $1`, id)
		_, _ = s.db.Exec(ctx, `DELETE FROM sandboxes WHERE tenant_id = $1`, id)
		_, _ = s.db.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
	})
	return id
}

func newSB(tn string) *Sandbox {
	return &Sandbox{ID: uuid.NewString(), Tenant: tn, Cell: "cell-1", Template: "t", Image: "img"}
}

func TestKeysAreHashedAndRevocable(t *testing.T) {
	s, ctx := open(t), context.Background()
	tn := tenant(t, s, 5)
	id, key, err := s.CreateKey(ctx, tn)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Authenticate(ctx, key); err != nil || got != tn {
		t.Fatalf("auth: %v %v", got, err)
	}
	var stored string
	_ = s.db.QueryRow(ctx, `SELECT key_hash FROM api_keys WHERE id=$1`, id).Scan(&stored)
	if stored == key || len(stored) != 64 {
		t.Fatal("key must be stored only as a sha256 hex hash")
	}
	if err := s.RevokeKey(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, key); !errors.Is(err, ErrBadKey) {
		t.Fatal("revoked key must not authenticate")
	}
	if _, _, err := s.CreateKey(ctx, "no-such-tenant"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("key for unknown tenant: %v", err)
	}
}

func TestTransitionIsCompareAndSetWithFencing(t *testing.T) {
	s, ctx := open(t), context.Background()
	tn := tenant(t, s, 5)
	sb := newSB(tn)
	if err := s.InsertWithQuota(ctx, sb); err != nil {
		t.Fatal(err)
	}
	got, err := s.Transition(ctx, sb.ID, []string{Pending}, Pending, "", Patch{BumpGeneration: true})
	if err != nil || got.Generation != 1 {
		t.Fatalf("bump: %v gen=%d", err, got.Generation)
	}
	if _, err := s.Transition(ctx, sb.ID, []string{Running}, Paused, "", Patch{}); !errors.Is(err, ErrConflict) {
		t.Fatal("transition from a state the sandbox is not in must fail with ErrConflict")
	}
	// Two concurrent resumes: exactly one wins (D6: never two instances).
	_, _ = s.Transition(ctx, sb.ID, []string{Pending}, Paused, "", Patch{})
	var wg sync.WaitGroup
	wins := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Transition(ctx, sb.ID, []string{Paused}, Resuming, "", Patch{})
			wins <- err == nil
		}()
	}
	wg.Wait()
	close(wins)
	n := 0
	for w := range wins {
		if w {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 winning resume, got %d", n)
	}
}

func TestQuotaIsEnforcedAtomically(t *testing.T) {
	s, ctx := open(t), context.Background()
	tn := tenant(t, s, 3)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.InsertWithQuota(ctx, newSB(tn)) }()
	}
	wg.Wait()
	close(errs)
	ok, quota := 0, 0
	for e := range errs {
		switch {
		case e == nil:
			ok++
		case errors.Is(e, ErrQuota):
			quota++
		default:
			t.Fatal(e)
		}
	}
	if ok != 3 || quota != 7 {
		t.Fatalf("10 concurrent creates with quota 3: ok=%d quota=%d", ok, quota)
	}
}

func TestTenantIsolationOnGet(t *testing.T) {
	s, ctx := open(t), context.Background()
	a, b := tenant(t, s, 5), tenant(t, s, 5)
	sb := newSB(a)
	if err := s.InsertWithQuota(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, b, sb.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("another tenant must not see the sandbox")
	}
	if items, _, err := s.List(ctx, b, []string{}, nil, 50, 0); err != nil || len(items) != 0 {
		t.Fatalf("tenant b list should be empty: %d %v", len(items), err)
	}
}
