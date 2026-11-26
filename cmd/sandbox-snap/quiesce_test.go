package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func putProc(t *testing.T, root string, pid, ppid int, start uint64, state byte) {
	t.Helper()
	fields := []string{strconv.Itoa(pid), "(worker)", string(state), strconv.Itoa(ppid)}
	for field := 5; field <= 22; field++ {
		if field == 22 {
			fields = append(fields, strconv.FormatUint(start, 10))
		} else {
			fields = append(fields, "0")
		}
	}
	write(t, filepath.Join(root, strconv.Itoa(pid), "stat"), []byte(strings.Join(fields, " ")))
}

func recorder(signals map[int][]syscall.Signal) signalFunc {
	return func(pid int, sig syscall.Signal) error {
		signals[pid] = append(signals[pid], sig)
		return nil
	}
}

func TestFreezeProtectsAncestorsAndRescansForChildren(t *testing.T) {
	root, session := t.TempDir(), filepath.Join(t.TempDir(), "frozen.json")
	putProc(t, root, 1, 0, 1, 'S')
	putProc(t, root, 2, 1, 2, 'R') // this helper
	putProc(t, root, 3, 2, 3, 'R') // workload
	putProc(t, root, 4, 1, 4, 'Z') // zombie: no signal required
	signals := map[int][]syscall.Signal{}
	forked := false
	signal := func(pid int, sig syscall.Signal) error {
		signals[pid] = append(signals[pid], sig)
		if pid == 3 && !forked {
			forked = true
			putProc(t, root, 5, 3, 5, 'R') // child appears during the first scan
		}
		return nil
	}
	if err := freezeProcesses(root, 2, session, signal); err != nil {
		t.Fatal(err)
	}
	if len(signals[1]) != 0 || len(signals[2]) != 0 || len(signals[4]) != 0 {
		t.Fatalf("protected or zombie process was signalled: %+v", signals)
	}
	for _, pid := range []int{3, 5} {
		if len(signals[pid]) != 1 || signals[pid][0] != syscall.SIGSTOP {
			t.Fatalf("pid %d was not stopped exactly once: %+v", pid, signals)
		}
	}
	if err := thawProcesses(root, session, signal); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{3, 5} {
		if got := signals[pid][len(signals[pid])-1]; got != syscall.SIGCONT {
			t.Fatalf("pid %d was not resumed: %+v", pid, signals)
		}
	}
	if err := thawProcesses(root, session, signal); err != nil {
		t.Fatalf("thaw must be idempotent: %v", err)
	}
}

func TestThawDoesNotSignalReusedPID(t *testing.T) {
	root, session := t.TempDir(), filepath.Join(t.TempDir(), "frozen.json")
	putProc(t, root, 1, 0, 1, 'S')
	putProc(t, root, 2, 1, 2, 'R')
	putProc(t, root, 3, 2, 3, 'R')
	if err := freezeProcesses(root, 2, session, recorder(map[int][]syscall.Signal{})); err != nil {
		t.Fatal(err)
	}
	// Simulate exit and PID reuse with a different kernel start time.
	putProc(t, root, 3, 2, 999, 'R')
	signals := map[int][]syscall.Signal{}
	if err := thawProcesses(root, session, recorder(signals)); err != nil {
		t.Fatal(err)
	}
	if len(signals[3]) != 0 {
		t.Fatalf("reused PID received SIGCONT: %+v", signals)
	}
	if _, err := os.Stat(session); !os.IsNotExist(err) {
		t.Fatalf("session must be removed after successful thaw: %v", err)
	}
}

func TestThawSkipsExitedProcesses(t *testing.T) {
	root, session := t.TempDir(), filepath.Join(t.TempDir(), "frozen.json")
	putProc(t, root, 1, 0, 1, 'S')
	putProc(t, root, 2, 1, 2, 'R')
	putProc(t, root, 3, 2, 3, 'T')
	if err := freezeProcesses(root, 2, session, recorder(map[int][]syscall.Signal{})); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "3")); err != nil {
		t.Fatal(err)
	}
	if err := thawProcesses(root, session, recorder(map[int][]syscall.Signal{})); err != nil {
		t.Fatal(err)
	}
}
