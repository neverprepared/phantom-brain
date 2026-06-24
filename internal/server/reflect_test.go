package server

import (
	"context"
	"errors"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// fakeScroller is an in-memory summaryScroller for unit-testing the
// reflect detectors without a live OpenSearch. It replays a fixed
// slice and filters on (profile, vault) like the real scroll does.
type fakeScroller struct {
	docs []osearch.SummaryDoc
	err  error
}

func (f *fakeScroller) ScrollSummaries(_ context.Context, profile, vault string, _ int, fn func(osearch.SummaryDoc) error) error {
	if f.err != nil {
		return f.err
	}
	for _, d := range f.docs {
		if d.Profile != profile || d.Vault != vault {
			continue
		}
		if err := fn(d); err != nil {
			return err
		}
	}
	return nil
}

func TestStaleGateCandidates_OnlyUnsynthesised(t *testing.T) {
	sc := &fakeScroller{docs: []osearch.SummaryDoc{
		{Profile: "p", Vault: "v", SHA: "aaa", Title: "stale one", Synthesised: false},
		{Profile: "p", Vault: "v", SHA: "bbb", Title: "enriched", Synthesised: true},
		{Profile: "p", Vault: "v", SHA: "ccc", Title: "stale two", Synthesised: false},
		// Different binding — must be excluded by the scroll filter.
		{Profile: "other", Vault: "v", SHA: "ddd", Title: "wrong tenant", Synthesised: false},
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
	sc := &fakeScroller{docs: []osearch.SummaryDoc{
		{Profile: "p", Vault: "v", SHA: "aaa", Synthesised: true},
		{Profile: "p", Vault: "v", SHA: "bbb", Synthesised: true},
	}}
	got, err := staleGateCandidates(context.Background(), sc, "p", "v")
	if err != nil {
		t.Fatalf("staleGateCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 candidates, got %d: %+v", len(got), got)
	}
}

func TestStaleGateCandidates_ScrollError(t *testing.T) {
	sentinel := errors.New("boom")
	sc := &fakeScroller{err: sentinel}
	_, err := staleGateCandidates(context.Background(), sc, "p", "v")
	if err == nil {
		t.Fatal("expected error from scroll failure")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error should wrap the scroll error, got %v", err)
	}
}
