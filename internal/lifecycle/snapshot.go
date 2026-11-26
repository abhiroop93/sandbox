package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/abhiroop93/sandbox/internal/execd"
	"github.com/abhiroop93/sandbox/internal/snapshot"
	"github.com/abhiroop93/sandbox/internal/store"
)

type operationPayload struct {
	Hydrate      bool   `json:"hydrate,omitempty"`
	Emergency    bool   `json:"emergency,omitempty"`
	Event        string `json:"event,omitempty"`
	TargetPrefix string `json:"targetPrefix,omitempty"`
}

type packedWorkspace struct {
	Chunks int    `json:"chunks"`
	Bytes  int64  `json:"bytes"`
	Digest string `json:"digest"`
}

func encodePayload(p operationPayload) json.RawMessage {
	b, _ := json.Marshal(p)
	return b
}

func decodePayload(j *store.Job) operationPayload {
	var p operationPayload
	_ = json.Unmarshal(j.Payload, &p)
	return p
}

func nullTime() **time.Time {
	var t *time.Time
	return &t
}

func (m *Manager) setNextAutosave(patch *store.Patch) {
	if m.AutosaveInterval <= 0 {
		return
	}
	t := time.Now().Add(m.AutosaveInterval).UTC()
	tp := &t
	patch.AutosaveAt = &tp
}

func (m *Manager) enqueueCapture(ctx context.Context, sb *store.Sandbox, kind store.JobKind, payload operationPayload,
	from []string, message string) error {
	job := &store.Job{SandboxID: sb.ID, Kind: kind, Generation: sb.Generation, Payload: encodePayload(payload)}
	patch := store.Patch{}
	if message != "" {
		patch.StateMessage = &message
	}
	_, err := m.st.TransitionWithJob(ctx, sb.ID, from, store.Pausing, "", patch, job)
	return err
}

func (m *Manager) scheduleAutosaves(ctx context.Context) {
	due, err := m.st.AutosaveDue(ctx, 100)
	if err != nil {
		m.log.Warn("autosave selection failed", "err", err)
		return
	}
	for _, sb := range due {
		p := operationPayload{TargetPrefix: snapshot.AutosavePrefix(sb.ID, sb.Generation, time.Now().UnixNano())}
		if err := m.enqueueCapture(ctx, sb, store.JobAutosave, p, []string{store.Running},
			"automatic workspace archive in progress; processes are briefly quiesced while it is packed"); err != nil &&
			!errors.Is(err, store.ErrConflict) {
			m.log.Warn("queue autosave", "id", sb.ID, "err", err)
		}
	}
}

// packWorkspace stops workload processes only for archive construction. The
// output is immutable before upload, so execution resumes before object-store I/O.
func (m *Manager) packWorkspace(ctx context.Context, sb *store.Sandbox) (packed *packedWorkspace, err error) {
	ip, token := deref(sb.PodIP), deref(sb.ExecdToken)
	if ip == "" || token == "" {
		return nil, fmt.Errorf("sandbox has no bound execd endpoint")
	}
	res, freezeErr := execd.Run(ctx, ip, token, []string{SnapTool, "freeze", freezePath}, Workspace, 30*time.Second)
	if freezeErr != nil || res.ExitCode != 0 {
		_ = m.thaw(ip, token)
		return nil, fmt.Errorf("freeze: %w", cmdErr(res, freezeErr))
	}
	res, packErr := execd.Run(ctx, ip, token, []string{SnapTool, "pack", Workspace, snapOutDir}, Workspace, 10*time.Minute)
	thawErr := m.thaw(ip, token)
	if packErr != nil || res.ExitCode != 0 {
		if thawErr != nil {
			return nil, errors.Join(fmt.Errorf("pack: %w", cmdErr(res, packErr)), thawErr)
		}
		return nil, fmt.Errorf("pack: %w", cmdErr(res, packErr))
	}
	if thawErr != nil {
		return nil, fmt.Errorf("thaw: %w", thawErr)
	}
	var out packedWorkspace
	if err := json.Unmarshal([]byte(strings.TrimSpace(lastLine(res.Stdout))), &out); err != nil {
		return nil, fmt.Errorf("pack output: %v: %q", err, res.Stdout)
	}
	if out.Digest == "" {
		return nil, fmt.Errorf("pack output is missing its archive digest")
	}
	return &out, nil
}

func (m *Manager) thaw(ip, token string) error {
	if ip == "" || token == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := execd.Run(ctx, ip, token, []string{SnapTool, "thaw", freezePath}, Workspace, 30*time.Second)
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("thaw process set: %w", cmdErr(res, err))
	}
	return nil
}

