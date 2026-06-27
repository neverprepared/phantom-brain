package brain

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestManifest_OmitemptyFieldsAbsentForGreenfield asserts the wire
// schema: a greenfield brain born without a daemon must not emit the
// optional parentage/heartbeat/nonce fields. Operators read these files
// during incidents, so spurious empty keys are noise — omitempty matters.
func TestManifest_OmitemptyFieldsAbsentForGreenfield(t *testing.T) {
	m := &Manifest{
		SchemaVersion:    ManifestSchemaVersion,
		BrainID:          "g1",
		ContributorID:    "personal/memory@host",
		Profile:          "personal",
		Vault:            "memory",
		BornAt:           "2026-01-01T00:00:00Z",
		Status:           StatusAlive,
		Host:             "host",
		LastCheckpointAt: "2026-01-01T00:00:00Z",
		SeedSource:       SeedGreenfield,
		// ParentSynthesisID, ContainerNonce, LastHeartbeat, StaleSeed left zero.
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	for _, absent := range []string{"parent_synthesis_id", "container_nonce", "last_heartbeat", "stale_seed"} {
		if strings.Contains(s, absent) {
			t.Errorf("omitempty field %q leaked into JSON: %s", absent, s)
		}
	}
	// Required (non-omitempty) fields must always be present, even at zero.
	for _, present := range []string{"schema_version", "born_at", "status", "last_checkpoint_at", "last_checkpoint_writes", "seed_source"} {
		if !strings.Contains(s, present) {
			t.Errorf("required field %q missing from JSON: %s", present, s)
		}
	}
}

// TestManifest_OptionalFieldsRoundTripWhenSet confirms the same fields
// serialize and read back when populated.
func TestManifest_OptionalFieldsRoundTripWhenSet(t *testing.T) {
	dir := t.TempDir()
	want := &Manifest{
		BrainID:           "g2",
		Profile:           "personal",
		Vault:             "memory",
		BornAt:            "2026-01-01T00:00:00Z",
		Status:            StatusAlive,
		Host:              "host",
		LastCheckpointAt:  "2026-01-01T00:00:00Z",
		SeedSource:        SeedTarball,
		ParentSynthesisID: "synth-42",
		ContainerNonce:    "nonce-7",
		LastHeartbeat:     "2026-01-01T01:00:00Z",
		StaleSeed:         true,
	}
	if err := WriteManifest(dir, want); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.ParentSynthesisID != "synth-42" || got.ContainerNonce != "nonce-7" {
		t.Errorf("optional identity fields lost: %+v", got)
	}
	if got.LastHeartbeat != "2026-01-01T01:00:00Z" || !got.StaleSeed {
		t.Errorf("heartbeat/stale_seed lost: %+v", got)
	}
	if got.SeedSource != SeedTarball {
		t.Errorf("seed_source = %q, want tarball", got.SeedSource)
	}
}

// TestWriteManifest_StampsSchemaAndTrailingNewline covers the
// zero-version stamping and the well-formed-for-shell-tools newline.
func TestWriteManifest_StampsSchemaAndTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{BrainID: "z", Status: StatusAlive, BornAt: "2026-01-01T00:00:00Z", LastCheckpointAt: "2026-01-01T00:00:00Z"}
	if m.SchemaVersion != 0 {
		t.Fatal("precondition: schema_version should start at 0")
	}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	// In-struct stamping mutates the caller's manifest.
	if m.SchemaVersion != ManifestSchemaVersion {
		t.Errorf("in-struct schema not stamped: %d", m.SchemaVersion)
	}
	raw, err := os.ReadFile(ManifestPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Error("manifest file should end with a trailing newline")
	}
}
