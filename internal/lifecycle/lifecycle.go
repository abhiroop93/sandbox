// Package lifecycle orchestrates sandbox state transitions (D6): create → Running, pause (snapshot /workspace
// to object storage, release the pod), resume (claim any warm pod, bind, hydrate), delete, expiry, reconciliation.
// Long operations run asynchronously; the API returns 202 and clients observe state (OpenSandbox semantics).
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

	"github.com/abhiroop93/sandbox/internal/cell"
	"github.com/abhiroop93/sandbox/internal/execd"
	"github.com/abhiroop93/sandbox/internal/snapshot"
	"github.com/abhiroop93/sandbox/internal/store"
)

const (
	Workspace  = "/workspace"
	SnapTool   = cell.SnapTool
	snapOutDir = "/workspace/.srvm-snap"
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
	Image      string
	Entrypoint []string
	Env        map[string]string
	Metadata   map[string]string
	Timeout    *time.Duration // nil = no expiry
	Network    *NetworkPolicy
}

type Manager struct {
	st            *store.Store
	cell          *cell.Cell
	snap          *snapshot.Store
	poolSize      int
	defaultEgress string // "deny" | "allow"
	log           *slog.Logger
	// ShutdownGrace bounds an emergency save: the pod is killed after this anyway (matches the pod grace period).
	ShutdownGrace time.Duration
}

