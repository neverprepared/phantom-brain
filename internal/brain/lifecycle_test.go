package brain

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/neverprepared/mcp-phantom-brain/internal/config"
)

// --- Death ------------------------------------------------------------

// birthForTest is a tiny helper so death/checkpoint tests don't repeat
// the Birth boilerplate. Returns (agent, brainDir, logBuf).
func birthForTest(t *testing.T) (*config.Agent, string, *bytes.Buffer) {
	t.Helper()
	agent := agentForTest(t)
	buf := &bytes.Buffer{}
	dir, _, err := Birth(BirthOpts{
		Agent:    agent,
		Platform: newFakePlatform(),
		Logger:   captureLogger(buf),
	})
	if err != nil {
		t.Fatalf("Birth: %v", err)
	}
	return agent, dir, buf
}

// Phase 6: Death no longer packs a tarball — writes ship to the
// daemon as they happen. The remaining contract is "alive → dead
// with a log marker".
func TestDeath_FlipsStatusAndLogs(t *testing.T) {
	agent, dir, _ := birthForTest(t)

	buf := &bytes.Buffer{}
	res, err := Death(DeathOpts{
		Agent:    agent,
		BrainDir: dir,
		Logger:   captureLogger(buf),
	})
	if err != nil {
		t.Fatalf("Death: %v", err)
	}
	if res.PayloadSize != 0 || res.PayloadPath != "" {
		t.Errorf("Phase 6 Death should produce no payload; got %+v", res)
	}
	m, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != StatusDead {
		t.Errorf("status=%q, want dead", m.Status)
	}
	if !strings.Contains(buf.String(), "brain died") {
		t.Errorf("expected death log marker, got: %s", buf.String())
	}
}

func TestDeath_RejectsNonAliveStatus(t *testing.T) {
	agent, dir, _ := birthForTest(t)
	m, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.Status = StatusShuttingDown
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	_, err = Death(DeathOpts{
		Agent:    agent,
		BrainDir: dir,
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err == nil || !strings.Contains(err.Error(), "shutting_down") {
		t.Fatalf("expected shutting_down rejection, got %v", err)
	}
}

// --- Checkpoint -------------------------------------------------------

func TestCheckpoint_SkippedWhenThresholdsNotMet(t *testing.T) {
	agent, dir, _ := birthForTest(t)
	res, err := Checkpoint(CheckpointOpts{
		Agent:      agent,
		BrainDir:   dir,
		WriteCount: 0, // below threshold and just-born so no idle gap
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !res.Skipped {
		t.Errorf("expected skip, got %+v", res)
	}
}

func TestCheckpoint_ForceBypassesThresholds(t *testing.T) {
	agent, dir, _ := birthForTest(t)
	buf := &bytes.Buffer{}
	res, err := Checkpoint(CheckpointOpts{
		Agent:    agent,
		BrainDir: dir,
		Force:    true,
		Logger:   captureLogger(buf),
	})
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if res.Skipped {
		t.Error("force=true should not skip")
	}
	if res.CheckpointDir == "" || !strings.Contains(res.CheckpointDir, "_checkpoints") {
		t.Errorf("checkpoint dir wrong: %q", res.CheckpointDir)
	}
	st, err := os.Stat(res.CheckpointDir)
	if err != nil || !st.IsDir() {
		t.Errorf("checkpoint dir missing on disk: %v", err)
	}
	if !strings.Contains(buf.String(), "daemon publish is no-op") {
		t.Errorf("expected daemon-stub warning, got: %s", buf.String())
	}
}

func TestCheckpoint_AdvancesManifestState(t *testing.T) {
	agent, dir, _ := birthForTest(t)
	before, _ := ReadManifest(dir)

	_, err := Checkpoint(CheckpointOpts{
		Agent:      agent,
		BrainDir:   dir,
		WriteCount: 999,
		Force:      true,
		Logger:     slog.New(slog.DiscardHandler),
		Now:        func() time.Time { return time.Unix(2_000_000_000, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	after, _ := ReadManifest(dir)
	if after.LastCheckpointWrites != 999 {
		t.Errorf("LastCheckpointWrites=%d, want 999", after.LastCheckpointWrites)
	}
	if after.LastCheckpointAt == before.LastCheckpointAt {
		t.Errorf("LastCheckpointAt not advanced")
	}
}

func TestShouldCheckpoint_AgeMaxGapForces(t *testing.T) {
	agent, dir, _ := birthForTest(t)
	m, _ := ReadManifest(dir)
	cfg := agent
	now := time.Now().Add(time.Duration(cfg.CheckpointMaxAgeDays+1) * 24 * time.Hour)
	ok, _ := ShouldCheckpoint(m, cfg, 0, now)
	if !ok {
		t.Error("expected checkpoint after max-age gap")
	}
}

// --- Lifecycle --------------------------------------------------------

func TestLifecycle_StartShutdown_HappyPath(t *testing.T) {
	agent := agentForTest(t)
	buf := &bytes.Buffer{}
	lc, err := Start(StartOpts{Agent: agent, Platform: newFakePlatform(), Logger: captureLogger(buf)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if lc.BrainDir() == "" || lc.VaultDir() == "" {
		t.Fatal("BrainDir/VaultDir not exposed")
	}
	snap := lc.Snapshot()
	if snap.Status != StatusAlive {
		t.Errorf("snapshot status=%q", snap.Status)
	}
	res, err := lc.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if res.BrainID != snap.BrainID {
		t.Errorf("Shutdown BrainID=%q, want %q", res.BrainID, snap.BrainID)
	}
}

func TestLifecycle_ShutdownIsIdempotent(t *testing.T) {
	agent := agentForTest(t)
	lc, err := Start(StartOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := lc.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	_, err = lc.Shutdown(context.Background())
	if !IsAlreadyShutDown(err) {
		t.Errorf("second Shutdown should return errAlreadyShutDown, got %v", err)
	}
}

func TestLifecycle_CheckpointAfterShutdownRejected(t *testing.T) {
	agent := agentForTest(t)
	lc, _ := Start(StartOpts{Agent: agent, Platform: newFakePlatform(), Logger: slog.New(slog.DiscardHandler)})
	_, _ = lc.Shutdown(context.Background())
	_, err := lc.Checkpoint(0, true)
	if err == nil || !strings.Contains(err.Error(), "shut down") {
		t.Fatalf("expected shut-down rejection, got %v", err)
	}
}

// --- helpers ----------------------------------------------------------

// tarContains is a small helper that returns true if the tar at path
// has an entry with the given name.
func tarContains(t *testing.T, path, want string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return false
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		if hdr.Name == want {
			return true
		}
	}
}