// executeCapture publishes a new snapshot pointer only after a complete upload.
// For autosave the pod and generation stay bound; explicit pause releases them.
func (m *Manager) executeCapture(ctx context.Context, workerID string, job *store.Job, payload operationPayload) error {
	sb, err := m.st.Get(ctx, "", job.SandboxID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if sb.State != store.Pausing || sb.Generation != job.Generation {
		return nil
	}
	autosave := job.Kind == store.JobAutosave
	prefix := payload.TargetPrefix
	if prefix == "" {
		prefix = snapshot.Prefix(sb.ID, sb.Generation)
	}
	if prefix == deref(sb.SnapshotKey) {
		return fmt.Errorf("refusing to overwrite current snapshot %s", prefix)
	}
	started := time.Now()
	packed, err := m.packWorkspace(ctx, sb)
	if err != nil {
		return err
	}
	packDone := time.Since(started)

	unchanged := autosave && sb.SnapshotKey != nil && sb.SnapshotDigest != "" && packed.Digest == sb.SnapshotDigest
	if !unchanged {
		// A retry deliberately clears every old chunk under its own target before
		// upload. Hydrate reads all chunks under a prefix.
		if err := m.snap.Delete(ctx, prefix); err != nil {
			return fmt.Errorf("clear snapshot prefix: %w", err)
		}
		urls, err := m.snap.PutURLs(ctx, prefix, packed.Chunks)
		if err != nil {
			return fmt.Errorf("presign snapshot upload: %w", err)
		}
		res, err := execd.Run(ctx, deref(sb.PodIP), deref(sb.ExecdToken),
			[]string{SnapTool, "upload", snapOutDir, encodeURLs(urls)}, Workspace, 10*time.Minute)
		if err != nil || res.ExitCode != 0 {
			return fmt.Errorf("upload snapshot: %w", cmdErr(res, err))
		}
	}

	old := deref(sb.SnapshotKey)
	patch := store.Patch{}
	to, event := store.Running, "autosaved"
	message := "workspace autosaved; processes continued after packing"
	if unchanged {
		message = "workspace unchanged; autosave deadline advanced without upload"
	}
	if !unchanged {
		bytes := packed.Bytes
		bytesPtr := &bytes
		patch.SnapshotKey = store.Str(prefix)
		patch.SnapshotBytes = &bytesPtr
		patch.SnapshotDigest = &packed.Digest
	}
	if autosave {
		m.setNextAutosave(&patch)
	} else {
		to, event = store.Paused, "paused"
		message = ""
		if payload.Emergency {
			message = "pod was shut down by the cluster (drain, eviction or scale-down); workspace files were saved, running processes were not. Resume to continue."
		}
		patch.ClaimName = store.NullStr()
		patch.PodName = store.NullStr()
		patch.PodIP = store.NullStr()
		patch.ExecdToken = store.NullStr()
		patch.AutosaveAt = nullTime()
	}
	patch.StateMessage = &message
	if _, err := m.st.TransitionWithJob(ctx, sb.ID, []string{store.Pausing}, to, event, patch, job); err != nil {
		return err
	}
	if !autosave {
		m.disarm(sb)
		if claim := deref(sb.ClaimName); claim != "" {
			_ = m.cell.Release(context.Background(), claim)
		}
	}
	if !unchanged && old != "" && old != prefix {
		if err := m.snap.Delete(context.Background(), old); err != nil {
			m.log.Warn("delete previous snapshot", "id", sb.ID, "prefix", old, "err", err)
		}
	}
	m.log.Info("workspace capture committed", "id", sb.ID, "gen", sb.Generation, "autosave", autosave,
		"emergency", payload.Emergency, "unchanged", unchanged, "bytes", packed.Bytes, "chunks", packed.Chunks,
		"pack_ms", packDone.Milliseconds(), "upload_ms", (time.Since(started) - packDone).Milliseconds(),
		"total_ms", time.Since(started).Milliseconds())
	return nil
}

func (m *Manager) failCapture(ctx context.Context, workerID string, job *store.Job, payload operationPayload, cause error) {
	sb, err := m.st.Get(ctx, "", job.SandboxID)
	if err != nil {
		m.retryJob(workerID, job, err)
		return
	}
	_ = m.thaw(deref(sb.PodIP), deref(sb.ExecdToken))
	patch := store.Patch{ClaimName: store.NullStr(), PodName: store.NullStr(), PodIP: store.NullStr(), ExecdToken: store.NullStr(), AutosaveAt: nullTime()}
	if payload.Emergency {
		to := store.Failed
		msg := "pod was shut down before its workspace could be saved: " + cause.Error() + "; no saved snapshot, workspace lost"
		if sb.SnapshotKey != nil {
			to = store.Paused
			msg = "pod was shut down before its workspace could be saved: " + cause.Error() + "; resuming from the last saved snapshot"
		}
		patch.StateMessage = &msg
		if _, err := m.st.TransitionWithJob(ctx, sb.ID, []string{store.Pausing}, to, "lost", patch, job); err == nil && sb.ClaimName != nil {
			_ = m.cell.Release(context.Background(), *sb.ClaimName)
		}
		m.disarm(sb)
		return
	}
	to := store.Running
	kind := "pause"
	if job.Kind == store.JobAutosave {
		kind = "autosave"
		m.setNextAutosave(&patch)
	}
	msg := kind + " failed: " + cause.Error() + "; pod preserved and processes resumed"
	patch = store.Patch{StateMessage: &msg}
	if job.Kind == store.JobAutosave {
		m.setNextAutosave(&patch)
	}
	_, _ = m.st.TransitionWithJob(ctx, sb.ID, []string{store.Pausing}, to, "", patch, job)
}

func (m *Manager) hydrate(ctx context.Context, sb *store.Sandbox, ip, token string) error {
	urls, err := m.snap.GetURLs(ctx, *sb.SnapshotKey)
	if err != nil {
		return fmt.Errorf("presign-get: %w", err)
	}
	if len(urls) == 0 {
		return nil
	}
	res, err := execd.Run(ctx, ip, token, []string{SnapTool, "hydrate", Workspace, encodeURLs(urls)}, Workspace, 10*time.Minute)
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("hydrate: %w", cmdErr(res, err))
	}
	return nil
}
