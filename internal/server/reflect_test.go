package server

import (
	"context"
	"errors"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// fakeUnsynthScanner is an in-memory unsynthScanner for unit-testing the
// reflect detector without a live Postgres. It mimics the SQL query
// (ListUnsynthesised) by filtering on (profile, vault) AND NOT
// synthesised, so the test exercises staleGateCandidates' row→candidate
// mapping rather than re-testing the SQL filter.
type fakeUnsynthScanner struct {
	recs []pgdb.Record
	err  error
}

func (f *fakeUnsynthScanner) ListUnsynthesised(_ context.Context, arg pgdb.ListUnsynthesisedParams) ([]pgdb.Record, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []pgdb.Record
	for _, r := range f.recs {
		if r.Profile != arg.Profile || r.Vault != arg.Vault {
			continue
		}
		if r.Synthesised {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func TestStaleGateCandidates_OnlyUnsynthesised(t *testing.T) {
	sc := &fakeUnsynthScanner{recs: []pgdb.Record{
		{Profile: "p", Vault: "v", Sha: "aaa", Title: "stale one", Synthesised: false},
		{Profile: "p", Vault: "v", Sha: "bbb", Title: "enriched", Synthesised: true},
		{Profile: "p", Vault: "v", Sha: "ccc", Title: "stale two", Synthesised: false},
		// Different binding — must be excluded by the scan filter.
		{Profile: "other", Vault: "v", Sha: "ddd", Title: "wrong tenant", Synthesised: false},
	}}

	got, err := staleGateCandidates(context.Background(), sc, "p", "v")
	if err != nil {
		t.Fatalf("staleGateCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d: %+v", len(got), got)
	}

	bySHA := map[string]ReflectCandidate{}
	for _, c := range got {
		bySHA[c.SHA] = c
	}
	for _, sha := range []string{"aaa", "ccc"} {
		c, ok := bySHA[sha]
		if !ok {
			t.Fatalf("expected candidate %s missing", sha)
		}
		if c.Reason != "stale-gate: never synthesised" {
			t.Errorf("%s: reason = %q, want stale-gate reason", sha, c.Reason)
		}
	}
	if _, ok := bySHA["bbb"]; ok {
		t.Error("synthesised doc bbb should not be a candidate")
	}
	if _, ok := bySHA["ddd"]; ok {
		t.Error("other-tenant doc ddd should not be a candidate")
	}
}

func TestStaleGateCandidates_NoneWhenAllSynthesised(t *testing.T) {
	sc := &fakeUnsynthScanner{recs: []pgdb.Record{
		{Profile: "p", Vault: "v", Sha: "aaa", Synthesised: true},
		{Profile: "p", Vault: "v", Sha: "bbb", Synthesised: true},
	}}
	got, err := staleGateCandidates(context.Background(), sc, "p", "v")
	if err != nil {
		t.Fatalf("staleGateCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 candidates, got %d: %+v", len(got), got)
	}
}

func TestStaleGateCandidates_ScanError(t *testing.T) {
	sentinel := errors.New("boom")
	sc := &fakeUnsynthScanner{err: sentinel}
	_, err := staleGateCandidates(context.Background(), sc, "p", "v")
	if err == nil {
		t.Fatal("expected error from scan failure")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error should wrap the scan error, got %v", err)
	}
}
