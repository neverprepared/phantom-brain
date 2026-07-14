//go:build integration

// Integration coverage for GET /api/brain/records — the keyset-paginated
// enumeration that powers `pbrainctl mart build`. Build-tagged OFF by default.
// Run with:
//
//	GOFLAGS="-tags=sqlite_fts5,integration" go test ./internal/server/ -run ListRecords -count=1 -v
//
// Reuses the write-integration harness (startPGForServer, startOSForServer,
// newHandlerWriteDaemon, bindingWithToken, postJSON). Records are seeded via
// /perceive (synthesised=false), so the test exercises the synthesised filter
// without running the synth pipeline.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/pgstore"
)

func getRecords(t *testing.T, url, token, query string) (int, ListRecordsResponse) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url+"/api/brain/records"+query, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /records%s: %v", query, err)
	}
	defer resp.Body.Close()
	var out ListRecordsResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp.StatusCode, out
}

func TestListRecordsEndpoint_Integration(t *testing.T) {
	ctx := context.Background()
	baseDSN := startPGForServer(ctx, t)
	osc := startOSForServer(ctx, t)
	if err := pgstore.Provision(ctx, baseDSN, "lrtest"); err != nil {
		t.Fatalf("provision lrtest db: %v", err)
	}

	token := "lrtok"
	b := bindingWithToken("lrtest", "main", "lr_", token)
	url, _ := newHandlerWriteDaemon(t, baseDSN, osc, b)

	// Seed 3 unsynthesised records; two carry tag "tax".
	seed := []struct {
		sha, title string
		tags       []string
	}{
		{"aa11000000000000000000000000000000000000000000000000000000000001", "Return 2025", []string{"tax"}},
		{"bb22000000000000000000000000000000000000000000000000000000000002", "W2 import", []string{"tax", "irs"}},
		{"cc33000000000000000000000000000000000000000000000000000000000003", "Grocery note", []string{"food"}},
	}
	for _, s := range seed {
		resp := postJSON(t, url, token, "/api/brain/perceive", PerceiveRequest{
			SHA: s.sha, Title: s.title, Body: "body of " + s.title, Tags: s.tags,
		})
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("seed %s: status %d", s.title, resp.StatusCode)
		}
	}

	t.Run("default synthesised=true returns nothing (all unsynthesised)", func(t *testing.T) {
		code, out := getRecords(t, url, token, "")
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		if len(out.Records) != 0 {
			t.Errorf("want 0 synthesised records, got %d", len(out.Records))
		}
	})

	t.Run("keyset pagination over synthesised=false", func(t *testing.T) {
		code, page1 := getRecords(t, url, token, "?synthesised=false&limit=2")
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		if len(page1.Records) != 2 {
			t.Fatalf("page1 = %d records, want 2", len(page1.Records))
		}
		if page1.NextAfterID == 0 {
			t.Fatal("expected a non-zero cursor on a full page")
		}
		code, page2 := getRecords(t, url, token,
			fmt.Sprintf("?synthesised=false&limit=2&after_id=%d", page1.NextAfterID))
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		if len(page2.Records) != 1 {
			t.Fatalf("page2 = %d records, want 1", len(page2.Records))
		}
		if page2.NextAfterID != 0 {
			t.Errorf("short page must end the stream, got cursor %d", page2.NextAfterID)
		}
	})

	t.Run("tag filter is array-overlap", func(t *testing.T) {
		code, out := getRecords(t, url, token, "?synthesised=false&tag=tax")
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		if len(out.Records) != 2 {
			t.Errorf("tag=tax matched %d, want 2", len(out.Records))
		}
	})

	t.Run("missing token is 401", func(t *testing.T) {
		code, _ := getRecords(t, url, "", "?synthesised=false")
		if code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", code)
		}
	})

	t.Run("change feed: since echoes a cursor, resume returns nothing new", func(t *testing.T) {
		// From the beginning: all three unsynthesised records, with a cursor.
		code, all := getRecords(t, url, token, "?synthesised=false&since=1970-01-01T00:00:00Z")
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		if len(all.Records) != 3 {
			t.Fatalf("since-epoch returned %d, want 3", len(all.Records))
		}
		if all.NextSince == "" || all.NextAfterID == 0 {
			t.Fatalf("change-feed mode must echo (NextSince, NextAfterID); got (%q, %d)", all.NextSince, all.NextAfterID)
		}
		// Resume exactly after the last row → nothing changed since.
		q := fmt.Sprintf("?synthesised=false&since=%s&after_id=%d", neturl.QueryEscape(all.NextSince), all.NextAfterID)
		code, none := getRecords(t, url, token, q)
		if code != http.StatusOK {
			t.Fatalf("resume status %d", code)
		}
		if len(none.Records) != 0 {
			t.Errorf("resume from the tail cursor returned %d, want 0", len(none.Records))
		}
	})

	t.Run("malformed since is 400", func(t *testing.T) {
		code, _ := getRecords(t, url, token, "?since=not-a-timestamp")
		if code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", code)
		}
	})
}
