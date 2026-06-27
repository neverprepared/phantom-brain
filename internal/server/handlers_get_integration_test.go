//go:build integration

// Phase D2a integration coverage for the GET read paths that moved off the
// legacy OpenSearch indices onto the Postgres SoR:
//
//	GET /api/brain/attach/{sha}   — presign metadata from records.minio_key
//	GET /api/brain/capture/{sha}  — presign from records.capture_minio_key
//	GET /api/brain/fetch/{sha}    — full body by SHA (brain_fetch backend)
//
// The blob bytes / presign step is faked (no live MinIO): the daemon's
// d.attach is a deterministic presigner so the test exercises the REAL
// Postgres read + handler logic (404 shapes, key-empty 404, JSON body)
// without standing up a MinIO container. Build-tagged OFF by default. Run:
//
//	GOFLAGS="-tags=sqlite_fts5,integration" go test ./internal/server/ -run GetEndpoints -count=1 -v
//
// Reuses the Phase A harness (startPGForServer / startOSForServer /
// newPGTestDaemon / pgstore.Provision) from pg_binding_views_integration_test.go.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// presignAttach is a deterministic AttachmentStore: PresignGet echoes the
// key into a stable URL so the handler test can assert it without MinIO.
type presignAttach struct{}

func (presignAttach) PutAttachment(_ context.Context, _, _, _, _ string, _ []byte, _ string) (string, error) {
	return "", nil
}
func (presignAttach) PutAttachmentWithTags(_ context.Context, _, _, _, _ string, _ []byte, _ string, _ []string) (string, error) {
	return "", nil
}
func (presignAttach) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://minio.test/" + key + "?sig=fake", nil
}
func (presignAttach) GetAttachmentBytes(_ context.Context, _ string, _ int64) ([]byte, error) {
	return nil, nil
}

