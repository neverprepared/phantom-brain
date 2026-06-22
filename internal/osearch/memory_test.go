package osearch

import (
	"encoding/json"
	"testing"
)

func TestKindIsValid(t *testing.T) {
	for _, k := range []Kind{
		KindNote, KindWebScrape, KindTaskSummary,
		KindAttachmentStub, KindEmailImport, KindManualCurate,
	} {
		if !k.IsValid() {
			t.Errorf("Kind(%q).IsValid() = false, want true", k)
		}
	}
	for _, bad := range []Kind{"", "experiment", "unknown", "Note", "WEB_SCRAPE"} {
		if bad.IsValid() {
			t.Errorf("Kind(%q).IsValid() = true, want false", bad)
		}
	}
}

func TestMemoryTypeIsValid(t *testing.T) {
	// Empty allowed (caller didn't classify).
	for _, m := range []MemoryType{"", MemorySemantic, MemoryEpisodic, MemoryProcedural} {
		if !m.IsValid() {
			t.Errorf("MemoryType(%q).IsValid() = false, want true", m)
		}
	}
	for _, bad := range []MemoryType{"declarative", "Semantic", "implicit"} {
		if bad.IsValid() {
			t.Errorf("MemoryType(%q).IsValid() = true, want false", bad)
		}
	}
}

func TestSummaryDocCarriesMemoryFields(t *testing.T) {
	in := SummaryDoc{
		Profile: "p", Vault: "v", SHA: "abc",
		Title: "t", Body: "b",
		Kind:       KindTaskSummary,
		MemoryType: MemoryEpisodic,
		Source:     []string{"task:42", "agent:x"},
		References: []string{"deadbeef"},
	}
	bs, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(bs, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"kind", "memory_type", "source", "references"} {
		if _, ok := out[key]; !ok {
			t.Errorf("SummaryDoc JSON missing %q", key)
		}
	}
}

func TestAttachmentDocCarriesMemoryFields(t *testing.T) {
	in := AttachmentDoc{
		Profile: "p", Vault: "v", SHA: "abc",
		OriginalFilename: "x.pdf",
		Kind:             KindAttachmentStub,
		MemoryType:       MemorySemantic,
		Source:           []string{"local:/Users/.../file.pdf"},
	}
	bs, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(bs, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"kind", "memory_type", "source"} {
		if _, ok := out[key]; !ok {
			t.Errorf("AttachmentDoc JSON missing %q", key)
		}
	}
}

// TestSchemaIncludesMemoryFields guards the OS mapping against drift —
// if someone adds a field to the struct without updating the mapping,
// faceted queries silently degrade to dynamic-mapping defaults.
func TestSchemaIncludesMemoryFields(t *testing.T) {
	must := func(name string, m map[string]any, fields ...string) {
		t.Helper()
		props, ok := m["mappings"].(map[string]any)["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s mapping malformed", name)
		}
		for _, f := range fields {
			if _, ok := props[f]; !ok {
				t.Errorf("%s mapping missing %q", name, f)
			}
		}
	}
	must("summaries", summariesMapping(),
		"kind", "memory_type", "source", "references", "captured_at")
	must("attachments", attachmentsMapping(),
		"kind", "memory_type", "source", "references", "captured_at", "tags")
}
