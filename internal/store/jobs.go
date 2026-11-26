package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// JobKind is a durable lifecycle operation. There is one current job per sandbox;
// replacing it increments Revision and fences the previous worker.
type JobKind string

const (
	JobStart    JobKind = "start"
	JobPause    JobKind = "pause"
	JobAutosave JobKind = "autosave"
	JobDelete   JobKind = "delete"
)

const jobLease = 60 * time.Second

// Job is a leased or desired lifecycle operation. Owner is empty when a job is
// being scheduled, and set when a worker uses the job as a completion fence.
type Job struct {
	SandboxID  string
	Revision   int64
	Kind       JobKind
	Generation int64
	Payload    json.RawMessage
	Attempts   int
	Owner      string
	CreatedAt  time.Time
}

const jobCols = `sandbox_id, revision, kind, generation, payload, attempts, owner, created_at`

func normalizePayload(p json.RawMessage) json.RawMessage {
	if len(p) == 0 || string(p) == "null" {
		return json.RawMessage(`{}`)
	}
	return p
}

func scanJob(r pgx.Row) (*Job, error) {
	var j Job
	var payload []byte
	var owner *string
	err := r.Scan(&j.SandboxID, &j.Revision, &j.Kind, &j.Generation, &payload, &j.Attempts, &owner, &j.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.Payload = payload
	if owner != nil {
		j.Owner = *owner
	}
	return &j, nil
}

// enqueueTx installs a new current job. Superseding clears the old lease and
// increments Revision, which makes old heartbeats/completions lose their fence.
func enqueueTx(ctx context.Context, tx pgx.Tx, j *Job) error {
	if j == nil {
		return nil
	}
	if j.SandboxID == "" || j.Kind == "" {
		return fmt.Errorf("job sandbox and kind are required")
	}
	err := tx.QueryRow(ctx, `INSERT INTO lifecycle_jobs (sandbox_id, revision, kind, generation, payload)
		VALUES ($1, 1, $2, $3, $4)
		ON CONFLICT (sandbox_id) DO UPDATE SET
			revision = lifecycle_jobs.revision + 1,
			kind = EXCLUDED.kind, generation = EXCLUDED.generation, payload = EXCLUDED.payload,
			available_at = now(), attempts = 0, owner = NULL, lease_until = NULL,
			last_error = '', created_at = now(), updated_at = now()
		RETURNING revision`, j.SandboxID, j.Kind, j.Generation, normalizePayload(j.Payload)).Scan(&j.Revision)
	return err
}

// CurrentJob returns the desired operation, whether queued or leased.
func (s *Store) CurrentJob(ctx context.Context, sandboxID string) (*Job, error) {
	return scanJob(s.db.QueryRow(ctx, `SELECT `+jobCols+` FROM lifecycle_jobs WHERE sandbox_id = $1`, sandboxID))
}

// LeaseJob atomically claims one available job. Expired leases are eligible for
// reclaim; FOR UPDATE SKIP LOCKED allows independent workers to make progress.
func (s *Store) LeaseJob(ctx context.Context, workerID string) (*Job, error) {
	seconds := int64(jobLease / time.Second)
	return scanJob(s.db.QueryRow(ctx, `WITH candidate AS (
		SELECT sandbox_id FROM lifecycle_jobs
		WHERE available_at <= now() AND (lease_until IS NULL OR lease_until < now())
		ORDER BY available_at FOR UPDATE SKIP LOCKED LIMIT 1
	)
	UPDATE lifecycle_jobs AS j SET attempts = j.attempts + 1, owner = $1,
		lease_until = now() + ($2 * interval '1 second'), updated_at = now()
	FROM candidate WHERE j.sandbox_id = candidate.sandbox_id
	RETURNING j.sandbox_id, j.revision, j.kind, j.generation, j.payload, j.attempts, j.owner, j.created_at`, workerID, seconds))
}

// HeartbeatJob extends a lease only while its revision and owner are current.
func (s *Store) HeartbeatJob(ctx context.Context, j *Job, workerID string) (bool, error) {
	seconds := int64(jobLease / time.Second)
	ct, err := s.db.Exec(ctx, `UPDATE lifecycle_jobs SET lease_until = now() + ($4 * interval '1 second'), updated_at = now()
		WHERE sandbox_id = $1 AND revision = $2 AND owner = $3`, j.SandboxID, j.Revision, workerID, seconds)
	return ct.RowsAffected() == 1, err
}

// CompleteJob deletes a finished job if no newer revision has replaced it.
// State-finishing operations normally use TransitionWithJob with the leased job
// instead, making the state transition and deletion one transaction.
func (s *Store) CompleteJob(ctx context.Context, j *Job, workerID string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM lifecycle_jobs WHERE sandbox_id = $1 AND revision = $2 AND owner = $3`,
		j.SandboxID, j.Revision, workerID)
	return err
}

func retryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	shift := attempts - 1
	if shift > 5 {
		shift = 5
	}
	d := time.Duration(1<<shift) * time.Second
	if d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}

// RetryJob releases a lease with bounded exponential backoff.
func (s *Store) RetryJob(ctx context.Context, j *Job, workerID string, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	delay := retryDelay(j.Attempts)
	seconds := int64(delay / time.Second)
	_, err := s.db.Exec(ctx, `UPDATE lifecycle_jobs SET owner = NULL, lease_until = NULL,
		available_at = now() + ($4 * interval '1 second'), last_error = $5, updated_at = now()
		WHERE sandbox_id = $1 AND revision = $2 AND owner = $3`,
		j.SandboxID, j.Revision, workerID, seconds, msg)
	return err
}

// DeferJob releases a lease without counting it as a failed attempt: the job did not run, it only had to wait
// (for example behind another operation on the same sandbox).
func (s *Store) DeferJob(ctx context.Context, j *Job, workerID string, delay time.Duration, reason error) error {
	msg := ""
	if reason != nil {
		msg = reason.Error()
	}
	_, err := s.db.Exec(ctx, `UPDATE lifecycle_jobs SET owner = NULL, lease_until = NULL, attempts = GREATEST(attempts - 1, 0),
		available_at = now() + ($4 * interval '1 millisecond'), last_error = $5, updated_at = now()
		WHERE sandbox_id = $1 AND revision = $2 AND owner = $3`,
		j.SandboxID, j.Revision, workerID, delay.Milliseconds(), msg)
	return err
}

// SandboxLock serializes external effects for a sandbox across all workers and
// replicas. The session-level lock is always explicitly released; failure to
// release destroys the connection so Postgres drops the lock.
type SandboxLock struct {
	conn *pgxpool.Conn
	id   string
}

func (s *Store) TrySandboxLock(ctx context.Context, sandboxID string) (*SandboxLock, bool, error) {
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, sandboxID).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return &SandboxLock{conn: conn, id: sandboxID}, true, nil
}

func (l *SandboxLock) Unlock() {
	if l == nil || l.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := l.conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, l.id); err != nil {
		_ = l.conn.Hijack().Close(context.Background())
	} else {
		l.conn.Release()
	}
	l.conn = nil
}
