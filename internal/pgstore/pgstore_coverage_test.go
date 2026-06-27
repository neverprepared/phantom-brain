package pgstore

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// fakeDBTX is a no-op pgdb.DBTX used to prove New wraps an arbitrary
// connection-or-pool handle without touching the network. The query methods
// are never invoked by these tests — they exist only to satisfy the interface.
type fakeDBTX struct{}

func (fakeDBTX) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (fakeDBTX) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, nil
}
func (fakeDBTX) QueryRow(context.Context, string, ...interface{}) pgx.Row { return nil }

// toPgxURL's default branch: any scheme other than postgres/postgresql/pgx5
// is returned verbatim (NewWithSourceInstance is then expected to surface the
// error downstream). The existing TestToPgxURL only exercises the three
// rewrite branches, never this passthrough.
func TestToPgxURL_DefaultPassthrough(t *testing.T) {
	cases := []string{
		"",                       // empty input
		"mysql://u:p@h/db",       // unrelated scheme
		"sqlite:///tmp/x.db",     // unrelated scheme
		"u:p@h/db",               // no scheme at all
		"PGX5://u@h/db",          // case-sensitive: not the lowercase prefix
		"postgres-ish://u@h/db",  // prefix-adjacent but not an exact match
	}
	for _, in := range cases {
		if got := toPgxURL(in); got != in {
			t.Errorf("toPgxURL(%q) = %q, want unchanged passthrough", in, got)
		}
	}
}

// toPgxURL must only rewrite the scheme portion, leaving userinfo/host/path/
// query bytes (including an embedded "postgres" substring later in the URL)
// untouched. Guards against a naive ReplaceAll-style implementation.
func TestToPgxURL_RewritesSchemeOnly(t *testing.T) {
	in := "postgres://u:p@h:5432/postgres?application_name=postgres"
	want := "pgx5://u:p@h:5432/postgres?application_name=postgres"
	if got := toPgxURL(in); got != want {
		t.Fatalf("toPgxURL(%q) = %q, want %q", in, got, want)
	}
}

// DSNForProfile must propagate a url.Parse failure (distinct from the invalid-
// profile path already covered in TestDSNForProfile). A control character in
// the host makes net/url reject the input, so the rewrite never runs.
func TestDSNForProfile_ParseError(t *testing.T) {
	// Valid profile (so we get past ProfileDBName) but an unparseable DSN:
	// the 0x7f control byte is rejected by net/url.
	_, err := DSNForProfile("postgres://h\x7f/db", "gsa")
	if err == nil {
		t.Fatal("DSNForProfile with unparseable DSN: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "parse DSN") {
		t.Errorf("error %q does not mention DSN parse failure", err)
	}
}

// DSNForProfile validates the profile BEFORE attempting to parse baseDSN, so
// an invalid profile short-circuits even when baseDSN is itself garbage. This
// pins the ordering: the returned error is the profile-validation error, not a
// DSN-parse error.
func TestDSNForProfile_ProfileCheckedBeforeParse(t *testing.T) {
	_, err := DSNForProfile("\x7f-not-a-url", "bad-profile")
	if err == nil {
		t.Fatal("expected error for invalid profile, got nil")
	}
	if !strings.Contains(err.Error(), "invalid profile") {
		t.Errorf("expected profile-validation error, got %q", err)
	}
}

// DSNForProfile with a base DSN that has no path still produces a rooted
// per-profile path. url.Parse of "postgres://host" yields an empty Path; we
// overwrite it with "/pb_<profile>".
func TestDSNForProfile_NoBasePath(t *testing.T) {
	got, err := DSNForProfile("postgres://h:5432", "personal")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "postgres://h:5432/pb_personal"
	if got != want {
		t.Fatalf("DSNForProfile = %q, want %q", got, want)
	}
}

// New wraps a DBTX in the generated sqlc query set. It must return a usable,
// non-nil *pgdb.Queries, and each call must produce an independent wrapper (no
// shared mutable state across handles). This is a pure constructor test — no
// Postgres required, since New never touches the DBTX it stores.
func TestNew_WrapsDBTX(t *testing.T) {
	q1 := New(fakeDBTX{})
	if q1 == nil {
		t.Fatal("New(fakeDBTX{}) returned nil")
	}
	q2 := New(fakeDBTX{})
	if q2 == nil {
		t.Fatal("second New returned nil")
	}
	if q1 == q2 {
		t.Error("New returned the same *pgdb.Queries pointer for two calls; want independent wrappers")
	}
	// Sanity: the returned value is the concrete generated type, confirming
	// the store.go constructor delegates to pgdb.New.
	var _ *pgdb.Queries = q1
}

// Open must surface a pgxpool.ParseConfig failure as a wrapped error WITHOUT
// touching the network. An out-of-range/non-integer pool tunable is rejected
// purely by config parsing, so this exercises Open's first error branch with
// no live Postgres. (The success path needs a real server and lives in the
// integration suite.)
func TestOpen_ParseConfigError(t *testing.T) {
	pool, err := Open(context.Background(), "postgres://h/db?pool_max_conns=not-an-int")
	if err == nil {
		if pool != nil {
			pool.Close()
		}
		t.Fatal("Open with bad pool config: expected error, got nil")
	}
	if pool != nil {
		t.Errorf("Open returned a non-nil pool alongside an error: %v", pool)
	}
	if !strings.Contains(err.Error(), "parse pool DSN") {
		t.Errorf("error %q does not mention pool DSN parse failure", err)
	}
}
