package vec

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// TestInitIdempotent verifies Init can be called repeatedly and only
// the first call has effect (sync.Once semantics). Important because
// internal/sqlite calls Init in its own package init; if a test or
// future caller calls Init directly we don't want a double-registration
// panic from database/sql.
func TestInitIdempotent(t *testing.T) {
	if err := Init(); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := Init(); err != nil {
			t.Fatalf("repeat Init #%d: %v", i, err)
		}
	}
}

// TestDriverRegistered confirms the driver name is registered after
// Init. database/sql.Drivers returns the sorted list of names.
func TestDriverRegistered(t *testing.T) {
	if err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	for _, name := range sql.Drivers() {
		if name == DriverName {
			return
		}
	}
	t.Errorf("driver %q not registered; have %v", DriverName, sql.Drivers())
}

// TestExtractedDylibIsStable confirms that two consecutive Init calls
// land on the SAME temp path (deterministic per content hash), so dev
// machines don't accumulate /tmp/pbrainctl-vec-*.dylib files across
// rebuilds with the same vendored dylib.
func TestExtractedDylibIsStable(t *testing.T) {
	if err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	a, err := extractDylib()
	if err != nil {
		t.Fatal(err)
	}
	b, err := extractDylib()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("extract returned %q then %q; want stable path", a, b)
	}
	if !strings.HasPrefix(filepath.Base(a), "pbrainctl-vec-") {
		t.Errorf("temp file name should be prefixed pbrainctl-vec-; got %q", filepath.Base(a))
	}
	st, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	if int(st.Size()) != len(dylibBytes) {
		t.Errorf("on-disk size %d != embedded size %d", st.Size(), len(dylibBytes))
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("dylib perm = %o, want 0600", perm)
	}
}

// TestVec0ModuleAvailable opens a database via the registered driver
// and confirms the vec0 virtual table module is wired in. This is the
// end-to-end canary: if every other test passes but this one fails,
// the ConnectHook isn't running or LoadExtension isn't finding the
// extension entry point.
func TestVec0ModuleAvailable(t *testing.T) {
	if err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	db, err := sql.Open(DriverName, "file:"+filepath.Join(t.TempDir(), "smoke.db")+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if _, err := db.Exec(`CREATE VIRTUAL TABLE v USING vec0(e float[4])`); err != nil {
		t.Fatalf("CREATE VIRTUAL TABLE: %v", err)
	}

	// vec_version() is a scalar function shipped with sqlite-vec; if
	// it resolves the load really happened, not just the module table.
	var version string
	if err := db.QueryRow(`SELECT vec_version()`).Scan(&version); err != nil {
		t.Fatalf("vec_version: %v", err)
	}
	if version == "" {
		t.Error("vec_version returned empty string")
	}
	t.Logf("loaded sqlite-vec %s", version)
}
