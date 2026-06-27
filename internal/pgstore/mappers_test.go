package pgstore

import (
	"testing"
	"time"
)

func TestOptText(t *testing.T) {
	tests := []struct {
		in        string
		wantValid bool
		wantStr   string
	}{
		{"", false, ""},
		{"hello", true, "hello"},
		{"\x00", false, ""},     // NUL-only sanitises to empty → NULL
		{"\x00\x00", false, ""}, // ditto
		{"a\x00b", true, "ab"},  // embedded NUL stripped, rest survives
	}
	for _, tt := range tests {
		got := OptText(tt.in)
		if got.Valid != tt.wantValid || got.String != tt.wantStr {
			t.Errorf("OptText(%q) = {%q,%v}, want {%q,%v}", tt.in, got.String, got.Valid, tt.wantStr, tt.wantValid)
		}
	}
}

func TestOptTimestamptz(t *testing.T) {
	if got := OptTimestamptz(nil); got.Valid {
		t.Errorf("nil time should map to NULL, got valid=%v", got.Valid)
	}
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	got := OptTimestamptz(&now)
	if !got.Valid || !got.Time.Equal(now) {
		t.Errorf("non-nil time should map to valid, got %+v", got)
	}
}

func TestOptInt8(t *testing.T) {
	for _, n := range []int64{0, -1, -1000} {
		if got := OptInt8(n); got.Valid {
			t.Errorf("OptInt8(%d) should be NULL, got valid=%v", n, got.Valid)
		}
	}
	if got := OptInt8(42); !got.Valid || got.Int64 != 42 {
		t.Errorf("OptInt8(42) should be valid 42, got %+v", got)
	}
}

func TestOptVector(t *testing.T) {
	if got := OptVector(nil); got != nil {
		t.Errorf("nil embedding should map to nil vector, got %+v", got)
	}
	if got := OptVector([]float32{}); got != nil {
		t.Errorf("empty embedding should map to nil vector, got %+v", got)
	}
	emb := []float32{0.1, 0.2, 0.3}
	got := OptVector(emb)
	if got == nil {
		t.Fatal("non-empty embedding should map to a vector, got nil")
	}
	slice := got.Slice()
	if len(slice) != len(emb) {
		t.Fatalf("vector length = %d, want %d", len(slice), len(emb))
	}
	for i := range emb {
		if slice[i] != emb[i] {
			t.Errorf("vector[%d] = %v, want %v", i, slice[i], emb[i])
		}
	}
}

func TestNonNilStrings(t *testing.T) {
	if got := NonNilStrings(nil); got == nil {
		t.Error("nil input must become a non-nil slice (NOT NULL DEFAULT '{}')")
	} else if len(got) != 0 {
		t.Errorf("nil input should become empty slice, got %v", got)
	}
	got := NonNilStrings([]string{"a\x00", "b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("NULs should be stripped per element, got %v", got)
	}
}
