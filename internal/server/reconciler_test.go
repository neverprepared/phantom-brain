package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

func setOf(shas ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(shas))
	for _, s := range shas {
		m[s] = struct{}{}
	}
	return m
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func TestDiffProjection(t *testing.T) {
	cases := []struct {
		name        string
		sor, os     map[string]struct{}
		wantMissing []string
		wantOrphan  []string
	}{
		{
			name:        "all in sync",
			sor:         setOf("a", "b", "c"),
			os:          setOf("a", "b", "c"),
			wantMissing: nil,
			wantOrphan:  nil,
		},
		{
			name:        "missing from projection",
			sor:         setOf("a", "b", "c"),
			os:          setOf("a"),
			wantMissing: []string{"b", "c"},
			wantOrphan:  nil,
		},
		{
			name:        "orphan in projection",
			sor:         setOf("a"),
			os:          setOf("a", "x", "y"),
			wantMissing: nil,
			wantOrphan:  []string{"x", "y"},
		},
		{
			name:        "both directions",
			sor:         setOf("a", "b"),
			os:          setOf("b", "z"),
			wantMissing: []string{"a"},
			wantOrphan:  []string{"z"},
		},
		{
			name:        "empty both",
			sor:         setOf(),
			os:          setOf(),
			wantMissing: nil,
			wantOrphan:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			missing, orphan := diffProjection(tc.sor, tc.os)
			if got := sortedCopy(missing); !equalStrings(got, tc.wantMissing) {
				t.Errorf("missing = %v, want %v", got, tc.wantMissing)
			}
			if got := sortedCopy(orphan); !equalStrings(got, tc.wantOrphan) {
				t.Errorf("orphan = %v, want %v", got, tc.wantOrphan)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fakeSoR is an in-memory reconcileSoR. records holds the full SoR keyed by
// sha; vanished names SHAs that ListSHAs reports but Fetch returns (nil, nil)
// for — the mid-cycle brain_forget skip path.
type fakeSoR struct {
	records  map[string]pgdb.Record // sha -> record
	order    []string               // sha order for deterministic keyset paging
	vanished map[string]struct{}
}

func (s *fakeSoR) ListSHAs(_ context.Context, _, _ string, after int64, limit int) ([]recordSHA, error) {
	// Emulate id > after keyset paging: ids are 1-based index into order.
	var out []recordSHA
	for i, sha := range s.order {
		id := int64(i + 1)
		if id <= after {
			continue
		}
		out = append(out, recordSHA{ID: id, SHA: sha})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *fakeSoR) Fetch(_ context.Context, _, _, sha string) (*pgdb.Record, error) {
	if _, ok := s.vanished[sha]; ok {
		return nil, nil // vanished mid-cycle
	}
	rec, ok := s.records[sha]
	if !ok {
		return nil, nil
	}
	return &rec, nil
}

// fakeProjection is an in-memory reconcileProjection. docs is the current
// projection SHA set; projected / deleted record the repairs applied.
type fakeProjection struct {
	docs      map[string]struct{}
	projected []string
	deleted   []string
}

func (p *fakeProjection) ScrollSHAs(_ context.Context, _, _ string, fn func(sha string) error) error {
	for sha := range p.docs {
		if err := fn(sha); err != nil {
			return err
		}
	}
	return nil
}

func (p *fakeProjection) Project(_ context.Context, rec pgdb.Record) error {
	p.projected = append(p.projected, rec.Sha)
	p.docs[rec.Sha] = struct{}{}
	return nil
}

func (p *fakeProjection) DeleteProjection(_ context.Context, _, _, sha string) error {
	p.deleted = append(p.deleted, sha)
	delete(p.docs, sha)
	return nil
}

func quietReconciler(t *testing.T) *Reconciler {
	t.Helper()
	return NewReconciler(slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
}

// TestReconcileBinding_RepairsBothDirections proves reconcileBinding
// re-projects a missing record, deletes an orphan doc, leaves a healthy record
// untouched, and SKIPS a record that vanished mid-cycle.
func TestReconcileBinding_RepairsBothDirections(t *testing.T) {
	rec := func(sha string) pgdb.Record {
		return pgdb.Record{Profile: "p", Vault: "v", Sha: sha, Kind: "note", Title: sha}
	}
	sor := &fakeSoR{
		records: map[string]pgdb.Record{
			"healthy": rec("healthy"),
			"missing": rec("missing"),
			// "vanished" is enumerated by ListSHAs but Fetch returns nil.
			"vanished": rec("vanished"),
		},
		order:    []string{"healthy", "missing", "vanished"},
		vanished: setOf("vanished"),
	}
	proj := &fakeProjection{
		docs: setOf("healthy", "orphan"),
	}

	r := quietReconciler(t)
	r.Resolve = func(profile, vault string) (reconcileSoR, reconcileProjection, bool) {
		return sor, proj, true
	}
	if err := r.reconcileBinding(context.Background(), VaultKey{Profile: "p", Vault: "v"}); err != nil {
		t.Fatalf("reconcileBinding: %v", err)
	}

	// "missing" re-projected (present in SoR, absent from OS, not vanished).
	if !equalStrings(sortedCopy(proj.projected), []string{"missing"}) {
		t.Errorf("projected = %v, want [missing]", proj.projected)
	}
	// "orphan" deleted (present in OS, absent from SoR).
	if !equalStrings(sortedCopy(proj.deleted), []string{"orphan"}) {
		t.Errorf("deleted = %v, want [orphan]", proj.deleted)
	}
	// "healthy" untouched; "vanished" skipped (never projected).
	for _, sha := range proj.projected {
		if sha == "healthy" || sha == "vanished" {
			t.Errorf("unexpected re-project of %q", sha)
		}
	}
	// Final projection: healthy + missing (orphan gone, vanished never added).
	if _, ok := proj.docs["orphan"]; ok {
		t.Error("orphan should be deleted from projection")
	}
	if _, ok := proj.docs["missing"]; !ok {
		t.Error("missing should be present in projection after repair")
	}
	if _, ok := proj.docs["vanished"]; ok {
		t.Error("vanished must not be projected")
	}
}

// TestReconcileBinding_ResolveMiss confirms a binding whose view isn't
// registered is skipped without error (no repairs attempted).
func TestReconcileBinding_ResolveMiss(t *testing.T) {
	r := quietReconciler(t)
	r.Resolve = func(profile, vault string) (reconcileSoR, reconcileProjection, bool) {
		return nil, nil, false
	}
	if err := r.reconcileBinding(context.Background(), VaultKey{Profile: "p", Vault: "v"}); err != nil {
		t.Fatalf("reconcileBinding on resolve-miss: %v", err)
	}
}

// TestReconcileBinding_ListError surfaces a SoR list error.
func TestReconcileBinding_ListError(t *testing.T) {
	r := quietReconciler(t)
	wantErr := errors.New("boom")
	r.Resolve = func(profile, vault string) (reconcileSoR, reconcileProjection, bool) {
		return &erroringSoR{err: wantErr}, &fakeProjection{docs: setOf()}, true
	}
	err := r.reconcileBinding(context.Background(), VaultKey{Profile: "p", Vault: "v"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("reconcileBinding err = %v, want %v", err, wantErr)
	}
}

type erroringSoR struct{ err error }

func (s *erroringSoR) ListSHAs(context.Context, string, string, int64, int) ([]recordSHA, error) {
	return nil, s.err
}
func (s *erroringSoR) Fetch(context.Context, string, string, string) (*pgdb.Record, error) {
	return nil, nil
}

// TestReconcileConfigInterval covers the disable sentinel + default mapping.
func TestReconcileConfigInterval(t *testing.T) {
	if got := (ReconcileConfig{IntervalSecs: 0}).Interval(); got != 0 {
		t.Errorf("IntervalSecs=0 → %v, want 0 (disabled)", got)
	}
	if got := (ReconcileConfig{IntervalSecs: -1}).Interval(); got != 0 {
		t.Errorf("IntervalSecs=-1 → %v, want 0 (disabled)", got)
	}
	if got := (ReconcileConfig{IntervalSecs: 60}).Interval().Seconds(); got != 60 {
		t.Errorf("IntervalSecs=60 → %vs, want 60s", got)
	}
	if !NewReconciler(nil, 60).Enabled() {
		t.Error("reconciler with interval 60 should be Enabled")
	}
	if NewReconciler(nil, 0).Enabled() {
		t.Error("reconciler with interval 0 should be disabled")
	}
}
