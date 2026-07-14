package mart

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/brain"
)

// fakeSource serves records from an in-memory slice, paginated by index, so
// Build can be tested without a daemon.
type fakeSource struct {
	recs     []brain.RecordDTO
	pageSize int
	calls    int
}

func (f *fakeSource) Page(_ context.Context, afterID int64) ([]brain.RecordDTO, int64, error) {
	f.calls++
	ps := f.pageSize
	if ps <= 0 {
		ps = 2
	}
	start := int(afterID) // fake ids are 1-based indices; afterID is the last returned
	if start >= len(f.recs) {
		return nil, 0, nil
	}
	end := start + ps
	if end > len(f.recs) {
		end = len(f.recs)
	}
	page := f.recs[start:end]
	var next int64
	if len(page) == ps && end < len(f.recs) {
		next = int64(end)
	}
	return page, next, nil
}

func recs(n int) []brain.RecordDTO {
	out := make([]brain.RecordDTO, n)
	for i := 0; i < n; i++ {
		out[i] = brain.RecordDTO{
			SHA:   string(rune('a'+i)) + "000000000000",
			Kind:  "note",
			Title: "rec" + string(rune('A'+i)),
			Body:  "body",
		}
	}
	return out
}

func TestBuild_EphemeralWritesFilesMarkerAndIndex(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "_mart")
	spec := Spec{Name: "m", Profile: "p", Vault: "v", Dest: dest, Ephemeral: true}
	src := &fakeSource{recs: recs(5), pageSize: 2}

	res, err := Build(context.Background(), spec, src)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.RecordsWritten != 5 {
		t.Errorf("RecordsWritten = %d, want 5", res.RecordsWritten)
	}
	if _, err := os.Stat(filepath.Join(dest, MarkerFile)); err != nil {
		t.Errorf("marker missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "index.md")); err != nil {
		t.Errorf("index.md missing: %v", err)
	}
	entries, _ := os.ReadDir(dest)
	// 5 record files + marker + index.md = 7
	if len(entries) != 7 {
		t.Errorf("dest has %d entries, want 7", len(entries))
	}
}

func TestBuild_Idempotent(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "_mart")
	spec := Spec{Name: "m", Profile: "p", Vault: "v", Dest: dest, Ephemeral: true}
	if _, err := Build(context.Background(), spec, &fakeSource{recs: recs(3)}); err != nil {
		t.Fatalf("first Build: %v", err)
	}
	if _, err := Build(context.Background(), spec, &fakeSource{recs: recs(3)}); err != nil {
		t.Fatalf("second Build: %v", err)
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 5 { // 3 records + marker + index
		t.Errorf("after rebuild dest has %d entries, want 5", len(entries))
	}
}

func TestBuild_RefusesUnmarkedNonEmptyDir(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "human-vault")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	// A hand-authored note the mart must never clobber.
	if err := os.WriteFile(filepath.Join(dest, "tax-timeline.md"), []byte("# mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Name: "m", Profile: "p", Vault: "v", Dest: dest, Ephemeral: true}
	_, err := Build(context.Background(), spec, &fakeSource{recs: recs(1)})
	if err == nil {
		t.Fatal("Build must refuse an unmarked non-empty directory")
	}
	// The human file must still be there.
	if _, err := os.Stat(filepath.Join(dest, "tax-timeline.md")); err != nil {
		t.Errorf("human file was disturbed: %v", err)
	}
}

func TestBuild_AdoptsEmptyDir(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Name: "m", Profile: "p", Vault: "v", Dest: dest, Ephemeral: true}
	if _, err := Build(context.Background(), spec, &fakeSource{recs: recs(2)}); err != nil {
		t.Fatalf("Build should adopt an empty dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, MarkerFile)); err != nil {
		t.Errorf("marker missing after adopting empty dir: %v", err)
	}
}
