// Package vec wires the sqlite-vec extension into a custom mattn/go-sqlite3
// driver so the rest of pbrainctl can use a normal database/sql handle
// and get the vec0 virtual-table module on every connection.
//
// # Why this exists
//
// Three earlier attempts to load sqlite-vec are documented in
// internal/sqlite/sqlite.go. All failed on macOS for the same root cause:
// Apple stubbed sqlite3_auto_extension in macOS 10.10, so any approach
// that uses the global auto-extension registry silently registers
// nothing. This package sidesteps it by loading the extension
// per-connection via SQLite's documented sqlite3_load_extension API,
// which works identically across macOS, Linux, and the BSDs.
//
// # How
//
// At package init() we extract the embedded sqlite-vec dylib for the
// host platform to a temp file, then register a custom database/sql
// driver name ("sqlite3_vec") whose ConnectHook calls LoadExtension
// against that path with the standard sqlite3_vec_init entry point.
// internal/sqlite.Open() uses this driver name; every *sql.DB it
// returns has the vec0 module available without any per-call setup.
//
// # Single-binary distribution
//
// The dylib is embedded into pbrainctl at compile time via go:embed
// (per-platform via build tags — see embed_*.go). At runtime we extract
// it to os.TempDir() under a deterministic-per-process filename. The
// temp file is created with 0600 permissions and is not registered for
// cleanup — long-lived processes (the MCP server, the daemon) hold
// the file open for their entire lifetime, and the OS reclaims it on
// reboot. Test processes use t.TempDir which the test runner cleans up.
//
// # Why a temp file at all
//
// sqlite3_load_extension takes a filesystem path; there is no in-memory
// extension load API in SQLite proper. We could fork SQLite to add one,
// but the temp-file cost is one ~160 KB write at process startup —
// well under the noise floor of any operation pbrainctl performs.
package vec

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// DriverName is the database/sql driver name that loads sqlite-vec on
// every new connection. Imported by internal/sqlite as the driver to
// open against.
const DriverName = "sqlite3_vec"

// dylibBytes is populated by the per-platform embed_*.go file via
// go:embed at compile time. An unsupported GOOS/GOARCH will compile
// the embed_unsupported.go variant whose dylibBytes is nil; Init then
// returns ErrUnsupportedPlatform.
var dylibBytes []byte

// dylibExt is the platform-appropriate filename suffix for the
// extracted extension (".dylib" on darwin, ".so" on linux, etc.).
var dylibExt string

// ErrUnsupportedPlatform is returned by Init when no vendored binary
// exists for the host's GOOS/GOARCH. Add the platform's dylib to
// dylibs/ and the matching embed_*.go file to lift this.
var ErrUnsupportedPlatform = errors.New("vec: no sqlite-vec binary is vendored for this platform — see internal/vec/dylibs/")

var (
	initOnce sync.Once
	initErr  error
)

// Init extracts the embedded sqlite-vec library to a temp file and
// registers the database/sql driver. Safe to call multiple times;
// subsequent calls return the result of the first.
//
// Callers that just want "use the vec-enabled driver" don't need to
// call this — internal/sqlite.Open does it implicitly via its blank
// import of this package. Init is exported for tests and for code
// paths that want to fail fast at startup rather than at first Open.
func Init() error {
	initOnce.Do(doInit)
	return initErr
}

func doInit() {
	if len(dylibBytes) == 0 {
		initErr = ErrUnsupportedPlatform
		return
	}

	path, err := extractDylib(os.TempDir())
	if err != nil {
		initErr = fmt.Errorf("vec: extract dylib: %w", err)
		return
	}

	sql.Register(DriverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			// sqlite3_vec_init is the standard sqlite-vec entry point.
			// Loading it registers the vec0 virtual-table module on
			// this specific connection.
			if err := conn.LoadExtension(path, "sqlite3_vec_init"); err != nil {
				return fmt.Errorf("vec: load extension: %w", err)
			}
			return nil
		},
	})
}

// extractDylib writes dylibBytes to a deterministic-per-process file
// under dir and returns its path. Deterministic means: if the process
// imports this package multiple times across the same binary (it
// shouldn't, but just in case), the second call reuses the same file
// rather than spamming the dir. Production passes os.TempDir(); tests
// pass a t.TempDir() so they never clobber the shared on-disk dylib
// that other packages load concurrently under `go test ./...`.
func extractDylib(dir string) (string, error) {
	sum := sha256.Sum256(dylibBytes)
	name := fmt.Sprintf("pbrainctl-vec-%s%s", hex.EncodeToString(sum[:8]), dylibExt)
	path := filepath.Join(dir, name)

	// If the file already exists with matching size, trust it. This
	// makes Init() idempotent across process restarts during dev
	// (incremental compiles keep the same sha8).
	if st, err := os.Stat(path); err == nil && st.Size() == int64(len(dylibBytes)) {
		return path, nil
	}

	// Otherwise write fresh. Use O_EXCL on a tmp file then rename so a
	// concurrent process can't see a half-written dylib.
	tmp, err := os.CreateTemp(dir, "pbrainctl-vec-*.tmp")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(dylibBytes); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return path, nil
}