func New(st *store.Store, c *cell.Cell, snap *snapshot.Store, poolSize int, defaultEgress string, log *slog.Logger) *Manager {
	return &Manager{st: st, cell: c, snap: snap, poolSize: poolSize, defaultEgress: defaultEgress, log: log,
		ShutdownGrace: 90 * time.Second}
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

// Create records the sandbox (quota-checked) and provisions it asynchronously.
func (m *Manager) Create(ctx context.Context, tenant string, spec CreateSpec) (*store.Sandbox, error) {
	if err := ValidateNetwork(spec.Network); err != nil {
		return nil, err
	}
	tmpl, err := m.cell.EnsureTemplate(ctx, spec.Image, m.poolSize)
	if err != nil {
		return nil, fmt.Errorf("template: %w", err)
	}
	sb := &store.Sandbox{ID: uuid.NewString(), Tenant: tenant, Cell: "cell-1", Template: tmpl, Image: spec.Image,
		Entrypoint: spec.Entrypoint, Env: spec.Env, Metadata: spec.Metadata}
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
	go m.start(sb.ID, false)
	return m.st.Get(ctx, tenant, sb.ID)
}

// start claims a warm pod for a Pending/Resuming sandbox, binds execd, applies network policy, hydrates (resume).
func (m *Manager) start(id string, hydrate bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	t0 := time.Now()
	from := []string{store.Pending}
	if hydrate {
		from = []string{store.Resuming}
	}
	// Bump generation + reserve a deterministic claim name atomically (fencing, D6).
	sb, err := m.st.Transition(ctx, id, from, from[0], "", store.Patch{BumpGeneration: true})
	if err != nil {
		m.log.Error("start: transition", "id", id, "err", err)
		return
	}
	fail := func(stage string, err error) {
		m.log.Error("start failed", "id", id, "stage", stage, "err", err)
		msg := stage + ": " + err.Error()
		if sb.ClaimName != nil {
			_ = m.cell.Release(context.Background(), *sb.ClaimName)
		}
		_, _ = m.st.Transition(context.Background(), id, from, store.Failed, "failed",
			store.Patch{StateMessage: &msg, ClaimName: store.NullStr(), PodName: store.NullStr(), PodIP: store.NullStr(), ExecdToken: store.NullStr()})
	}
	claim, err := m.cell.Claim(ctx, sb.Template, sb.ID, sb.Tenant, sb.Generation)
	if err != nil {
		fail("claim", err)
		return
	}
	sb.ClaimName = &claim
	if sb, err = m.st.Transition(ctx, id, from, from[0], "", store.Patch{ClaimName: store.Str(claim)}); err != nil {
		_ = m.cell.Release(ctx, claim)
		m.log.Error("start: record claim", "id", id, "err", err)
		return
	}
	pod, ip, err := m.cell.WaitReady(ctx, claim, 5*time.Minute)
	if err != nil {
		fail("wait-ready", err)
		return
	}
	tClaim := time.Since(t0)
	if err := m.applyPolicy(ctx, sb); err != nil {
		fail("network-policy", err)
		return
	}
	token := execd.NewToken()
	if err := execd.Bind(ctx, ip, sb.ID, sb.Generation, token, sb.Env, sb.Tenant); err != nil {
		fail("execd-bind", err)
		return
	}
	tBind := time.Since(t0)
	var tHydrate time.Duration
	if hydrate && sb.SnapshotKey != nil {
		urls, err := m.snap.GetURLs(ctx, *sb.SnapshotKey)
		if err != nil {
			fail("presign-get", err)
			return
		}
		if len(urls) > 0 {
			b64 := encodeURLs(urls)
			res, err := execd.Run(ctx, ip, token, []string{SnapTool, "hydrate", Workspace, b64}, Workspace, 10*time.Minute)
			if err != nil || res.ExitCode != 0 {
				fail("hydrate", cmdErr(res, err))
				return
			}
		}
		tHydrate = time.Since(t0) - tBind
	}
	ev := "started"
	if hydrate {
		ev = "resumed"
	}
	if _, err := m.st.Transition(ctx, id, from, store.Running, ev, store.Patch{PodName: store.Str(pod), PodIP: store.Str(ip),
		ExecdToken: store.Str(token)}); err != nil {
		_ = m.cell.Release(ctx, claim) // sandbox was deleted while starting
		return
	}
	m.protect(ctx, sb.ID, pod, ip, token) // after Running: not on the create latency path
	m.log.Info("sandbox running", "id", id, "gen", sb.Generation, "resume", hydrate, "claim_ms", tClaim.Milliseconds(),
		"bind_ms", (tBind - tClaim).Milliseconds(), "hydrate_ms", tHydrate.Milliseconds(), "total_ms", time.Since(t0).Milliseconds())
}

// protect arms the pod's preStop hold (so a graceful shutdown waits for srvm to save the workspace) and marks it
// not-safe-to-evict for the cluster autoscaler. Best effort: a failure only weakens interruption handling.
func (m *Manager) protect(ctx context.Context, id, pod, ip, token string) {
	if res, err := execd.Run(ctx, ip, token, []string{SnapTool, "guard", "arm", cell.GuardMarker}, Workspace, 30*time.Second); err != nil || res.ExitCode != 0 {
		m.log.Warn("arm shutdown guard failed", "id", id, "err", cmdErr(res, err))
	}
	if err := m.cell.MarkBound(ctx, pod); err != nil {
		m.log.Warn("mark pod bound failed", "id", id, "err", err)
	}
}

// disarm lets the pod's preStop hook return, so a pod srvm is releasing shuts down at once.
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

// Pause snapshots /workspace to object storage and releases the pod (async).
func (m *Manager) Pause(ctx context.Context, tenant, id string) error {
	if _, err := m.st.Get(ctx, tenant, id); err != nil {
		return err
	}
	sb, err := m.st.Transition(ctx, id, []string{store.Running}, store.Pausing, "", store.Patch{})
	if err != nil {
		return err
	}
	go m.pause(sb, false)
	return nil
}

// pause snapshots and releases the pod. emergency=true: the cluster is already shutting the pod down (drain,
// eviction, scale-down); the preStop hook holds it for at most the grace period while this runs.
func (m *Manager) pause(sb *store.Sandbox, emergency bool) {
	limit := 15 * time.Minute
	if emergency {
		limit = m.ShutdownGrace // the pod may be on a dead node: don't wait longer than it can live
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	t0 := time.Now()
	back := func(stage string, err error) {
		m.log.Error("pause failed", "id", sb.ID, "stage", stage, "err", err, "emergency", emergency)
		if emergency { // the pod is going away: fall back to the last good snapshot (or Failed)
			m.markLost(context.Background(), sb, []string{store.Pausing},
				"pod was shut down by the cluster before its workspace could be saved ("+stage+": "+err.Error()+")")
			m.disarm(sb)
			return
		}
		// pod is still alive → back to Running, nothing lost
		msg := "pause failed (" + stage + "): " + err.Error()
		_, _ = m.st.Transition(context.Background(), sb.ID, []string{store.Pausing}, store.Running, "", store.Patch{StateMessage: &msg})
	}
	ip, token := deref(sb.PodIP), deref(sb.ExecdToken)
	res, err := execd.Run(ctx, ip, token, []string{SnapTool, "pack", Workspace, snapOutDir}, Workspace, 10*time.Minute)
	if err != nil || res.ExitCode != 0 {
		back("pack", cmdErr(res, err))
		return
	}
	var packed struct {
		Chunks int
		Bytes  int64
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(lastLine(res.Stdout))), &packed); err != nil {
		back("pack-output", fmt.Errorf("%v: %q", err, res.Stdout))
		return
	}
	tPack := time.Since(t0)
	prefix := snapshot.Prefix(sb.ID, sb.Generation)
	if prefix == deref(sb.SnapshotKey) {
		back("snapshot-key", fmt.Errorf("refusing to overwrite the current snapshot %s", prefix))
		return
	}
	// New snapshots go under this generation's prefix; the sandbox switches to it only after a complete upload, so
	// the last good snapshot is never overwritten. Clear leftovers of an earlier failed attempt first: hydrate reads
	// every chunk under the prefix.
	if err := m.snap.Delete(ctx, prefix); err != nil {
		back("clear-prefix", err)
		return
	}
	urls, err := m.snap.PutURLs(ctx, prefix, packed.Chunks)
	if err != nil {
		back("presign-put", err)
		return
	}
	res, err = execd.Run(ctx, ip, token, []string{SnapTool, "upload", snapOutDir, encodeURLs(urls)}, Workspace, 10*time.Minute)
	if err != nil || res.ExitCode != 0 {
		back("upload", cmdErr(res, err))
		return
	}
	tUpload := time.Since(t0) - tPack
	old := deref(sb.SnapshotKey)
	claim := deref(sb.ClaimName)
	bytes := packed.Bytes
	pb := &bytes
	msg := ""
	if emergency {
		msg = "pod was shut down by the cluster (drain, eviction or scale-down); workspace files were saved, " +
			"running processes were not. Resume to continue."
	}
	if _, err := m.st.Transition(ctx, sb.ID, []string{store.Pausing}, store.Paused, "paused", store.Patch{
		StateMessage: &msg, SnapshotKey: store.Str(prefix), SnapshotBytes: &pb, ClaimName: store.NullStr(),
		PodName: store.NullStr(), PodIP: store.NullStr(), ExecdToken: store.NullStr()}); err != nil {
		m.log.Error("pause: record", "id", sb.ID, "err", err)
		return
	}
	m.disarm(sb) // saved: let the preStop hook return so the pod goes away now
	if claim != "" {
		_ = m.cell.Release(ctx, claim) // pod deleted, never reused
	}
	if old != "" && old != prefix {
		_ = m.snap.Delete(ctx, old)
	}
	m.log.Info("sandbox paused", "id", sb.ID, "gen", sb.Generation, "emergency", emergency, "bytes", packed.Bytes, "chunks", packed.Chunks,
		"pack_ms", tPack.Milliseconds(), "upload_ms", tUpload.Milliseconds(), "total_ms", time.Since(t0).Milliseconds())
}

// Resume claims any warm pod for the template, binds it and hydrates /workspace (async).
func (m *Manager) Resume(ctx context.Context, tenant, id string) error {
	if _, err := m.st.Get(ctx, tenant, id); err != nil {
		return err
	}
	if _, err := m.st.Transition(ctx, id, []string{store.Paused}, store.Resuming, "", store.Patch{}); err != nil {
		return err
	}
	go m.start(id, true)
	return nil
}

// Delete terminates a sandbox: release pod, drop policy and snapshots.
func (m *Manager) Delete(ctx context.Context, tenant, id string) error {
	if _, err := m.st.Get(ctx, tenant, id); err != nil {
		return err
	}
	return m.terminate(ctx, id, "terminated")
}

func (m *Manager) terminate(ctx context.Context, id, ev string) error {
	live := []string{store.Pending, store.Running, store.Pausing, store.Paused, store.Resuming, store.Failed}
	sb, err := m.st.Transition(ctx, id, live, store.Stopping, "", store.Patch{})
	if err != nil {
		return err
	}
	m.disarm(sb) // deleting: nothing to save, don't let the preStop hook hold the pod
	if sb.ClaimName != nil {
		_ = m.cell.Release(ctx, *sb.ClaimName)
	}
	_ = m.cell.DeleteSandboxPolicy(ctx, id)
	_ = m.snap.Delete(ctx, "ws/"+id+"/")
	_, err = m.st.Transition(ctx, id, []string{store.Stopping}, store.Terminated, ev, store.Patch{
		ClaimName: store.NullStr(), PodName: store.NullStr(), PodIP: store.NullStr(), ExecdToken: store.NullStr(), SnapshotKey: store.NullStr()})
	return err
}

// Renew sets a new expiry.
func (m *Manager) Renew(ctx context.Context, tenant, id string, at time.Time) (*store.Sandbox, error) {
	cur, err := m.st.Get(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	t := at.UTC()
	tp := &t
	return m.st.Transition(ctx, id, []string{cur.State}, cur.State, "", store.Patch{StateMessage: &cur.StateMessage, ExpiresAt: &tp})
}

// RecoverInterrupted settles operations that were in flight when the server stopped. Async steps run in-process
// (no durable queue yet, see roadmap #1), so after a restart nothing will ever finish them, and Pending/Resuming
// rows would hold tenant quota forever. Must run before serving traffic (single replica).
func (m *Manager) RecoverInterrupted(ctx context.Context) {
	stuck, err := m.st.InStates(ctx, []string{store.Pending, store.Resuming, store.Pausing})
	if err != nil {
		m.log.Error("recover: list", "err", err)
		return
	}
	for _, sb := range stuck {
		claim := deref(sb.ClaimName)
		if sb.State == store.Pausing && claim != "" {
			if _, _, err := m.cell.WaitReady(ctx, claim, 2*time.Second); err == nil {
				// The pod is still alive: nothing was lost, the pause just didn't finish. Let the user retry.
				msg := "pause interrupted by server restart; retry pause"
				_, _ = m.st.Transition(ctx, sb.ID, []string{store.Pausing}, store.Running, "", store.Patch{StateMessage: &msg})
				m.log.Warn("recovered interrupted pause", "id", sb.ID)
				continue
			}
		}
		if claim != "" {
			_ = m.cell.Release(ctx, claim)
		}
		msg := strings.ToLower(sb.State) + " interrupted by server restart; create or resume again"
		if sb.State == store.Resuming || sb.State == store.Pausing {
			if sb.SnapshotKey != nil {
				// A previous snapshot exists: keep the workspace resumable instead of failing it.
				m2 := msg + " (last snapshot kept)"
				_, _ = m.st.Transition(ctx, sb.ID, []string{sb.State}, store.Paused, "", store.Patch{StateMessage: &m2,
					ClaimName: store.NullStr(), PodName: store.NullStr(), PodIP: store.NullStr(), ExecdToken: store.NullStr()})
				m.log.Warn("recovered interrupted operation to Paused", "id", sb.ID, "was", sb.State)
				continue
			}
		}
		_, _ = m.st.Transition(ctx, sb.ID, []string{sb.State}, store.Failed, "failed", store.Patch{StateMessage: &msg,
			ClaimName: store.NullStr(), PodName: store.NullStr(), PodIP: store.NullStr(), ExecdToken: store.NullStr()})
		m.log.Warn("recovered interrupted operation to Failed", "id", sb.ID, "was", sb.State)
	}
}

// Loop runs expiry and two-way reconciliation until ctx ends (D6: reconcile Postgres ⇄ CRDs).
func (m *Manager) Loop(ctx context.Context) {
	go m.cell.WatchTerminating(ctx, m.onTerminating)
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
					m.log.Info("sandbox expired", "id", sb.ID)
				}
			}
		}
		m.reconcile(ctx)
		m.checkPods(ctx)
	}
}

