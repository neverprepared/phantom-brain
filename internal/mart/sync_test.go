package mart

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/neverprepared/phantom-brain/internal/brain"
)

// changeFeedServer is a minimal /api/brain/records change-feed: it holds an
// ordered (updated_at, id) list of note records and serves the compound-keyset
// since-mode the mart Sync drives. Append to `recs` between Sync calls to
// simulate upstream changes.
type feedRec struct {
	dto     brain.RecordDTO
	id      int64
	updated time.Time
}

func changeFeedServer(t *testing.T, recs *[]feedRec) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		since, _ := time.Parse(time.RFC3339, q.Get("since"))
		afterID, _ := strconv.ParseInt(q.Get("after_id"), 10, 64)
		var out brain.ListRecordsResponse
		for _, fr := range *recs {
			after := fr.updated.After(since) || (fr.updated.Equal(since) && fr.id > afterID)
			if after {
				out.Records = append(out.Records, fr.dto)
				out.NextAfterID = fr.id
				out.NextSince = fr.updated.Format(time.RFC3339Nano)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

func note(id int64, title string, ts time.Time) feedRec {
	return feedRec{
		id:      id,
		updated: ts,
		dto: brain.RecordDTO{
			SHA:       title + "000000000000",
			Kind:      "note",
			Title:     title,
			Body:      "body of " + title,
			UpdatedAt: ts,
		},
	}
}

func TestSync_BootstrapWritesAllAndAdvancesCursor(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	recs := []feedRec{
		note(1, "alpha", base),
		note(2, "bravo", base.Add(time.Minute)),
		note(3, "charlie", base.Add(2*time.Minute)),
	}
	ts := changeFeedServer(t, &recs)
	defer ts.Close()
	c, _ := brain.NewClient(brain.ClientOpts{BaseURL: ts.URL, Token: "tok"})

	dest := filepath.Join(t.TempDir(), "_mart")
	spec := Spec{Name: "m", Profile: "p", Vault: "v", Dest: dest, SkipAttachments: true}

	res, cur, err := Sync(context.Background(), spec, c, Cursor{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.RecordsWritten != 3 {
		t.Errorf("RecordsWritten = %d, want 3", res.RecordsWritten)
	}
	if cur.AfterID != 3 || cur.Since == "" {
		t.Errorf("cursor = %+v, want AfterID 3 + non-empty Since", cur)
	}
	if _, err := os.Stat(filepath.Join(dest, MarkerFile)); err != nil {
		t.Errorf("marker missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "index.md")); err != nil {
		t.Errorf("index.md missing: %v", err)
	}
	mds, _ := filepath.Glob(filepath.Join(dest, "*.md"))
	if len(mds) != 4 { // 3 notes + index.md
		t.Errorf("got %d .md files, want 4", len(mds))
	}
}

func TestSync_IncrementalUpsertsWithoutWiping(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	recs := []feedRec{note(1, "alpha", base), note(2, "bravo", base.Add(time.Minute))}
	ts := changeFeedServer(t, &recs)
	defer ts.Close()
	c, _ := brain.NewClient(brain.ClientOpts{BaseURL: ts.URL, Token: "tok"})

	dest := filepath.Join(t.TempDir(), "_mart")
	spec := Spec{Name: "m", Profile: "p", Vault: "v", Dest: dest, SkipAttachments: true}

	// First sync: 2 records.
	_, cur, err := Sync(context.Background(), spec, c, Cursor{})
	if err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	before, _ := filepath.Glob(filepath.Join(dest, "*.md"))
	if len(before) != 3 { // alpha, bravo, index
		t.Fatalf("after first sync: %d .md, want 3", len(before))
	}

	// A new record appears upstream (later updated_at).
	recs = append(recs, note(3, "charlie", base.Add(2*time.Minute)))

	// Second sync from the saved cursor: only charlie is new.
	res, cur2, err := Sync(context.Background(), spec, c, cur)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if res.RecordsWritten != 1 {
		t.Errorf("second sync wrote %d, want 1 (only the new record)", res.RecordsWritten)
	}
	if cur2.AfterID != 3 {
		t.Errorf("cursor did not advance to 3: %+v", cur2)
	}
	after, _ := filepath.Glob(filepath.Join(dest, "*.md"))
	if len(after) != 4 { // alpha, bravo, charlie, index — the first two SURVIVED (no wipe)
		t.Errorf("after incremental sync: %d .md, want 4 (existing files must survive)", len(after))
	}
}

func TestSync_NoOpWhenNothingChanged(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	recs := []feedRec{note(1, "alpha", base)}
	ts := changeFeedServer(t, &recs)
	defer ts.Close()
	c, _ := brain.NewClient(brain.ClientOpts{BaseURL: ts.URL, Token: "tok"})
	dest := filepath.Join(t.TempDir(), "_mart")
	spec := Spec{Name: "m", Profile: "p", Vault: "v", Dest: dest, SkipAttachments: true}

	_, cur, _ := Sync(context.Background(), spec, c, Cursor{})
	res, cur2, err := Sync(context.Background(), spec, c, cur)
	if err != nil {
		t.Fatalf("no-op Sync: %v", err)
	}
	if res.RecordsWritten != 0 {
		t.Errorf("no-op sync wrote %d, want 0", res.RecordsWritten)
	}
	if cur2 != cur {
		t.Errorf("cursor moved on a no-op sync: %+v → %+v", cur, cur2)
	}
}
