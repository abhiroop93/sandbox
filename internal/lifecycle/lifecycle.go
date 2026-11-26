// Package lifecycle orchestrates durable sandbox state transitions (D6):
// create → Running, pause (snapshot /workspace), resume, delete, expiry and
// reconciliation. Public operations remain asynchronous; persisted jobs replay
// interrupted external effects with generation and revision fencing.
package lifecycle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/abhiroop93/sandbox/internal/cell"
	"github.com/abhiroop93/sandbox/internal/execd"
	"github.com/abhiroop93/sandbox/internal/snapshot"
	"github.com/abhiroop93/sandbox/internal/store"
)

const (
	Workspace  = "/workspace"
	SnapTool   = cell.SnapTool
	snapOutDir = "/workspace/.sandbox-snap"
	freezePath = "/opt/sandbox-bin/.frozen-processes"
)

// NetworkPolicy is the OpenSandbox networkPolicy shape.
type NetworkPolicy struct {
	DefaultAction string `json:"defaultAction,omitempty"`
	Egress        []struct {
		Action string `json:"action"`
		Target string `json:"target,omitempty"`
		Ports  []int  `json:"ports,omitempty"`
	} `json:"egress,omitempty"`
}

type CreateSpec struct {
	Image            string
	Entrypoint       []string
	Env              map[string]string
	Metadata         map[string]string
	Timeout          *time.Duration // nil = no expiry
	Network          *NetworkPolicy
	ResourceLimits   map[string]string
	ResourceRequests map[string]string
}

type Manager struct {
	st            *store.Store
	cell          *cell.Cell
	snap          *snapshot.Store
	poolSize      int
	defaultEgress string // "deny" | "allow"
	log           *slog.Logger
	WorkerCount   int
	WorkerID      string
	// AutosaveInterval is zero when periodic workspace archives are disabled.
	AutosaveInterval time.Duration
	ReconcileTimeout time.Duration
	// ShutdownGrace bounds an emergency save: the pod is killed after this anyway.
	ShutdownGrace time.Duration
}

func New(st *store.Store, c *cell.Cell, snap *snapshot.Store, poolSize int, defaultEgress string, log *slog.Logger) *Manager {
	return &Manager{st: st, cell: c, snap: snap, poolSize: poolSize, defaultEgress: defaultEgress, log: log,
		WorkerCount: 4, WorkerID: uuid.NewString(), ReconcileTimeout: 30 * time.Second, ShutdownGrace: 90 * time.Second}
}

var ErrInvalid = errors.New("invalid request")

// ValidateNetwork rejects rules this cell cannot enforce (FQDN targets need an FQDN-aware CNI such as Cilium).
func ValidateNetwork(np *NetworkPolicy) error {
	if np == nil {
		return nil
	}
	if np.DefaultAction != "" && np.DefaultAction != "allow" && np.DefaultAction != "deny" {
		return fmt.Errorf("%w: defaultAction must be allow|deny", ErrInvalid)
	}
	for _, r := range np.Egress {
		if r.Action != "allow" && r.Action != "deny" {
			return fmt.Errorf("%w: egress action must be allow|deny", ErrInvalid)
		}
		if r.Action == "deny" {
			return fmt.Errorf("%w: explicit deny rules are not supported (egress is default-deny; use defaultAction)", ErrInvalid)
		}
		t := r.Target
		if t == "" || strings.IndexFunc(t, func(c rune) bool { return (c < '0' || c > '9') && c != '.' && c != '/' }) >= 0 {
			return fmt.Errorf("%w: egress target %q: only IPv4/CIDR targets are supported in this cell (FQDN rules need an FQDN-aware CNI)", ErrInvalid, t)
		}
	}
	return nil
}

