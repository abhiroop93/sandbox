package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestRetryDelayIsExponentialAndBounded(t *testing.T) {
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		if got := retryDelay(i + 1); got != want {
			t.Fatalf("attempt %d: got %s want %s", i+1, got, want)
		}
	}
	if got := retryDelay(20); got != 30*time.Second {
		t.Fatalf("retry delay must be capped at 30s, got %s", got)
	}
}

func TestJobLeaseRetryAdvisoryLockAndCompletion(t *testing.T) {
	s, ctx := open(t), context.Background()
	tn := tenant(t, s, 5)
	sb := newSB(tn)
	if err := s.InsertWithQuota(ctx, sb); err != nil {
		t.Fatal(err)
	}
	first, err := s.LeaseJob(ctx, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	if first.Kind != JobStart || first.SandboxID != sb.ID || first.Revision != 1 || first.Attempts != 1 {
		t.Fatalf("unexpected first lease: %+v", first)
	}
	if _, err := s.LeaseJob(ctx, "worker-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an actively leased job must not be leased twice: %v", err)
	}
	if ok, err := s.HeartbeatJob(ctx, first, "worker-a"); err != nil || !ok {
		t.Fatalf("heartbeat: %v %v", ok, err)
	}
	lock, acquired, err := s.TrySandboxLock(ctx, sb.ID)
	if err != nil || !acquired {
		t.Fatalf("first advisory lock: %v %v", acquired, err)
	}
	if other, ok, err := s.TrySandboxLock(ctx, sb.ID); err != nil || ok {
		t.Fatalf("same sandbox must serialize: %v %v", ok, err)
	} else if other != nil {
		other.Unlock()
	}
	firstRevision := first.Revision
	if _, err := s.db.Exec(ctx, `UPDATE lifecycle_jobs SET lease_until = now() - interval '1 second' WHERE sandbox_id = $1`, sb.ID); err != nil {
		t.Fatal(err)
	}
	second, err := s.LeaseJob(ctx, "worker-b")
	if err != nil || second.Revision != firstRevision || second.Attempts != 2 {
		t.Fatalf("expired lease reclaim: %+v %v", second, err)
	}
	if ok, err := s.HeartbeatJob(ctx, first, "worker-a"); err != nil || ok {
		t.Fatalf("old lease heartbeat must lose: %v %v", ok, err)
	}
	if err := s.RetryJob(ctx, second, "worker-b", errors.New("retry")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LeaseJob(ctx, "worker-c"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("backoff should delay lease: %v", err)
	}
	if _, err := s.db.Exec(ctx, `UPDATE lifecycle_jobs SET available_at = now() - interval '1 second' WHERE sandbox_id = $1`, sb.ID); err != nil {
		t.Fatal(err)
	}
	third, err := s.LeaseJob(ctx, "worker-c")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteJob(ctx, third, "worker-c"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CurrentJob(ctx, sb.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("job should be complete: %v", err)
	}
	lock.Unlock()
	// Unlock must not leak a session-level lock through the pool.
	lock, acquired, err = s.TrySandboxLock(ctx, sb.ID)
	if err != nil || !acquired {
		t.Fatalf("lock after unlock: %v %v", acquired, err)
	}
	lock.Unlock()
}

func TestJobSupersessionFencesOldWorkerAndCompletesAtomically(t *testing.T) {
	s, ctx := open(t), context.Background()
	tn := tenant(t, s, 5)
	sb := newSB(tn)
	if err := s.InsertWithQuota(ctx, sb); err != nil {
		t.Fatal(err)
	}
	old, err := s.LeaseJob(ctx, "old-worker")
	if err != nil {
		t.Fatal(err)
	}
	replacement := &Job{SandboxID: sb.ID, Kind: JobPause, Generation: 1, Payload: json.RawMessage(`{"emergency":true}`)}
	if _, err := s.TransitionWithJob(ctx, sb.ID, []string{Pending}, Running, "", Patch{}, replacement); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteJob(ctx, old, "old-worker"); err != nil {
		t.Fatal(err)
	}
	current, err := s.CurrentJob(ctx, sb.ID)
	if err != nil || current.Revision != 2 || current.Kind != JobPause {
		t.Fatalf("old worker deleted/completed replacement: %+v %v", current, err)
	}
	leased, err := s.LeaseJob(ctx, "new-worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionWithJob(ctx, sb.ID, []string{Running}, Paused, "paused", Patch{}, leased); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CurrentJob(ctx, sb.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("state transition must atomically consume leased job: %v", err)
	}
	got, err := s.Get(ctx, "", sb.ID)
	if err != nil || got.State != Paused {
		t.Fatalf("state did not transition: %+v %v", got, err)
	}
}

func TestAutosaveDueUsesPersistedDeadline(t *testing.T) {
	s, ctx := open(t), context.Background()
	tn := tenant(t, s, 5)
	sb := newSB(tn)
	if err := s.InsertWithQuota(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(ctx, `UPDATE sandboxes SET state = 'Running', autosave_at = now() - interval '1 second' WHERE id = $1`, sb.ID); err != nil {
		t.Fatal(err)
	}
	due, err := s.AutosaveDue(ctx, 10)
	if err != nil || len(due) != 1 || due[0].ID != sb.ID {
		t.Fatalf("due autosave: %+v %v", due, err)
	}
	if _, err := s.db.Exec(ctx, `UPDATE sandboxes SET autosave_at = now() + interval '1 hour' WHERE id = $1`, sb.ID); err != nil {
		t.Fatal(err)
	}
	if due, err := s.AutosaveDue(ctx, 10); err != nil || len(due) != 0 {
		t.Fatalf("future autosave must not be due: %+v %v", due, err)
	}
}

func TestSupersedingAJobResetsItsAgeAndDeferDoesNotSpendAttempts(t *testing.T) {
	s, ctx := open(t), context.Background()
	tn := tenant(t, s, 5)
	sb := newSB(tn)
	if err := s.InsertWithQuota(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(ctx, `UPDATE lifecycle_jobs SET created_at = now() - interval '1 hour' WHERE sandbox_id = $1`, sb.ID); err != nil {
		t.Fatal(err)
	}
	j, err := s.LeaseJob(ctx, "w")
	if err != nil || j.Attempts != 1 {
		t.Fatalf("lease: %+v %v", j, err)
	}
	if err := s.DeferJob(ctx, j, "w", 0, errors.New("busy")); err != nil {
		t.Fatal(err)
	}
	again, err := s.LeaseJob(ctx, "w")
	if err != nil || again.Attempts != 1 {
		t.Fatalf("a deferred job must not accumulate attempts: %+v %v", again, err)
	}
	// Replace the job (as a pause would): the new operation is new work, so it is young again.
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueueTx(ctx, tx, &Job{SandboxID: sb.ID, Kind: JobPause, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	cur, err := s.CurrentJob(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(cur.CreatedAt) > time.Minute {
		t.Fatalf("a superseding job kept the old created_at: %s", cur.CreatedAt)
	}
}
