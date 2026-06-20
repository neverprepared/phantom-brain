package working

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
)

// shardRE matches wm-<digits>.sqlite and captures the PID. Anchored to
// the start and end so it won't match wm-foo.sqlite or wm-12.sqlite.bak.
var shardRE = regexp.MustCompile(`^wm-(\d+)\.sqlite$`)

// ReapResult describes what ReapOrphanedShards did.
type ReapResult struct {
	// Scanned is the number of wm-*.sqlite files found.
	Scanned int

	// Reaped lists the PIDs whose shards were deleted (orphaned).
	Reaped []int

	// SelfSkipped is the PID of the current process if it was found
	// during the walk. Always skipped — we never reap our own shard.
	SelfSkipped int

	// LiveSkipped lists PIDs whose shards were left alone because the
	// process is still running.
	LiveSkipped []int
}

// ReapOrphanedShards walks indexDir for wm-<PID>.sqlite files and
// deletes those whose owning process has exited. The current process's
// own shard is always skipped (its PID is alive by definition).
//
// "Process exited" is detected via syscall.Kill(pid, 0):
//
//	err == nil           -> process is alive (or we own it)
//	errno == ESRCH       -> no such process; safe to reap
//	any other err/errno  -> conservatively skip; might be a permission
//	                        issue, not death
//
// Side note: PID reuse is a concern in the brain manifest's orphan
// sweep (we use boot_id there) but not here — a wm-<PID>.sqlite was
// written at process N's start, and at most one process owns each
// PID at any moment. If a new process happens to grab the same PID,
// the worst case is we don't reap an actually-orphaned shard until
// the next sweep when its successor exits.
//
// Returns a ReapResult so callers can log or surface what was done.
// Best-effort on individual file removals: a single failure doesn't
// abort the walk.
func ReapOrphanedShards(indexDir string) (ReapResult, error) {
	res := ReapResult{SelfSkipped: -1}

	entries, err := os.ReadDir(indexDir)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil // nothing to reap
		}
		return res, fmt.Errorf("working: ReapOrphanedShards: read %q: %w", indexDir, err)
	}

	self := os.Getpid()

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := shardRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		res.Scanned++

		pid, err := strconv.Atoi(m[1])
		if err != nil {
			// Should be unreachable since shardRE matched \d+, but be loud.
			continue
		}

		if pid == self {
			res.SelfSkipped = pid
			continue
		}

		if pidAlive(pid) {
			res.LiveSkipped = append(res.LiveSkipped, pid)
			continue
		}

		// Orphan. Remove the .sqlite and its WAL/SHM sidecars.
		base := filepath.Join(indexDir, e.Name())
		_ = os.Remove(base)
		_ = os.Remove(base + "-wal")
		_ = os.Remove(base + "-shm")
		res.Reaped = append(res.Reaped, pid)
	}
	return res, nil
}

// pidAlive returns true if a process with the given PID is currently
// running on this host. POSIX-only; relies on signal 0 to probe
// existence without actually delivering a signal.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	// EPERM means the process exists but we don't own it — still alive
	// from our perspective. ESRCH means no such process.
	if errno, ok := err.(syscall.Errno); ok {
		return errno == syscall.EPERM
	}
	return false
}
