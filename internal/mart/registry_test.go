package mart

import (
	"os"
	"path/filepath"
	"testing"
)

func testSpec() Spec {
	return Spec{
		Name:      "taxes",
		Profile:   "personal",
		Vault:     "default",
		Dest:      "/tmp/does-not-matter/_mart",
		Ephemeral: true,
		Filters:   Filters{Tags: []string{"tax", "irs"}, Topic: "memory"},
	}
}

func TestRegistry_SaveLoadListRemove(t *testing.T) {
	dir := t.TempDir()
	reg := OpenRegistry(dir)

	// Empty registry: List is empty, not an error.
	specs, err := reg.List()
	if err != nil {
		t.Fatalf("List on empty: %v", err)
	}
	if len(specs) != 0 {
		t.Fatalf("expected 0 specs, got %d", len(specs))
	}

	in := testSpec()
	if err := reg.Save(in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "marts", "taxes.toml")); err != nil {
		t.Fatalf("spec file not written: %v", err)
	}

	got, err := reg.Load("taxes")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Name != in.Name || got.Profile != in.Profile || got.Vault != in.Vault ||
		got.Dest != in.Dest || got.Ephemeral != in.Ephemeral || got.Filters.Topic != in.Filters.Topic ||
		len(got.Filters.Tags) != 2 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	specs, err = reg.List()
	if err != nil || len(specs) != 1 {
		t.Fatalf("List: err=%v n=%d", err, len(specs))
	}

	if err := reg.Remove("taxes"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := reg.Load("taxes"); err == nil {
		t.Fatal("Load after Remove should error")
	}
}

func TestRegistry_SaveRejectsInvalidSpec(t *testing.T) {
	reg := OpenRegistry(t.TempDir())
	bad := testSpec()
	bad.Name = "Not Valid Name"
	if err := reg.Save(bad); err == nil {
		t.Fatal("Save should reject an invalid mart name")
	}
	bad = testSpec()
	bad.Dest = "relative/path"
	if err := reg.Save(bad); err == nil {
		t.Fatal("Save should reject a non-absolute dest")
	}
}

func TestRegistry_LoadMissing(t *testing.T) {
	reg := OpenRegistry(t.TempDir())
	if _, err := reg.Load("ghost"); err == nil {
		t.Fatal("Load of a missing mart should error")
	}
}
