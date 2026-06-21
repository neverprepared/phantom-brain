package config

import (
	"strings"
	"testing"
)

// mockEnv is the test substitute for os.Getenv. Keys not present in the
// map return "" — same semantics as Getenv for unset vars.
type mockEnv map[string]string

func (m mockEnv) lookup(k string) string { return m[k] }

// baseRequired is the minimal valid env: all four required vars set.
func baseRequired() mockEnv {
	return mockEnv{
		"CL_BRAIN_API":         "https://brain.example.com",
		"CL_BRAIN_API_TOKEN":   "pb_token_abc",
		"CL_WORKSPACE_PROFILE": "personal",
		"CL_BRAIN_VAULT":       "memory",
		"HOME":                 "/home/test",
	}
}

func TestLoadAgentRequiresAllFour(t *testing.T) {
	cases := []struct {
		drop string
	}{
		{"CL_BRAIN_API"},
		{"CL_BRAIN_API_TOKEN"},
		{"CL_WORKSPACE_PROFILE"},
		{"CL_BRAIN_VAULT"},
	}
	for _, c := range cases {
		t.Run("missing-"+c.drop, func(t *testing.T) {
			env := baseRequired()
			delete(env, c.drop)
			_, err := loadAgentFrom(env.lookup)
			if err == nil {
				t.Fatalf("expected error when %s missing, got nil", c.drop)
			}
			if !strings.Contains(err.Error(), c.drop) {
				t.Errorf("error should name the missing var %q: %v", c.drop, err)
			}
		})
	}
}

func TestLoadAgentReportsAllMissingAtOnce(t *testing.T) {
	// Operators should see every missing var in one shot, not have to
	// fix-and-retry four times.
	env := mockEnv{"HOME": "/home/test"}
	_, err := loadAgentFrom(env.lookup)
	if err == nil {
		t.Fatal("expected error")
	}
	want := []string{"CL_BRAIN_API", "CL_BRAIN_API_TOKEN", "CL_WORKSPACE_PROFILE", "CL_BRAIN_VAULT"}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error should mention %q; got: %v", w, err)
		}
	}
}

