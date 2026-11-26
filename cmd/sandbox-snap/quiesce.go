package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processRef identifies a process incarnation, not merely a reusable PID.
type processRef struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"`
}

type procStat struct {
	PPID  int
	Start uint64
	State byte
}

type frozenProcesses struct {
	Processes []processRef `json:"processes"`
}

func readProcStat(root string, pid int) (procStat, error) {
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	// The command in field 2 may contain spaces. Fields after its final ')'
	// begin with state(3), ppid(4); starttime(22) is index 19 in that suffix.
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return procStat{}, fmt.Errorf("malformed stat for pid %d", pid)
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) < 20 || len(fields[0]) != 1 {
		return procStat{}, fmt.Errorf("short stat for pid %d", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return procStat{}, fmt.Errorf("ppid for pid %d: %w", pid, err)
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("start time for pid %d: %w", pid, err)
	}
	return procStat{PPID: ppid, Start: start, State: fields[0][0]}, nil
}

func readProcesses(root string) (map[int]procStat, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := map[int]procStat{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		st, err := readProcStat(root, pid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[pid] = st
	}
	return out, nil
}

func protectedProcesses(root string, self int) (map[int]bool, error) {
	protected := map[int]bool{1: true}
	for pid, depth := self, 0; pid > 1; depth++ {
		if depth > 64 {
			return nil, fmt.Errorf("process parent cycle at pid %d", pid)
		}
		if protected[pid] {
			return nil, fmt.Errorf("process parent cycle at pid %d", pid)
		}
		protected[pid] = true
		st, err := readProcStat(root, pid)
		if err != nil {
			return nil, fmt.Errorf("protect process ancestor %d: %w", pid, err)
		}
		pid = st.PPID
	}
	return protected, nil
}

func saveFrozen(path string, refs map[int]processRef) error {
	out := frozenProcesses{Processes: make([]processRef, 0, len(refs))}
	for _, ref := range refs {
		out.Processes = append(out.Processes, ref)
	}
	// Persist in PID order for reproducible diagnostics and tests.
	sort.Slice(out.Processes, func(i, j int) bool { return out.Processes[i].PID < out.Processes[j].PID })
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".freeze-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func loadFrozen(path string) (*frozenProcesses, error) {
	var out frozenProcesses
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &out, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, json.Unmarshal(b, &out)
}

type signalFunc func(pid int, sig syscall.Signal) error

// freezeProcesses stops workload processes while preserving the current helper
// and its execd/runner ancestors. Existing sessions are included so a replayed
// pack also owns processes stopped before a server interruption.
func freezeProcesses(root string, self int, path string, signal signalFunc) error {
	protected, err := protectedProcesses(root, self)
	if err != nil {
		return err
	}
	previous, err := loadFrozen(path)
	if err != nil {
		return err
	}
	refs := map[int]processRef{}
	for _, ref := range previous.Processes {
		refs[ref.PID] = ref
	}
	stable := 0
	for stable < 2 {
		procs, err := readProcesses(root)
		if err != nil {
			return err
		}
		// Drop exited processes and protect current ancestors even if their identity
		// changed while retrying an interrupted capture.
		for pid, ref := range refs {
			st, ok := procs[pid]
			if !ok || st.Start != ref.Start {
				delete(refs, pid)
			}
		}
		added := false
		for pid, st := range procs {
			if protected[pid] || st.State == 'Z' {
				continue
			}
			if _, exists := refs[pid]; exists {
				continue
			}
			if err := signal(pid, syscall.SIGSTOP); err != nil && !errors.Is(err, syscall.ESRCH) {
				_ = saveFrozen(path, refs)
				return fmt.Errorf("stop pid %d: %w", pid, err)
			}
			refs[pid] = processRef{PID: pid, Start: st.Start}
			added = true
		}
		if err := saveFrozen(path, refs); err != nil {
			return err
		}
		if added {
			stable = 0
			time.Sleep(20 * time.Millisecond)
		} else {
			stable++
		}
	}
	return nil
}

// thawProcesses is idempotent and never signals a reused PID. The session file
// is retained after a real signal error so another pause attempt can finish it.
func thawProcesses(root, path string, signal signalFunc) error {
	frozen, err := loadFrozen(path)
	if err != nil {
		return err
	}
	if len(frozen.Processes) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	var errs []error
	for _, ref := range frozen.Processes {
		st, err := readProcStat(root, ref.PID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if st.Start != ref.Start {
			continue // PID was reused: it is not the process we stopped.
		}
		if err := signal(ref.PID, syscall.SIGCONT); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("resume pid %d: %w", ref.PID, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func freeze(path string) error {
	return freezeProcesses("/proc", os.Getpid(), path, syscall.Kill)
}

func thaw(path string) error {
	return thawProcesses("/proc", path, syscall.Kill)
}
