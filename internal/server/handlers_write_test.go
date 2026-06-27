package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// --- in-memory fakes ----------------------------------------------
//
// fakeOS / fakeAttach remain in the unit suite because the per-binding
// resolver tests (storage_overrides_test.go) assign them into a
// bindingDeps and assert resolveOS/resolveAttach return them. They no
// longer back any write-handler test: post-D1 the perceive/learn/attach
// handlers write to the Postgres SoR, which needs a live pool, so those
// happy-path tests moved to the integration suite (dual_write_integration_test.go).

type fakeOS struct {
	mu          sync.Mutex
	summaries   map[string]osearch.SummaryDoc
	entities    map[string]osearch.EntityDoc
	attachments map[string]osearch.AttachmentDoc
}

func newFakeOS() *fakeOS {
	return &fakeOS{
		summaries:   map[string]osearch.SummaryDoc{},
		entities:    map[string]osearch.EntityDoc{},
		attachments: map[string]osearch.AttachmentDoc{},
	}
}

func (f *fakeOS) UpsertSummary(_ context.Context, doc osearch.SummaryDoc, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summaries[osearch.DocID(doc.Profile, doc.Vault, doc.SHA)] = doc
	return nil
}
func (f *fakeOS) GetSummary(_ context.Context, profile, vault, sha string) (*osearch.SummaryDoc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	doc, ok := f.summaries[osearch.DocID(profile, vault, sha)]
	if !ok {
		return nil, nil
	}
	return &doc, nil
}
func (f *fakeOS) UpsertEntity(_ context.Context, doc osearch.EntityDoc, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entities[osearch.DocID(doc.Profile, doc.Vault, doc.Slug)] = doc
	return nil
}
func (f *fakeOS) GetEntity(_ context.Context, profile, vault, slug string) (*osearch.EntityDoc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	doc, ok := f.entities[osearch.DocID(profile, vault, slug)]
	if !ok {
		return nil, nil
	}
	return &doc, nil
}
func (f *fakeOS) UpsertAttachment(_ context.Context, doc osearch.AttachmentDoc, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachments[osearch.DocID(doc.Profile, doc.Vault, doc.SHA)] = doc
	return nil
}
func (f *fakeOS) GetAttachment(_ context.Context, profile, vault, sha string) (*osearch.AttachmentDoc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	doc, ok := f.attachments[osearch.DocID(profile, vault, sha)]
	if !ok {
		return nil, nil
	}
	return &doc, nil
}

func (f *fakeOS) DeleteSummary(_ context.Context, profile, vault, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.summaries, osearch.DocID(profile, vault, sha))
	return nil
}
func (f *fakeOS) ScrollSummaries(_ context.Context, profile, vault string, _ int, fn func(osearch.SummaryDoc) error) error {
	f.mu.Lock()
	docs := make([]osearch.SummaryDoc, 0, len(f.summaries))
	for _, d := range f.summaries {
		if d.Profile == profile && d.Vault == vault {
			docs = append(docs, d)
		}
	}
	f.mu.Unlock()
	for _, d := range docs {
		if err := fn(d); err != nil {
			return err
		}
	}
	return nil
}

type fakeAttach struct {
	mu      sync.Mutex
	blobs   map[string][]byte
	tags    map[string][]string
	failPut bool
}

func newFakeAttach() *fakeAttach {
	return &fakeAttach{blobs: map[string][]byte{}, tags: map[string][]string{}}
}

func (f *fakeAttach) PutAttachment(ctx context.Context, profile, vault, sha, ext string, body []byte, ct string) (string, error) {
	return f.PutAttachmentWithTags(ctx, profile, vault, sha, ext, body, ct, nil)
}

func (f *fakeAttach) PutAttachmentWithTags(_ context.Context, profile, vault, sha, ext string, body []byte, _ string, indexTags []string) (string, error) {
	if f.failPut {
		return "", errors.New("fake: put failed")
	}
	key := fmt.Sprintf("%s/%s/attachments/%s%s", profile, vault, sha, ext)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blobs[key] = append([]byte(nil), body...)
	if len(indexTags) > 0 {
		f.tags[key] = append([]string(nil), indexTags...)
	}
	return key, nil
}
func (f *fakeAttach) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://example.test/" + key + "?sig=fake", nil
}
func (f *fakeAttach) GetAttachmentBytes(_ context.Context, key string, _ int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.blobs[key]
	if !ok {
		return nil, errors.New("no such key: " + key)
	}
	return append([]byte(nil), b...), nil
}