func validateResourceMap(field string, values map[string]string, cpu, memory string) error {
	expected := map[string]string{"cpu": cpu, "memory": memory}
	for name, value := range values {
		want, ok := expected[name]
		if !ok {
			return fmt.Errorf("%w: %s.%s is unsupported; only cpu and memory are available", ErrInvalid, field, name)
		}
		gotQuantity, err := resource.ParseQuantity(value)
		if err != nil {
			return fmt.Errorf("%w: invalid %s.%s %q", ErrInvalid, field, name, value)
		}
		wantQuantity, _ := resource.ParseQuantity(want)
		if gotQuantity.Cmp(wantQuantity) != 0 {
			return fmt.Errorf("%w: %s.%s=%s, fixed resource class is %s (custom resource classes are not supported)",
				ErrInvalid, field, name, value, want)
		}
	}
	return nil
}

// Create performs resource and quota admission before any Kubernetes side
// effect. Requests matching the one configured pod resource class are honored;
// other resource classes are rejected rather than silently ignored.
func (m *Manager) Create(ctx context.Context, tenant string, spec CreateSpec) (*store.Sandbox, error) {
	if err := ValidateNetwork(spec.Network); err != nil {
		return nil, err
	}
	cpu, memory := m.cell.FixedResources()
	if err := validateResourceMap("resourceLimits", spec.ResourceLimits, cpu, memory); err != nil {
		return nil, err
	}
	if err := validateResourceMap("resourceRequests", spec.ResourceRequests, cpu, memory); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	claim := cell.ClaimName(id, 1)
	token := execd.NewToken()
	sb := &store.Sandbox{ID: id, Tenant: tenant, Cell: "cell-1", Template: cell.TemplateName(spec.Image), Image: spec.Image,
		Entrypoint: spec.Entrypoint, Env: spec.Env, Metadata: spec.Metadata, Generation: 1,
		ClaimName: &claim, ExecdToken: &token}
	if spec.Network != nil {
		sb.Network, _ = json.Marshal(spec.Network)
	}
	if spec.Timeout != nil {
		t := time.Now().Add(*spec.Timeout).UTC()
		sb.ExpiresAt = &t
	}
	if err := m.st.InsertWithQuota(ctx, sb); err != nil {
		return nil, err
	}
	return m.st.Get(ctx, tenant, sb.ID)
}

// Pause queues a process-quiesced archive followed by pod release.
func (m *Manager) Pause(ctx context.Context, tenant, id string) error {
	cur, err := m.st.Get(ctx, tenant, id)
	if err != nil {
		return err
	}
	payload := operationPayload{TargetPrefix: snapshot.Prefix(id, cur.Generation)}
	return m.enqueueCapture(ctx, cur, store.JobPause, payload, []string{store.Running}, "")
}

// Resume durably preallocates the next generation and queues hydration.
func (m *Manager) Resume(ctx context.Context, tenant, id string) error {
	cur, err := m.st.Get(ctx, tenant, id)
	if err != nil {
		return err
	}
	gen := cur.Generation + 1
	claim := cell.ClaimName(id, gen)
	token := execd.NewToken()
	job := &store.Job{SandboxID: id, Kind: store.JobStart, Generation: gen,
		Payload: encodePayload(operationPayload{Hydrate: true})}
	_, err = m.st.TransitionWithJob(ctx, id, []string{store.Paused}, store.Resuming, "", store.Patch{
		BumpGeneration: true, ClaimName: store.Str(claim), ExecdToken: store.Str(token)}, job)
	return err
}

// Delete queues idempotent pod, policy and snapshot cleanup.
func (m *Manager) Delete(ctx context.Context, tenant, id string) error {
	if _, err := m.st.Get(ctx, tenant, id); err != nil {
		return err
	}
	return m.terminate(ctx, id, "terminated")
}

func (m *Manager) terminate(ctx context.Context, id, ev string) error {
	cur, err := m.st.Get(ctx, "", id)
	if err != nil {
		return err
	}
	live := []string{store.Pending, store.Running, store.Pausing, store.Paused, store.Resuming, store.Failed}
	job := &store.Job{SandboxID: id, Kind: store.JobDelete, Generation: cur.Generation,
		Payload: encodePayload(operationPayload{Event: ev})}
	_, err = m.st.TransitionWithJob(ctx, id, live, store.Stopping, "", store.Patch{}, job)
	return err
}

