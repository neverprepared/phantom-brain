package server

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// TestCapturedAtOmittedWhenNil guards the wire contract: a nil
// CapturedAt must drop out of the marshaled JSON entirely rather than
// materializing as the time.Time zero value (0001-01-01T00:00:00Z),
// which would otherwise leak into OpenSearch and look like real data
// on every doc that didn't set the field.
func TestCapturedAtOmittedWhenNil(t *testing.T) {
	t.Run("SummaryDoc", func(t *testing.T) {
		doc := osearch.SummaryDoc{
			Profile: "p", Vault: "v", SHA: "abc",
			Title: "t", Body: "b",
		}
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if bytes.Contains(out, []byte("captured_at")) {
			t.Errorf("nil CapturedAt should be omitted; got %s", out)
		}
	})

	t.Run("AttachmentDoc", func(t *testing.T) {
		doc := osearch.AttachmentDoc{
			Profile: "p", Vault: "v", SHA: "abc",
			OriginalFilename: "x.pdf",
		}
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if bytes.Contains(out, []byte("captured_at")) {
			t.Errorf("nil CapturedAt should be omitted; got %s", out)
		}
	})

	t.Run("PerceiveRequestThroughApply", func(t *testing.T) {
		// PerceiveRequest with no CapturedAt -> applyMemoryFields onto
		// a fresh SummaryDoc -> marshal should still omit the field.
		req := PerceiveRequest{SHA: "abc", Title: "t", Body: "b"}
		var doc osearch.SummaryDoc
		applyMemoryFields(&doc, req.MemoryFields)
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if bytes.Contains(out, []byte("captured_at")) {
			t.Errorf("nil CapturedAt should be omitted after applyMemoryFields; got %s", out)
		}
	})

	t.Run("RoundTripWhenSet", func(t *testing.T) {
		// Sanity: when CapturedAt IS set, it must serialize and parse
		// back to the same instant. Guards against the pointer change
		// breaking the present-value path.
		when := time.Date(2025, time.July, 8, 12, 30, 0, 0, time.UTC)
		doc := osearch.SummaryDoc{
			Profile: "p", Vault: "v", SHA: "abc",
			Title: "t", Body: "b",
			CapturedAt: &when,
		}
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !bytes.Contains(out, []byte("captured_at")) {
			t.Fatalf("non-nil CapturedAt must serialize; got %s", out)
		}
		var back osearch.SummaryDoc
		if err := json.Unmarshal(out, &back); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if back.CapturedAt == nil || !back.CapturedAt.Equal(when) {
			t.Errorf("round-trip mismatch: got %v, want %v", back.CapturedAt, when)
		}
	})
}

// TestApplyMemoryFields_VaultKindDefault guards the vault→kind default: an
// unclassified write to a verbatim vault inherits that vault's kind (so the
// synth bypass fires), while an explicit kind always wins and non-verbatim
// vaults stay unset.
func TestApplyMemoryFields_VaultKindDefault(t *testing.T) {
	cases := []struct {
		name    string
		vault   string
		reqKind string
		want    osearch.Kind
	}{
		{"todo vault defaults to todo kind", "todo", "", osearch.KindTodo},
		{"sessions vault defaults to session kind", "sessions", "", osearch.KindSession},
		{"skills vault defaults to skill kind", "skills", "", osearch.KindSkill},
		{"memory vault stays unset", "memory", "", ""},
		{"artifacts vault stays unset", "artifacts", "", ""},
		{"explicit kind wins over vault default", "todo", string(osearch.KindNote), osearch.KindNote},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := osearch.SummaryDoc{Vault: tc.vault}
			applyMemoryFields(&doc, MemoryFields{Kind: tc.reqKind})
			if doc.Kind != tc.want {
				t.Errorf("vault=%q reqKind=%q: got kind %q, want %q", tc.vault, tc.reqKind, doc.Kind, tc.want)
			}
		})
	}
}
