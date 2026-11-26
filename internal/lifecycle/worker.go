package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/abhiroop93/sandbox/internal/execd"
	"github.com/abhiroop93/sandbox/internal/store"
)

func (m *Manager) workerLoop(ctx context.Context, workerID string) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		job, err := m.st.LeaseJob(ctx, workerID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				m.log.Warn("lease lifecycle job", "worker", workerID, "err", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
			continue
		}
		m.runLeasedJob(ctx, workerID, job)
	}
}

func (m *Manager) runLeasedJob(parent context.Context, workerID string, job *store.Job) {
	payload := decodePayload(job)
	limit := 15 * time.Minute
	switch job.Kind {
	case store.JobStart:
		limit = 10 * time.Minute
	case store.JobPause:
		if payload.Emergency {
			limit = m.ShutdownGrace
		}
	case store.JobDelete:
		limit = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, limit)
	defer cancel()

	lock, acquired, err := m.st.TrySandboxLock(ctx, job.SandboxID)
	if err != nil {
		m.retryJob(workerID, job, err)
		return
	}
	if !acquired {
		// Not a failure: do not spend the job's attempts while another operation holds the sandbox.
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		if err := m.st.DeferJob(dctx, job, workerID, 2*time.Second, fmt.Errorf("waiting for previous sandbox operation")); err != nil {
			m.log.Warn("defer lifecycle job", "id", job.SandboxID, "kind", job.Kind, "err", err)
		}
		return
	}
	defer lock.Unlock()

	heartbeatDone := make(chan struct{})
	heartbeatCurrent := make(chan bool, 1)
	go m.heartbeat(ctx, cancel, heartbeatDone, heartbeatCurrent, workerID, job)

	switch job.Kind {
	case store.JobStart:
		err = m.executeStart(ctx, workerID, job, payload.Hydrate)
	case store.JobPause, store.JobAutosave:
		err = m.executeCapture(ctx, workerID, job, payload)
	case store.JobDelete:
		err = m.executeDelete(ctx, workerID, job, payload.Event)
	default:
		err = fmt.Errorf("unknown lifecycle job kind %q", job.Kind)
	}
	close(heartbeatDone)
	current := <-heartbeatCurrent
	if !current {
		return // a newer desired operation owns the queue row now
	}
	if err != nil {
		// Cancellation and shutdown release work for another attempt; only a CAS
		// conflict can complete it because a newer operation owns the outcome.
		if errors.Is(err, store.ErrConflict) {
			m.finishJob(workerID, job)
			return
		}
		// Deletes never become permanently stuck just because cleanup is
		// temporarily unavailable. Other kinds eventually surface failure.
		if job.Kind != store.JobDelete && (job.Attempts >= 10 || time.Since(job.CreatedAt) > 15*time.Minute) {
			m.failLeasedJob(workerID, job, payload, err)
			return
		}
		m.retryJob(workerID, job, err)
		return
	}
	m.finishJob(workerID, job)
}

func (m *Manager) heartbeat(ctx context.Context, cancel context.CancelFunc, done <-chan struct{}, result chan<- bool,
	workerID string, job *store.Job) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	current := true
	failures := 0
	for {
		select {
		case <-done:
			result <- current
			return
		case <-ctx.Done():
			result <- current
			return
		case <-ticker.C:
			ok, err := m.st.HeartbeatJob(ctx, job, workerID)
			if err != nil {
				failures++
				if failures >= 3 {
					cancel()
				}
				continue
			}
			failures = 0
			if !ok {
				current = false
				cancel()
				result <- current
				return
			}
		}
	}
}

func (m *Manager) finishJob(workerID string, job *store.Job) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.st.CompleteJob(ctx, job, workerID); err != nil {
		m.log.Warn("complete lifecycle job", "id", job.SandboxID, "kind", job.Kind, "err", err)
	}
}

func (m *Manager) retryJob(workerID string, job *store.Job, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.st.RetryJob(ctx, job, workerID, cause); err != nil {
		m.log.Warn("retry lifecycle job", "id", job.SandboxID, "kind", job.Kind, "err", err)
		return
	}
	m.log.Warn("lifecycle job released for retry", "id", job.SandboxID, "kind", job.Kind,
		"attempt", job.Attempts, "err", cause)
}