// Renew sets a new expiry without changing lifecycle state.
func (m *Manager) Renew(ctx context.Context, tenant, id string, at time.Time) (*store.Sandbox, error) {
	cur, err := m.st.Get(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	t := at.UTC()
	tp := &t
	return m.st.Transition(ctx, id, []string{cur.State}, cur.State, "", store.Patch{StateMessage: &cur.StateMessage, ExpiresAt: &tp})
}

// RecoverInterrupted reconstructs jobs only for rows written by pre-queue
// versions. Existing durable jobs keep their normal lease and replay semantics.
func (m *Manager) RecoverInterrupted(ctx context.Context) {
	states := []string{store.Pending, store.Resuming, store.Pausing, store.Stopping}
	stuck, err := m.st.InStates(ctx, states)
	if err != nil {
		m.log.Error("recover: list", "err", err)
		return
	}
	for _, sb := range stuck {
		if _, err := m.st.CurrentJob(ctx, sb.ID); err == nil {
			continue
		}
		switch sb.State {
		case store.Pending, store.Resuming:
			m.recoverStart(ctx, sb)
		case store.Pausing:
			p := operationPayload{TargetPrefix: snapshot.Prefix(sb.ID, sb.Generation)}
			if err := m.enqueueCapture(ctx, sb, store.JobPause, p, []string{store.Pausing}, "pause recovered after server restart"); err != nil {
				m.log.Error("recover pause", "id", sb.ID, "err", err)
			}
		case store.Stopping:
			job := &store.Job{SandboxID: sb.ID, Kind: store.JobDelete, Generation: sb.Generation,
				Payload: encodePayload(operationPayload{Event: "terminated"})}
			if _, err := m.st.TransitionWithJob(ctx, sb.ID, []string{store.Stopping}, store.Stopping, "", store.Patch{}, job); err != nil {
				m.log.Error("recover delete", "id", sb.ID, "err", err)
			}
		}
	}
}

func (m *Manager) recoverStart(ctx context.Context, sb *store.Sandbox) {
	gen := sb.Generation
	patch := store.Patch{}
	if gen == 0 {
		gen = 1
		patch.BumpGeneration = true
	}
	claim := deref(sb.ClaimName)
	if claim == "" {
		claim = cell.ClaimName(sb.ID, gen)
		patch.ClaimName = store.Str(claim)
	}
	if deref(sb.ExecdToken) == "" {
		patch.ExecdToken = store.Str(execd.NewToken())
	}
	job := &store.Job{SandboxID: sb.ID, Kind: store.JobStart, Generation: gen,
		Payload: encodePayload(operationPayload{Hydrate: sb.State == store.Resuming})}
	if _, err := m.st.TransitionWithJob(ctx, sb.ID, []string{sb.State}, sb.State, "", patch, job); err != nil {
		m.log.Error("recover start", "id", sb.ID, "err", err)
	}
}

// Loop starts leased workers and bounded maintenance loops. Workers exit through
// ctx; crash-abandoned leases expire and can be reclaimed by any replica.
func (m *Manager) Loop(ctx context.Context) {
	for i := 0; i < m.WorkerCount; i++ {
		go m.workerLoop(ctx, fmt.Sprintf("%s-%d", m.WorkerID, i))
	}
	go m.cell.WatchTerminating(ctx, m.onTerminating)
	go m.maintenanceLoop(ctx)
	<-ctx.Done()
}

func (m *Manager) maintenanceLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if exp, err := m.st.Expired(ctx, 100); err == nil {
			for _, sb := range exp {
				if err := m.terminate(ctx, sb.ID, "expired"); err == nil {
					m.log.Info("sandbox expiry queued", "id", sb.ID)
				}
			}
		}
		rctx, cancel := context.WithTimeout(ctx, m.ReconcileTimeout)
		m.reconcile(rctx)
		m.checkPods(rctx)
		cancel()
		if m.AutosaveInterval > 0 {
			m.scheduleAutosaves(ctx)
		}
	}
}

