package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neverprepared/mcp-phantom-brain/internal/osearch"
)

func osReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// --- in-memory fakes ----------------------------------------------

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

type fakeAttach struct {
	mu     sync.Mutex
	blobs  map[string][]byte
	failPut bool
}

func newFakeAttach() *fakeAttach { return &fakeAttach{blobs: map[string][]byte{}} }

func (f *fakeAttach) PutAttachment(_ context.Context, profile, vault, sha, ext string, body []byte, _ string) (string, error) {
	if f.failPut {
		return "", errors.New("fake: put failed")
	}
	key := fmt.Sprintf("%s/%s/attachments/%s%s", profile, vault, sha, ext)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blobs[key] = append([]byte(nil), body...)
	return key, nil
}
func (f *fakeAttach) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://example.test/" + key + "?sig=fake", nil
}

type fakeSynth struct {
	mu    sync.Mutex
	calls []string // "profile:vault:sha"
}

func (q *fakeSynth) Enqueue(profile, vault, sha string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls = append(q.calls, fmt.Sprintf("%s:%s:%s", profile, vault, sha))
}
func (q *fakeSynth) snapshot() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, len(q.calls))
	copy(out, q.calls)
	return out
}

// --- test rig ------------------------------------------------------

type writeRig struct {
	d       *Daemon
	url     string
	token   string
	os      *fakeOS
	attach  *fakeAttach
	synth   *fakeSynth
	cleanup func()
}

func startWriteRig(t *testing.T) *writeRig {
	t.Helper()
	d, url, token, cleanup := startTestRig(t)
	os := newFakeOS()
	attach := newFakeAttach()
	synth := &fakeSynth{}
	d.osClient = os
	d.attach = attach
	d.synth = synth
	return &writeRig{d: d, url: url, token: token, os: os, attach: attach, synth: synth, cleanup: cleanup}
}