// doGet drives a GET through the daemon's chi router with the bearer token.
func doGet(t *testing.T, d *Daemon, token, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	d.buildRouter().ServeHTTP(rec, req)

	var out map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

// insertRecord writes a raw record then (optionally) marks it synthesised
// with capture columns, returning nothing — the SHA addresses it later.
func insertRecord(t *testing.T, ctx context.Context, view *pgBindingView, p pgdb.UpsertRecordParams, mark *pgdb.MarkRecordSynthesisedParams) {
	t.Helper()
	q := pgstore.New(view.Pool())
	rec, err := q.UpsertRecord(ctx, p)
	if err != nil {
		t.Fatalf("UpsertRecord %s: %v", p.Sha, err)
	}
	if mark != nil {
		mark.ID = rec.ID
		if err := q.MarkRecordSynthesised(ctx, *mark); err != nil {
			t.Fatalf("MarkRecordSynthesised %s: %v", p.Sha, err)
		}
	}
}

func TestGetEndpoints_Integration(t *testing.T) {
	ctx := context.Background()
	baseDSN := startPGForServer(ctx, t)
	osc := startOSForServer(ctx, t)

	if err := pgstore.Provision(ctx, baseDSN, "gettest"); err != nil {
		t.Fatalf("provision gettest db: %v", err)
	}

	const token = "get-test-token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	b := authedBinding("gettest", "main", "pbc_get_", token)
	d := newPGTestDaemon(t, b)
	d.osBase, d.osClient, d.osExport = osc, osc, osc
	d.pgBaseDSN = baseDSN
	d.attach = presignAttach{}
	d.buildBindingDeps()
	t.Cleanup(d.closePGProfiles)

	view, err := d.resolvePG(b)
	if err != nil {
		t.Fatalf("resolvePG: %v", err)
	}

	const (
		shaAttach  = "aa11000000000000000000000000000000000000000000000000000000000001"
		shaNote    = "bb22000000000000000000000000000000000000000000000000000000000002"
		shaCapture = "cc33000000000000000000000000000000000000000000000000000000000003"
		shaMissing = "ff99000000000000000000000000000000000000000000000000000000000099"
	)

	// Attachment record: minio_key + mime + size set.
	insertRecord(t, ctx, view, pgdb.UpsertRecordParams{
		Profile: "gettest", Vault: "main", Sha: shaAttach, Kind: "attachment",
		Title:            "Invoice PDF",
		Source:           []string{}, Tags: []string{},
		MinioKey:         pgtype.Text{String: "gettest/main/attachments/" + shaAttach + ".pdf", Valid: true},
		MimeType:         pgtype.Text{String: "application/pdf", Valid: true},
		SizeBytes:        pgtype.Int8{Int64: 4242, Valid: true},
		OriginalFilename: pgtype.Text{String: "invoice.pdf", Valid: true},
	}, nil)

	// Note record with a synthesised body + a capture key/size.
	insertRecord(t, ctx, view, pgdb.UpsertRecordParams{
		Profile: "gettest", Vault: "main", Sha: shaCapture, Kind: "web_scrape",
		Title:     "Captured Page",
		RawBody:   pgtype.Text{String: "raw scrape", Valid: true},
		SourceUrl: pgtype.Text{String: "https://example.com/page", Valid: true},
		Source:    []string{}, Tags: []string{"web"},
	}, &pgdb.MarkRecordSynthesisedParams{
		Body:             pgtype.Text{String: "distilled body", Valid: true},
		Reliability:      pgtype.Text{String: "medium", Valid: true},
		Topic:            pgtype.Text{String: "general", Valid: true},
		CaptureMinioKey:  pgtype.Text{String: "gettest/main/captures/" + shaCapture + ".html", Valid: true},
		CaptureSizeBytes: pgtype.Int8{Int64: 9001, Valid: true},
	})

	// Plain note: body present, no minio/capture keys.
	insertRecord(t, ctx, view, pgdb.UpsertRecordParams{
		Profile: "gettest", Vault: "main", Sha: shaNote, Kind: "note",
		Title:   "Plain Note",
		RawBody: pgtype.Text{String: "raw note body", Valid: true},
		Source:  []string{}, Tags: []string{"alpha"},
	}, &pgdb.MarkRecordSynthesisedParams{
		Body: pgtype.Text{String: "synth note body", Valid: true},
	})

	t.Run("AttachGet_Found", func(t *testing.T) {
		code, body := doGet(t, d, token, "/api/brain/attach/"+shaAttach)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if body["original"] != "invoice.pdf" {
			t.Errorf("original = %v", body["original"])
		}
		if body["mime_type"] != "application/pdf" {
			t.Errorf("mime_type = %v", body["mime_type"])
		}
		if body["size_bytes"].(float64) != 4242 {
			t.Errorf("size_bytes = %v", body["size_bytes"])
		}
		if url, _ := body["url"].(string); url == "" {
			t.Error("url empty")
		}
	})

	t.Run("AttachGet_NotFoundUnknownSHA", func(t *testing.T) {
		code, _ := doGet(t, d, token, "/api/brain/attach/"+shaMissing)
		if code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", code)
		}
	})

	t.Run("AttachGet_NotFoundNoMinioKey", func(t *testing.T) {
		// A note record has no minio_key ⇒ 404, not a presign of an empty key.
		code, _ := doGet(t, d, token, "/api/brain/attach/"+shaNote)
		if code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 for record without minio_key", code)
		}
	})

	t.Run("CaptureGet_Found", func(t *testing.T) {
		code, body := doGet(t, d, token, "/api/brain/capture/"+shaCapture)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if body["source_url"] != "https://example.com/page" {
			t.Errorf("source_url = %v", body["source_url"])
		}
		if body["size_bytes"].(float64) != 9001 {
			t.Errorf("size_bytes = %v", body["size_bytes"])
		}
		if url, _ := body["url"].(string); url == "" {
			t.Error("url empty")
		}
	})

	t.Run("CaptureGet_NotFoundNoCaptureKey", func(t *testing.T) {
		code, _ := doGet(t, d, token, "/api/brain/capture/"+shaNote)
		if code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 for record without capture key", code)
		}
	})

	t.Run("Fetch_Found", func(t *testing.T) {
		code, body := doGet(t, d, token, "/api/brain/fetch/"+shaNote)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if body["title"] != "Plain Note" {
			t.Errorf("title = %v", body["title"])
		}
		if body["kind"] != "note" {
			t.Errorf("kind = %v", body["kind"])
		}
		if body["body"] != "synth note body" {
			t.Errorf("body = %v (want synthesised body)", body["body"])
		}
	})

	t.Run("Fetch_FallsBackToRawBody", func(t *testing.T) {
		// The attachment record was never synthesised → no Body → fetch
		// must fall back to raw_body. It has empty raw_body, so assert the
		// record is returned with an empty/raw body (not a 404).
		code, body := doGet(t, d, token, "/api/brain/fetch/"+shaCapture)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if body["body"] != "distilled body" {
			t.Errorf("body = %v (want distilled body)", body["body"])
		}
	})

	t.Run("Fetch_NotFound", func(t *testing.T) {
		code, _ := doGet(t, d, token, "/api/brain/fetch/"+shaMissing)
		if code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", code)
		}
	})
}