// ---------------------------------------------------------------- pod interruptions

type verdict int

const (
	podOK verdict = iota
	podTerminating
	podRestarted
	podLost
)

// podVerdict decides what a Running sandbox's pod state means. found=false: no pod with the recorded name.
func podVerdict(sb *store.Sandbox, p cell.PodInfo, found bool) (verdict, string) {
	switch {
	case !found:
		return podLost, "pod no longer exists (node failure or forced deletion)"
	case p.Terminating:
		return podTerminating, ""
	case p.Phase == corev1.PodFailed || p.Phase == corev1.PodSucceeded:
		return podLost, "pod stopped (" + string(p.Phase) + ")"
	case p.IP != deref(sb.PodIP):
		return podLost, "pod was replaced"
	case p.BoundRestarts >= 0 && p.Restarts > p.BoundRestarts:
		return podRestarted, p.LastExit
	}
	return podOK, ""
}

func (m *Manager) checkPods(ctx context.Context) {
	running, err := m.st.InStates(ctx, []string{store.Running})
	if err != nil || len(running) == 0 {
		return
	}
	pods, err := m.cell.SandboxPods(ctx)
	if err != nil {
		m.log.Warn("pod reconciliation list failed", "err", err)
		return
	}
	for _, sb := range running {
		p, found := pods[deref(sb.PodName)]
		switch v, why := podVerdict(sb, p, found); v {
		case podTerminating:
			m.onTerminating(p.Name, sb.ID)
		case podRestarted:
			m.rebind(ctx, sb, p, why)
		case podLost:
			m.markLost(ctx, sb, []string{store.Running}, why)
		}
	}
}

// onTerminating gives emergency pause precedence over an ordinary or automatic
// capture. Revision-based replacement cancels the previous worker.
func (m *Manager) onTerminating(pod, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cur, err := m.st.Get(ctx, "", id)
	if err != nil || deref(cur.PodName) != pod {
		return
	}
	msg := "pod is being shut down by the cluster; saving workspace"
	payload := operationPayload{Emergency: true, TargetPrefix: snapshot.Prefix(id, cur.Generation)}
	switch cur.State {
	case store.Running:
		if err := m.enqueueCapture(ctx, cur, store.JobPause, payload, []string{store.Running}, msg); err == nil {
			m.log.Warn("pod shutting down, emergency save queued", "id", id, "pod", pod)
		}
	case store.Pausing:
		job, err := m.st.CurrentJob(ctx, id)
		if err != nil {
			return
		}
		old := decodePayload(job)
		if (job.Kind == store.JobPause || job.Kind == store.JobAutosave) && !old.Emergency {
			if err := m.enqueueCapture(ctx, cur, store.JobPause, payload, []string{store.Pausing}, msg); err == nil {
				m.log.Warn("pod shutdown preempted previous capture", "id", id, "pod", pod)
			}
		}
	}
}

func (m *Manager) markLost(ctx context.Context, sb *store.Sandbox, from []string, why string) {
	clear := store.Patch{ClaimName: store.NullStr(), PodName: store.NullStr(), PodIP: store.NullStr(), ExecdToken: store.NullStr()}
	to, msg := store.Failed, why+"; no saved snapshot, workspace lost"
	if sb.SnapshotKey != nil {
		to, msg = store.Paused, why+"; changes since the last saved snapshot were lost. Resume to continue."
	}
	clear.StateMessage = &msg
	if _, err := m.st.Transition(ctx, sb.ID, from, to, "lost", clear); err != nil {
		return
	}
	if sb.ClaimName != nil {
		_ = m.cell.Release(ctx, *sb.ClaimName)
	}
	m.log.Warn("sandbox pod lost", "id", sb.ID, "to", to, "why", why)
}

