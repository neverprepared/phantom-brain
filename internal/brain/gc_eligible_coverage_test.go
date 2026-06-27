package brain

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIsGCEligible_DirectBranches drives IsGCEligible without going
// through the full Recover sweep, covering the predicate's decision
// table including the "age unknown" path (empty heartbeat + missing
// manifest file → manifestAge returns -1).
func TestIsGCEligible_DirectBranches(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	retention := 24 * time.Hour

	t.Run("retention disabled", func(t *testing.T) {
		ok, reason := IsGCEligible(&Manifest{Status: StatusDead}, "/nope", now, 0)
		if ok || reason != "gc disabled" {
			t.Errorf("got (%v, %q)", ok, reason)
		}
	})

	t.Run("nil manifest", func(t *testing.T) {
		ok, reason := IsGCEligible(nil, "/nope", now, retention)
		if ok || reason != "nil manifest" {
			t.Errorf("got (%v, %q)", ok, reason)
		}
	})

	t.Run("non-dead status kept", func(t *testing.T) {
		ok, reason := IsGCEligible(&Manifest{Status: StatusAlive}, "/nope", now, retention)
		if ok || !strings.HasPrefix(reason, "status=") {
			t.Errorf("got (%v, %q)", ok, reason)
		}
	})

	t.Run("age unknown when heartbeat empty and manifest absent", func(t *testing.T) {
		// dir has no manifest file on disk → os.Stat fails → age=-1.
		dir := filepath.Join(t.TempDir(), "ghost")
		ok, reason := IsGCEligible(&Manifest{Status: StatusDead}, dir, now, retention)
		if ok || reason != "age unknown" {
			t.Errorf("got (%v, %q), want (false, \"age unknown\")", ok, reason)
		}
	})

	t.Run("eligible when heartbeat older than retention", func(t *testing.T) {
		m := &Manifest{Status: StatusDead, LastHeartbeat: now.Add(-48 * time.Hour).Format(time.RFC3339)}
		ok, reason := IsGCEligible(m, "/unused", now, retention)
		if !ok || !strings.HasPrefix(reason, "eligible") {
			t.Errorf("got (%v, %q)", ok, reason)
		}
		if !strings.Contains(reason, "last_heartbeat") {
			t.Errorf("reason should name the timestamp source: %q", reason)
		}
	})

	t.Run("too young when heartbeat within retention", func(t *testing.T) {
		m := &Manifest{Status: StatusDead, LastHeartbeat: now.Add(-1 * time.Hour).Format(time.RFC3339)}
		ok, reason := IsGCEligible(m, "/unused", now, retention)
		if ok || !strings.Contains(reason, "too young") {
			t.Errorf("got (%v, %q)", ok, reason)
		}
	})
}