// --- pure helpers (no rig, no I/O) --------------------------------

func TestValidateSHA(t *testing.T) {
	if err := validateSHA(strings.Repeat("0", 64)); err != nil {
		t.Errorf("64 hex zeros rejected: %v", err)
	}
	if err := validateSHA("short"); err == nil {
		t.Error("short sha accepted")
	}
	if err := validateSHA(strings.Repeat("Z", 64)); err == nil {
		t.Error("uppercase non-hex accepted")
	}
}

func TestExtFromFilename(t *testing.T) {
	for in, want := range map[string]string{
		"file.pdf":     ".pdf",
		"NOTES.md":     ".md",
		"no-ext":       "",
		"trailing.":    "",
		"path/leak.tx": ".tx",
		"a/b/c":        "",
	} {
		if got := extFromFilename(in); got != want {
			t.Errorf("extFromFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- handler VALIDATION tests (no PG, no Start) --------------------
//
// Phase D1: the write happy-paths now hit a live Postgres SoR and moved
// to the integration suite (handlers_write_integration_test.go). But the
// validation paths (bad JSON / bad SHA / missing fields / sha mismatch)
// return 400 BEFORE any SoR access, so they stay in the unit suite. They
// drive the same no-Start router rig as the birth/maintenance handler
// tests (newRouterRig in handlers_test.go).

// postJSONRig marshals body and POSTs it to the rig with the bearer token.
func postJSONRig(t *testing.T, r *routerRig, path string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, r.server.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func validSHA() string { return strings.Repeat("a", 64) }

// --- /perceive validation -----------------------------------------

func TestHandlerPerceive_RejectsBadSHA(t *testing.T) {
	r := newRouterRig(t)
	resp := postJSONRig(t, r, "/api/brain/perceive", PerceiveRequest{SHA: "not-hex", Title: "x", Body: "y"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandlerPerceive_RejectsMissingTitleBody(t *testing.T) {
	r := newRouterRig(t)
	resp := postJSONRig(t, r, "/api/brain/perceive", PerceiveRequest{SHA: validSHA(), Title: "", Body: "x"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for missing title", resp.StatusCode)
	}
}

func TestHandlerPerceive_RejectsBadJSON(t *testing.T) {
	r := newRouterRig(t)
	req, _ := http.NewRequest(http.MethodPost, r.server.URL+"/api/brain/perceive",
		bytes.NewReader([]byte("{not json")))
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for malformed JSON", resp.StatusCode)
	}
}

// --- /learn validation --------------------------------------------

func TestHandlerLearn_RejectsBadSHA(t *testing.T) {
	r := newRouterRig(t)
	resp := postJSONRig(t, r, "/api/brain/learn", LearnRequest{SHA: "nope", Title: "x", Body: "y"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandlerLearn_RejectsMissingTitleBody(t *testing.T) {
	r := newRouterRig(t)
	resp := postJSONRig(t, r, "/api/brain/learn", LearnRequest{SHA: validSHA(), Title: "T", Body: ""})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for missing body", resp.StatusCode)
	}
}

// --- /attach validation -------------------------------------------

func TestHandlerAttach_RejectsBadSHA(t *testing.T) {
	r := newRouterRig(t)
	resp := postJSONRig(t, r, "/api/brain/attach", AttachRequest{
		SHA: "short", OriginalFilename: "x.txt", BytesB64: "eA==",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandlerAttach_RejectsMissingFilename(t *testing.T) {
	r := newRouterRig(t)
	resp := postJSONRig(t, r, "/api/brain/attach", AttachRequest{
		SHA: validSHA(), OriginalFilename: "", BytesB64: "eA==",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for missing filename", resp.StatusCode)
	}
}

func TestHandlerAttach_RejectsSHAMismatch(t *testing.T) {
	r := newRouterRig(t)
	// validSHA() is not the real hash of "x" → mismatch is a 400.
	resp := postJSONRig(t, r, "/api/brain/attach", AttachRequest{
		SHA: validSHA(), OriginalFilename: "x.txt", BytesB64: "eA==", // base64("x")
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 on sha mismatch", resp.StatusCode)
	}
}

// --- /trace validation --------------------------------------------

func TestHandlerTrace_RejectsMissingKindMessage(t *testing.T) {
	r := newRouterRig(t)
	resp := postJSONRig(t, r, "/api/brain/trace", TraceRequest{Kind: "", Message: ""})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for missing kind/message", resp.StatusCode)
	}
}