func (m *Manager) rebind(ctx context.Context, sb *store.Sandbox, p cell.PodInfo, why string) {
	ip, token := deref(sb.PodIP), deref(sb.ExecdToken)
	if err := execd.Bind(ctx, ip, sb.ID, sb.Generation, token, sb.Env, sb.Tenant); err != nil {
		m.log.Error("rebind after container restart failed", "id", sb.ID, "err", err)
		return
	}
	m.protect(ctx, sb.ID, p.Name, ip, token)
	msg := "sandbox container restarted (" + why + "); files kept, running processes were lost"
	_, _ = m.st.Transition(ctx, sb.ID, []string{store.Running}, store.Running, "restarted", store.Patch{StateMessage: &msg})
	m.log.Warn("sandbox container restarted, re-bound", "id", sb.ID, "why", why)
}

func (m *Manager) reconcile(ctx context.Context) {
	started := time.Now()
	known, err := m.st.ActiveClaims(ctx)
	if err != nil {
		m.log.Warn("reconcile: known claims", "err", err)
		return
	}
	live, err := m.cell.ListClaims(ctx)
	if err != nil {
		m.log.Warn("reconcile: live claims", "err", err)
		return
	}
	orphans, reclaimed, failures := 0, 0, 0
	for claim, sbID := range live {
		if _, ok := known[claim]; ok {
			continue
		}
		orphans++
		if cur, err := m.st.Get(ctx, "", sbID); err == nil && (cur.State == store.Pending || cur.State == store.Resuming) {
			continue
		}
		if err := m.cell.Release(ctx, claim); err != nil {
			failures++
			m.log.Warn("reaping orphan claim failed", "claim", claim, "sandbox", sbID, "err", err)
		} else {
			reclaimed++
		}
	}
	m.log.Info("claim reconciliation complete", "duration_ms", time.Since(started).Milliseconds(),
		"orphans", orphans, "reclaimed", reclaimed, "failures", failures)
}

// protect arms preStop and tells the autoscaler not to evict a claimed pod.
// protect runs after the state change that completes a job. That transition deletes the job row, which makes the
// worker's heartbeat cancel the job context; arming the guard must outlive that, or the sandbox starts without
// eviction protection.
func (m *Manager) protect(ctx context.Context, id, pod, ip, token string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	if res, err := execd.Run(ctx, ip, token, []string{SnapTool, "guard", "arm", cell.GuardMarker}, Workspace, 30*time.Second); err != nil || res.ExitCode != 0 {
		m.log.Warn("arm shutdown guard failed", "id", id, "err", cmdErr(res, err))
	}
	if err := m.cell.MarkBound(ctx, pod); err != nil {
		m.log.Warn("mark pod bound failed", "id", id, "err", err)
	}
}

func (m *Manager) disarm(sb *store.Sandbox) {
	ip, token := deref(sb.PodIP), deref(sb.ExecdToken)
	if ip == "" || token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = execd.Run(ctx, ip, token, []string{SnapTool, "guard", "disarm", cell.GuardMarker}, Workspace, 5*time.Second)
}

func (m *Manager) applyPolicy(ctx context.Context, sb *store.Sandbox) error {
	allow := m.defaultEgress == "allow"
	var rules []cell.EgressRule
	if len(sb.Network) > 0 {
		var np NetworkPolicy
		if err := json.Unmarshal(sb.Network, &np); err == nil {
			if np.DefaultAction != "" {
				allow = np.DefaultAction == "allow"
			}
			for _, r := range np.Egress {
				rules = append(rules, cell.EgressRule{Action: r.Action, Target: r.Target, Ports: r.Ports})
			}
		}
	}
	return m.cell.ApplySandboxPolicy(ctx, sb.ID, allow, rules)
}

func encodeURLs(urls []string) string {
	b, _ := json.Marshal(urls)
	return base64.StdEncoding.EncodeToString(b)
}

func cmdErr(res *execd.Result, err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("exit %d: %s %s", res.ExitCode, strings.TrimSpace(res.Stderr), res.Error)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
