package brain

import (
	"fmt"
	"os"
	"time"
)

// IsGCEligible decides whether a brain directory's manifest qualifies
// for local garbage collection. Shared by the opportunistic sweep in
// Recover() and the `pbrainctl gc-brains` operator subcommand — if the
// signature changes, both callers must move together.
//
// Eligibility rules:
//   - retention <= 0 disables GC entirely.
//   - nil or non-dead manifests are kept; only StatusDead brains can be
//     reclaimed.
//   - "age" is measured from LastHeartbeat when parseable; otherwise it
//     falls back to the manifest file's mtime. Brains younger than
//     retention are kept (operators may still want to inspect them).
//
// Returns (true, reason) when the brain should be deleted, otherwise
// (false, reason). The reason string is operator-facing and lands in
// RecoverySweepResult.DeleteSkipped for kept-but-inspected brains.
func IsGCEligible(m *Manifest, dir string, now time.Time, retention time.Duration) (bool, string) {
	if retention <= 0 {
		return false, "gc disabled"
	}
	if m == nil {
		return false, "nil manifest"
	}
	if m.Status != StatusDead {
		return false, fmt.Sprintf("status=%s", m.Status)
	}

	age, source := manifestAge(m, dir, now)
	if age < 0 {
		return false, "age unknown"
	}
	if age < retention {
		return false, fmt.Sprintf("too young (age=%s via %s)", age.Truncate(time.Second), source)
	}
	return true, fmt.Sprintf("eligible (age=%s via %s)", age.Truncate(time.Second), source)
}

// manifestAge returns how old the brain looks plus the source of the
// timestamp used. LastHeartbeat is authoritative; falls back to the
// manifest's mtime when the heartbeat field is empty or unparseable so
// brains that crashed before their first heartbeat tick can still be
// reaped.
func manifestAge(m *Manifest, dir string, now time.Time) (time.Duration, string) {
	if last, err := time.Parse(time.RFC3339, m.LastHeartbeat); err == nil {
		return now.Sub(last), "last_heartbeat"
	}
	st, err := os.Stat(ManifestPath(dir))
	if err != nil {
		return -1, "stat error"
	}
	return now.Sub(st.ModTime()), "manifest mtime"
}
