package brain

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClient_ListRecords_EncodesFiltersAndDecodes(t *testing.T) {
	synth := false
	want := ListRecordsResponse{
		Records: []RecordDTO{{
			SHA:       "abc123",
			Kind:      "note",
			Title:     "A",
			Body:      "b",
			Tags:      []string{"tax"},
			UpdatedAt: time.Unix(1000, 0).UTC(),
		}},
		NextAfterID: 42,
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/api/brain/records" {
			t.Errorf("path = %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("after_id") != "10" {
			t.Errorf("after_id = %q, want 10", q.Get("after_id"))
		}
		if q.Get("limit") != "50" {
			t.Errorf("limit = %q, want 50", q.Get("limit"))
		}
		if got := q["kind"]; len(got) != 1 || got[0] != "note" {
			t.Errorf("kind = %v", got)
		}
		if got := q["tag"]; len(got) != 2 || got[0] != "tax" || got[1] != "irs" {
			t.Errorf("tag = %v, want [tax irs]", got)
		}
		if q.Get("topic") != "memory" {
			t.Errorf("topic = %q", q.Get("topic"))
		}
		if q.Get("synthesised") != "false" {
			t.Errorf("synthesised = %q, want false", q.Get("synthesised"))
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer; got %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer ts.Close()

	c, _ := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok"})
	got, err := c.ListRecords(context.Background(), ListRecordsRequest{
		AfterID:     10,
		Limit:       50,
		Kinds:       []string{"note"},
		Tags:        []string{"tax", "irs"},
		Topic:       "memory",
		Synthesised: &synth,
	})
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if got.NextAfterID != 42 {
		t.Errorf("NextAfterID = %d, want 42", got.NextAfterID)
	}
	if len(got.Records) != 1 || got.Records[0].SHA != "abc123" {
		t.Fatalf("records = %+v", got.Records)
	}
}

func TestClient_ListRecords_ChangeFeedMode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("since") != "2026-06-01T12:00:00Z" {
			t.Errorf("since = %q", q.Get("since"))
		}
		if q.Get("after_id") != "7" {
			t.Errorf("after_id = %q", q.Get("after_id"))
		}
		_ = json.NewEncoder(w).Encode(ListRecordsResponse{
			Records:     []RecordDTO{{SHA: "x", Kind: "note"}},
			NextAfterID: 9,
			NextSince:   "2026-06-01T12:05:00Z",
		})
	}))
	defer ts.Close()
	c, _ := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok"})
	got, err := c.ListRecords(context.Background(), ListRecordsRequest{
		Since:   "2026-06-01T12:00:00Z",
		AfterID: 7,
	})
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if got.NextSince != "2026-06-01T12:05:00Z" || got.NextAfterID != 9 {
		t.Errorf("cursor = (%q,%d), want (2026-06-01T12:05:00Z,9)", got.NextSince, got.NextAfterID)
	}
}

func TestClient_ListRecords_DaemonUnreachable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ts.Close() // closed → connection refused

	c, _ := NewClient(ClientOpts{BaseURL: ts.URL, Token: "tok"})
	_, err := c.ListRecords(context.Background(), ListRecordsRequest{})
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("err = %v, want ErrDaemonUnreachable", err)
	}
}
