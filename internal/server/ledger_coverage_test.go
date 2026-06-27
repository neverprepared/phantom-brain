package server

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func openLedgerForTest(t *testing.T) *Ledger {
	t.Helper()
	l, err := OpenLedger(DataDir(t.TempDir()), "personal", "memory")
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func TestOpenLedger_RequiresProfileVault(t *testing.T) {
	if _, err := OpenLedger(DataDir(t.TempDir()), "", "v"); err == nil {
		t.Error("empty profile should error")
	}
	if _, err := OpenLedger(DataDir(t.TempDir()), "p", ""); err == nil {
		t.Error("empty vault should error")
	}
}

func TestLedger_InsertGetRoundTrip(t *testing.T) {
	l := openLedgerForTest(t)
	merged := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rec := MergeRecord{
		BrainID:         "brain-1",
		ContributorID:   "agent-7",
		Profile:         "personal",
		Vault:           "memory",
		MergedAt:        merged,
		RawCount:        3,
		AttachmentCount: 1,
		PayloadBytes:    2048,
	}
	if err := l.Insert(rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := l.Get("brain-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.BrainID != "brain-1" || got.ContributorID != "agent-7" {
		t.Errorf("identity mismatch: %+v", got)
	}
	if got.RawCount != 3 || got.AttachmentCount != 1 || got.PayloadBytes != 2048 {
		t.Errorf("counts mismatch: %+v", got)
	}
	if !got.MergedAt.Equal(merged) {
		t.Errorf("merged_at = %v, want %v", got.MergedAt, merged)
	}
}

func TestLedger_Insert_Validation(t *testing.T) {
	l := openLedgerForTest(t)
	bad := []MergeRecord{
		{BrainID: "", Profile: "p", Vault: "v"},
		{BrainID: "b", Profile: "", Vault: "v"},
		{BrainID: "b", Profile: "p", Vault: ""},
	}
	for i, r := range bad {
		if err := l.Insert(r); err == nil {
			t.Errorf("case %d: expected validation error for %+v", i, r)
		}
	}
}

func TestLedger_Insert_DefaultsMergedAt(t *testing.T) {
	l := openLedgerForTest(t)
	before := time.Now().UTC().Add(-time.Second)
	if err := l.Insert(MergeRecord{BrainID: "b-zero", Profile: "p", Vault: "v"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := l.Get("b-zero")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.MergedAt.Before(before) {
		t.Errorf("zero merged_at should default to ~now, got %v", got.MergedAt)
	}
}

func TestLedger_Insert_DuplicateIsSentinel(t *testing.T) {
	l := openLedgerForTest(t)
	rec := MergeRecord{BrainID: "dup", Profile: "p", Vault: "v"}
	if err := l.Insert(rec); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	err := l.Insert(rec)
	if !errors.Is(err, ErrDuplicateMerge) {
		t.Errorf("re-insert should return ErrDuplicateMerge, got %v", err)
	}
}

func TestLedger_Get_NoRows(t *testing.T) {
	l := openLedgerForTest(t)
	_, err := l.Get("never-merged")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get of missing brain should be sql.ErrNoRows, got %v", err)
	}
}

func TestLedger_List_OrderAndLimit(t *testing.T) {
	l := openLedgerForTest(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := l.Insert(MergeRecord{
			BrainID:  "b" + string(rune('0'+i)),
			Profile:  "p",
			Vault:    "v",
			MergedAt: base.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	// Default limit (<=0 → 100) returns all, newest first.
	all, err := l.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List returned %d rows, want 3", len(all))
	}
	if all[0].BrainID != "b2" {
		t.Errorf("newest-first ordering broken, head = %q", all[0].BrainID)
	}
	// Explicit limit caps the result.
	capped, err := l.List(2)
	if err != nil {
		t.Fatalf("List(2): %v", err)
	}
	if len(capped) != 2 {
		t.Errorf("List(2) returned %d rows, want 2", len(capped))
	}
}

func TestLedger_Close_Idempotentish(t *testing.T) {
	l, err := OpenLedger(DataDir(t.TempDir()), "p", "v")
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// nil-db Ledger Close is a no-op.
	empty := &Ledger{}
	if err := empty.Close(); err != nil {
		t.Errorf("Close on nil-db ledger should be nil, got %v", err)
	}
}

func TestIsSQLitePrimaryKeyConflict(t *testing.T) {
	if isSQLitePrimaryKeyConflict(nil) {
		t.Error("nil is not a conflict")
	}
	if !isSQLitePrimaryKeyConflict(errors.New("UNIQUE constraint failed: merges.brain_id")) {
		t.Error("UNIQUE constraint message should match")
	}
	if !isSQLitePrimaryKeyConflict(errors.New("PRIMARY KEY must be unique")) {
		t.Error("PRIMARY KEY message should match")
	}
	if isSQLitePrimaryKeyConflict(errors.New("disk I/O error")) {
		t.Error("unrelated error should not match")
	}
}