// executeStart claims and binds the preallocated generation. Every external
// operation is idempotent under the sandbox advisory lock.
func (m *Manager) executeStart(ctx context.Context, workerID string, job *store.Job, hydrate bool) (err error) {
	started := time.Now()
	sb, err := m.st.Get(ctx, "", job.SandboxID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	from := store.Pending
	if hydrate {
		from = store.Resuming
	}
	if sb.State != from || sb.Generation != job.Generation {
		return nil
	}
	claim, token := deref(sb.ClaimName), deref(sb.ExecdToken)
	if claim == "" || token == "" {
		return fmt.Errorf("start generation %d has no persisted claim/token", job.Generation)
	}
	tmpl, err := m.cell.EnsureTemplate(ctx, sb.Image, m.poolSize)
	if err != nil {
		return fmt.Errorf("template: %w", err)
	}
	if tmpl != sb.Template {
		return fmt.Errorf("template name changed for image: persisted %s, computed %s", sb.Template, tmpl)
	}
	createdClaim, err := m.cell.Claim(ctx, sb.Template, sb.ID, sb.Tenant, sb.Generation)
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	if createdClaim != claim {
		return fmt.Errorf("claim identity changed: persisted %s, created %s", claim, createdClaim)
	}
	pod, ip, err := m.cell.WaitReady(ctx, claim, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("wait-ready: %w", err)
	}
	claimDone := time.Since(started)
	if err := m.applyPolicy(ctx, sb); err != nil {
		return fmt.Errorf("network-policy: %w", err)
	}
	if err := execd.Bind(ctx, ip, sb.ID, sb.Generation, token, sb.Env, sb.Tenant); err != nil {
		return fmt.Errorf("execd-bind: %w", err)
	}
	bindDone := time.Since(started)
	if hydrate && sb.SnapshotKey != nil {
		if err := m.hydrate(ctx, sb, ip, token); err != nil {
			return err
		}
	}
	event := "started"
	if hydrate {
		event = "resumed"
	}
	patch := store.Patch{PodName: store.Str(pod), PodIP: store.Str(ip), ExecdToken: store.Str(token)}
	m.setNextAutosave(&patch)
	if _, err := m.st.TransitionWithJob(ctx, sb.ID, []string{from}, store.Running, event, patch, job); err != nil {
		_ = m.cell.Release(context.Background(), claim)
		return err
	}
	m.protect(ctx, sb.ID, pod, ip, token)
	m.log.Info("sandbox running", "id", sb.ID, "gen", sb.Generation, "resume", hydrate,
		"claim_ms", claimDone.Milliseconds(), "bind_ms", (bindDone - claimDone).Milliseconds(),
		"hydrate_ms", (time.Since(started) - bindDone).Milliseconds(), "total_ms", time.Since(started).Milliseconds())
	return nil
}

func (m *Manager) executeDelete(ctx context.Context, workerID string, job *store.Job, event string) error {
	sb, err := m.st.Get(ctx, "", job.SandboxID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if sb.State == store.Terminated {
		return nil
	}
	if sb.State != store.Stopping {
		return nil
	}
	m.disarm(sb)
	if claim := deref(sb.ClaimName); claim != "" {
		if err := m.cell.Release(ctx, claim); err != nil {
			return fmt.Errorf("release claim: %w", err)
		}
	}
	if err := m.cell.DeleteSandboxPolicy(ctx, sb.ID); err != nil {
		return fmt.Errorf("delete policy: %w", err)
	}
	if err := m.snap.Delete(ctx, "ws/"+sb.ID+"/"); err != nil {
		return fmt.Errorf("delete snapshots: %w", err)
	}
	if event == "" {
		event = "terminated"
	}
	_, err = m.st.TransitionWithJob(ctx, sb.ID, []string{store.Stopping}, store.Terminated, event, store.Patch{
		ClaimName: store.NullStr(), PodName: store.NullStr(), PodIP: store.NullStr(), ExecdToken: store.NullStr(),
		SnapshotKey: store.NullStr(), AutosaveAt: nullTime()}, job)
	return err
}

func (m *Manager) failLeasedJob(workerID string, job *store.Job, payload operationPayload, cause error) {
	m.log.Error("lifecycle job permanently failed", "id", job.SandboxID, "kind", job.Kind, "err", cause)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	switch job.Kind {
	case store.JobStart:
		sb, err := m.st.Get(ctx, "", job.SandboxID)
		if err != nil {
			m.retryJob(workerID, job, err)
			return
		}
		from := store.Pending
		if payload.Hydrate {
			from = store.Resuming
		}
		msg := cause.Error()
		_, err = m.st.TransitionWithJob(ctx, sb.ID, []string{from}, store.Failed, "failed", store.Patch{
			StateMessage: &msg, ClaimName: store.NullStr(), PodName: store.NullStr(), PodIP: store.NullStr(),
			ExecdToken: store.NullStr(), AutosaveAt: nullTime()}, job)
		if err == nil && sb.ClaimName != nil {
			_ = m.cell.Release(context.Background(), *sb.ClaimName)
		}
	case store.JobPause, store.JobAutosave:
		m.failCapture(ctx, workerID, job, payload, cause)
	default:
		m.retryJob(workerID, job, cause)
	}
}