func (r *writeRig) post(t *testing.T, path string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, r.url+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func validSHA() string {
	return strings.Repeat("a", 64)
}

// --- /perceive ----------------------------------------------------

func TestHandlerPerceive_HappyPath(t *testing.T) {
	r := startWriteRig(t)
	defer r.cleanup()

	sha := validSHA()
	resp := r.post(t, "/api/brain/perceive", PerceiveRequest{
		SHA: sha, Title: "Hi", Body: "World",
		URL: "https://example.com", Tags: []string{"a"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	doc, ok := r.os.summaries[osearch.DocID("personal", "memory", sha)]
	if !ok {
		t.Fatal("OS doc not written")
	}
	if doc.Synthesised {
		t.Errorf("perceive should leave Synthesised=false")
	}
	if doc.SourceURL != "https://example.com" {
		t.Errorf("SourceURL = %q, want example.com", doc.SourceURL)
	}
	if len(r.synth.snapshot()) != 1 {
		t.Errorf("synth queue calls = %v, want 1", r.synth.snapshot())
	}
}

func TestHandlerPerceive_RejectsBadSHA(t *testing.T) {
	r := startWriteRig(t)
	defer r.cleanup()

	resp := r.post(t, "/api/brain/perceive", PerceiveRequest{
		SHA: "not-hex", Title: "x", Body: "y",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandlerPerceive_RejectsMissingTitleBody(t *testing.T) {
	r := startWriteRig(t)
	defer r.cleanup()

	resp := r.post(t, "/api/brain/perceive", PerceiveRequest{SHA: validSHA(), Title: "", Body: "x"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for missing title", resp.StatusCode)
	}
}

func TestHandlerPerceive_DisabledWithoutOS(t *testing.T) {
	d, url, token, cleanup := startTestRig(t)
	defer cleanup()
	d.osClient = nil // simulate [opensearch] absent

	body, _ := json.Marshal(PerceiveRequest{SHA: validSHA(), Title: "x", Body: "y"})
	req, _ := http.NewRequest(http.MethodPost, url+"/api/brain/perceive", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when OS unwired", resp.StatusCode)
	}
}

// --- /learn -------------------------------------------------------

func TestHandlerLearn_HappyPath(t *testing.T) {
	r := startWriteRig(t)
	defer r.cleanup()

	sha := validSHA()
	resp := r.post(t, "/api/brain/learn", LearnRequest{
		SHA: sha, Title: "T", Body: "B",
		Tags: []string{"curated"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	doc := r.os.summaries[osearch.DocID("personal", "memory", sha)]
	if doc.Reliability != osearch.ReliabilityMedium {
		t.Errorf("learn should set reliability=medium; got %q", doc.Reliability)
	}
	if doc.GateReason == "" {
		t.Error("learn should set GateReason")
	}
}

// --- /attach ------------------------------------------------------

func TestHandlerAttach_HappyPath(t *testing.T) {
	r := startWriteRig(t)
	defer r.cleanup()

	payload := []byte("hello, attachment bytes")
	sha := osearch.SHA256Hex(payload)
	resp := r.post(t, "/api/brain/attach", AttachRequest{
		SHA:              sha,
		OriginalFilename: "note.pdf",
		MIMEType:         "application/pdf",
		BytesB64:         base64.StdEncoding.EncodeToString(payload),
		ExtractedText:    "extracted text",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}

	doc := r.os.attachments[osearch.DocID("personal", "memory", sha)]
	if doc.SHA != sha {
		t.Errorf("doc SHA = %q", doc.SHA)
	}
	if doc.SizeBytes != int64(len(payload)) {
		t.Errorf("SizeBytes = %d, want %d", doc.SizeBytes, len(payload))
	}
	wantKey := "personal/memory/attachments/" + sha + ".pdf"
	if doc.MinIOKey != wantKey {
		t.Errorf("MinIOKey = %q, want %q", doc.MinIOKey, wantKey)
	}
	stored, ok := r.attach.blobs[wantKey]
	if !ok || !bytes.Equal(stored, payload) {
		t.Errorf("blob not stored or content mismatch (got %d bytes)", len(stored))
	}
}

func TestHandlerAttach_RejectsSHAMismatch(t *testing.T) {
	r := startWriteRig(t)
	defer r.cleanup()

	bad := validSHA() // not the real hash of "x"
	resp := r.post(t, "/api/brain/attach", AttachRequest{
		SHA: bad, OriginalFilename: "x.txt",
		BytesB64: base64.StdEncoding.EncodeToString([]byte("x")),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 on sha mismatch", resp.StatusCode)
	}
}

// --- /attach/{sha} ------------------------------------------------

func TestHandlerAttachGet_PresignsExisting(t *testing.T) {
	r := startWriteRig(t)
	defer r.cleanup()

	// Seed via the handler so MinIO key + OS doc both exist.
	payload := []byte("contents")
	sha := osearch.SHA256Hex(payload)
	resp := r.post(t, "/api/brain/attach", AttachRequest{
		SHA: sha, OriginalFilename: "x.bin",
		BytesB64: base64.StdEncoding.EncodeToString(payload),
	})
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, r.url+"/api/brain/attach/"+sha, nil)
	req.Header.Set("Authorization", "Bearer "+r.token)
	getResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(getResp.Body)
		t.Fatalf("status = %d body=%s", getResp.StatusCode, b)
	}
	var body map[string]any
	_ = json.NewDecoder(getResp.Body).Decode(&body)
	url, _ := body["url"].(string)
	if !strings.HasPrefix(url, "https://example.test/personal/memory/attachments/") {
		t.Errorf("unexpected presigned URL: %q", url)
	}
}

func TestHandlerAttachGet_404OnMissing(t *testing.T) {
	r := startWriteRig(t)
	defer r.cleanup()

	req, _ := http.NewRequest(http.MethodGet, r.url+"/api/brain/attach/"+validSHA(), nil)
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// --- /trace -------------------------------------------------------

func TestHandlerTrace_AppendsLog(t *testing.T) {
	r := startWriteRig(t)
	defer r.cleanup()

	resp := r.post(t, "/api/brain/trace", TraceRequest{
		Kind: "test", Message: "hello", Meta: map[string]any{"x": 1},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	logPath := filepath.Join(r.d.DataDir.VaultDir("personal", "memory"), "Wiki", "_log.md")
	got := readFile(t, logPath)
	if !strings.Contains(got, `"kind":"test"`) {
		t.Errorf("log missing kind=test: %s", got)
	}
	if !strings.Contains(got, `"message":"hello"`) {
		t.Errorf("log missing message: %s", got)
	}
}

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

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := osReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
