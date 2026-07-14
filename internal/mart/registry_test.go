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

func TestRegistry_ListSkipsCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	reg := OpenRegistry(dir)
	if err := reg.Save(testSpec()); err != nil {
		t.Fatal(err)
	}
	// credentials.toml lives in the same marts dir — it must NOT be loaded as a
	// bogus empty spec.
	if err := SaveCredentials(dir, Credentials{Bindings: []Credential{{Profile: "p", Vault: "v", API: "https://x", Token: "t"}}}); err != nil {
		t.Fatal(err)
	}
	specs, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "taxes" {
		t.Fatalf("List = %+v, want only the taxes spec (credentials.toml excluded)", specs)
	}
}

func TestRegistry_CursorRoundtrip(t *testing.T) {
	reg := OpenRegistry(t.TempDir())

	// Missing cursor → zero, no error.
	got, err := reg.LoadCursor("taxes")
	if err != nil || got != (Cursor{}) {
		t.Fatalf("missing cursor: got %+v err %v, want zero", got, err)
	}

	want := Cursor{Since: "2026-06-01T12:00:00Z", AfterID: 42}
	if err := reg.SaveCursor("taxes", want); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}
	got, err = reg.LoadCursor("taxes")
	if err != nil || got != want {
		t.Fatalf("roundtrip: got %+v err %v, want %+v", got, err, want)
	}

	if err := reg.RemoveCursor("taxes"); err != nil {
		t.Fatalf("RemoveCursor: %v", err)
	}
	if got, _ := reg.LoadCursor("taxes"); got != (Cursor{}) {
		t.Fatalf("after remove: got %+v, want zero", got)
	}
	// RemoveCursor is idempotent.
	if err := reg.RemoveCursor("taxes"); err != nil {
		t.Fatalf("RemoveCursor on missing: %v", err)
	}
}