// ---------------------------------------------------------------- pod interruptions

type verdict int

const (
	podOK          verdict = iota
	podTerminating         // graceful shutdown in progress: save the workspace now
	podRestarted           // sandbox container restarted in place (e.g. OOM): files kept, execd must be re-bound
	podLost                // pod gone or replaced: fall back to the last snapshot
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

// checkPods compares every Running sandbox with its pod (one pod list per tick).
func (m *Manager) checkPods(ctx context.Context) {
	running, err := m.st.InStates(ctx, []string{store.Running})
	if err != nil || len(running) == 0 {
		return
	}
	pods, err := m.cell.SandboxPods(ctx)
	if err != nil {
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

// onTerminating starts an emergency save when the cluster begins shutting down a Running sandbox's pod.
// Idempotent: only the Running → Pausing transition winner proceeds.
func (m *Manager) onTerminating(pod, id string) {
	ctx := context.Background()
	cur, err := m.st.Get(ctx, "", id)
	if err != nil || cur.State != store.Running || deref(cur.PodName) != pod {
		return // already pausing/stopping (srvm released it itself) or a stale pod
	}
	msg := "pod is being shut down by the cluster; saving workspace"
	sb, err := m.st.Transition(ctx, id, []string{store.Running}, store.Pausing, "", store.Patch{StateMessage: &msg})
	if err != nil {
		return
	}
	m.log.Warn("pod shutting down, emergency save", "id", id, "pod", pod)
	go m.pause(sb, true)
}

// markLost settles a sandbox whose pod is gone: resumable from the last snapshot if there is one, else Failed.
func (m *Manager) markLost(ctx context.Context, sb *store.Sandbox, from []string, why string) {
	clear := store.Patch{ClaimName: store.NullStr(), PodName: store.NullStr(), PodIP: store.NullStr(), ExecdToken: store.NullStr()}
	to, msg := store.Failed, why+"; no saved snapshot, workspace lost"
	if sb.SnapshotKey != nil {
		to, msg = store.Paused, why+"; changes since the last pause were lost. Resume to continue from the last snapshot."
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

// rebind re-initialises execd after the sandbox container restarted inside the same pod. /workspace is an emptyDir
// of the pod, so files survive; running processes do not.
func (m *Manager) rebind(ctx context.Context, sb *store.Sandbox, p cell.PodInfo, why string) {
	ip, token := deref(sb.PodIP), deref(sb.ExecdToken)
	if err := execd.Bind(ctx, ip, sb.ID, sb.Generation, token, sb.Env, sb.Tenant); err != nil {
		m.log.Error("rebind after container restart failed", "id", sb.ID, "err", err)
		return // retried next tick
	}
	m.protect(ctx, sb.ID, p.Name, ip, token)
	msg := "sandbox container restarted (" + why + "); files kept, running processes were lost"
	_, _ = m.st.Transition(ctx, sb.ID, []string{store.Running}, store.Running, "restarted", store.Patch{StateMessage: &msg})
	m.log.Warn("sandbox container restarted, re-bound", "id", sb.ID, "why", why)
}

func (m *Manager) reconcile(ctx context.Context) {
	known, err := m.st.ActiveClaims(ctx)
	if err != nil {
		return
	}
	live, err := m.cell.ListClaims(ctx)
	if err != nil {
		return
	}
	for claim, sbID := range live {
		if _, ok := known[claim]; !ok {
			// Orphan: claim exists in the cluster but not in Postgres (crash between claim and record, or deleted).
			if cur, err := m.st.Get(ctx, "", sbID); err == nil && (cur.State == store.Pending || cur.State == store.Resuming) {
				continue // still being started
			}
			m.log.Warn("reaping orphan claim", "claim", claim, "sandbox", sbID)
			_ = m.cell.Release(ctx, claim)
		}
	}
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