func TestLoadAgentTunableDefaults(t *testing.T) {
	env := baseRequired()
	a, err := loadAgentFrom(env.lookup)
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"CheckpointWrites", a.CheckpointWrites, 50},
		{"CheckpointMinIntervalSecs", a.CheckpointMinIntervalSecs, 300},
		{"CheckpointIdleHours", a.CheckpointIdleHours, 6},
		{"CheckpointMaxAgeDays", a.CheckpointMaxAgeDays, 7},
		{"HeartbeatIntervalSecs", a.HeartbeatIntervalSecs, 30},
		{"OrphanThresholdSecs", a.OrphanThresholdSecs, 300},
		{"MaxPendingMB", a.MaxPendingMB, 5000},
		{"DiskPreflightCeilingBytes", a.DiskPreflightCeilingBytes, int64(10 * 1024 * 1024 * 1024)},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadAgentTunableOverrides(t *testing.T) {
	env := baseRequired()
	env["CL_BRAIN_CHECKPOINT_WRITES"] = "100"
	env["CL_BRAIN_HEARTBEAT_INTERVAL_SECS"] = "60"
	env["CL_BRAIN_ORPHAN_THRESHOLD_SECS"] = "900" // must satisfy 10x heartbeat rule
	env["CL_BRAIN_MAX_PENDING_MB"] = "8000"
	env["CL_BRAIN_DISK_PREFLIGHT_CEILING_BYTES"] = "21474836480"

	a, err := loadAgentFrom(env.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if a.CheckpointWrites != 100 {
		t.Errorf("CheckpointWrites = %d, want 100", a.CheckpointWrites)
	}
	if a.HeartbeatIntervalSecs != 60 {
		t.Errorf("HeartbeatIntervalSecs = %d, want 60", a.HeartbeatIntervalSecs)
	}
	if a.MaxPendingMB != 8000 {
		t.Errorf("MaxPendingMB = %d, want 8000", a.MaxPendingMB)
	}
	if a.DiskPreflightCeilingBytes != 21474836480 {
		t.Errorf("DiskPreflightCeilingBytes = %d, want 21474836480", a.DiskPreflightCeilingBytes)
	}
}

func TestLoadAgentTunableBogusFallsBack(t *testing.T) {
	// Garbage values should not crash the process; they should fall
	// back to the documented default and continue.
	env := baseRequired()
	env["CL_BRAIN_CHECKPOINT_WRITES"] = "lol"
	env["CL_BRAIN_HEARTBEAT_INTERVAL_SECS"] = "-7"  // negative — invalid
	env["CL_BRAIN_MAX_PENDING_MB"] = "0"            // zero — invalid

	a, err := loadAgentFrom(env.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if a.CheckpointWrites != 50 {
		t.Errorf("CheckpointWrites = %d, want default 50", a.CheckpointWrites)
	}
	if a.HeartbeatIntervalSecs != 30 {
		t.Errorf("HeartbeatIntervalSecs = %d, want default 30", a.HeartbeatIntervalSecs)
	}
	if a.MaxPendingMB != 5000 {
		t.Errorf("MaxPendingMB = %d, want default 5000", a.MaxPendingMB)
	}
}

func TestOrphanThresholdEnforces10xHeartbeat(t *testing.T) {
	// v5.0 invariant: orphan threshold >= 10x heartbeat interval, so a
	// late touch doesn't false-orphan a live brain. The validator must
	// reject configurations that would silently break orphan detection.
	env := baseRequired()
	env["CL_BRAIN_HEARTBEAT_INTERVAL_SECS"] = "60"
	env["CL_BRAIN_ORPHAN_THRESHOLD_SECS"] = "300" // only 5x heartbeat — should reject

	_, err := loadAgentFrom(env.lookup)
	if err == nil {
		t.Fatal("expected error when orphan threshold < 10x heartbeat")
	}
	if !strings.Contains(err.Error(), "10 *") {
		t.Errorf("error should explain the 10x rule: %v", err)
	}
}

func TestOptionalFieldsCleanWhenUnset(t *testing.T) {
	env := baseRequired()
	a, err := loadAgentFrom(env.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if a.CollectivePath != "" {
		t.Errorf("CollectivePath = %q, want empty", a.CollectivePath)
	}
	if a.BrainID != "" {
		t.Errorf("BrainID = %q, want empty", a.BrainID)
	}
}

func TestOptionalFieldsSetWhenProvided(t *testing.T) {
	env := baseRequired()
	env["CL_BRAIN_COLLECTIVE_PATH"] = "/mnt/juicefs/personal/memory/collective"
	env["CL_BRAIN_ID"] = "abc-123"

	a, err := loadAgentFrom(env.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if a.CollectivePath != "/mnt/juicefs/personal/memory/collective" {
		t.Errorf("CollectivePath = %q", a.CollectivePath)
	}
	if a.BrainID != "abc-123" {
		t.Errorf("BrainID = %q", a.BrainID)
	}
}

func TestWhitespaceInRequiredFieldsTreatedAsMissing(t *testing.T) {
	env := baseRequired()
	env["CL_BRAIN_API"] = "   "

	_, err := loadAgentFrom(env.lookup)
	if err == nil || !strings.Contains(err.Error(), "CL_BRAIN_API") {
		t.Errorf("whitespace-only required field should be treated as missing; got: %v", err)
	}
}

// --- paths ---

func TestPathsUseXDGDataHome(t *testing.T) {
	env := baseRequired()
	env["XDG_DATA_HOME"] = "/var/data"
	a, err := loadAgentFrom(env.lookup)
	if err != nil {
		t.Fatal(err)
	}

	want := "/var/data/phantom-brain/personal/memory"
	if a.VaultBaseDir() != want {
		t.Errorf("VaultBaseDir = %q, want %q", a.VaultBaseDir(), want)
	}
	if a.BrainsRoot() != want+"/brains" {
		t.Errorf("BrainsRoot = %q", a.BrainsRoot())
	}
	if a.BrainDir("xyz") != want+"/brains/xyz" {
		t.Errorf("BrainDir(xyz) = %q", a.BrainDir("xyz"))
	}
	if a.SnapshotCacheDir() != want+"/_snapshot-cache" {
		t.Errorf("SnapshotCacheDir = %q", a.SnapshotCacheDir())
	}
	if a.ShipPendingDir() != want+"/_pending" {
		t.Errorf("ShipPendingDir = %q", a.ShipPendingDir())
	}
}

func TestPathsFallBackToHomeDotLocalShare(t *testing.T) {
	env := baseRequired() // HOME=/home/test, no XDG_DATA_HOME
	a, err := loadAgentFrom(env.lookup)
	if err != nil {
		t.Fatal(err)
	}
	want := "/home/test/.local/share/phantom-brain/personal/memory"
	if a.VaultBaseDir() != want {
		t.Errorf("VaultBaseDir = %q, want %q", a.VaultBaseDir(), want)
	}
}

func TestPathsNoHomeAndNoXDGIsError(t *testing.T) {
	env := mockEnv{
		"CL_BRAIN_API":         "https://brain.example.com",
		"CL_BRAIN_API_TOKEN":   "pb_token",
		"CL_WORKSPACE_PROFILE": "personal",
		"CL_BRAIN_VAULT":       "memory",
		// No HOME, no XDG_DATA_HOME.
	}
	_, err := loadAgentFrom(env.lookup)
	if err == nil {
		t.Fatal("expected error when neither HOME nor XDG_DATA_HOME is set")
	}
}

func TestBrainDirEmptyReturnsBrainsRoot(t *testing.T) {
	env := baseRequired()
	a, _ := loadAgentFrom(env.lookup)
	if a.BrainDir("") != a.BrainsRoot() {
		t.Errorf("BrainDir(\"\") should equal BrainsRoot; got %q vs %q", a.BrainDir(""), a.BrainsRoot())
	}
}
