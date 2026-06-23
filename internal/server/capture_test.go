package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureStore is a minimal AttachmentStore for capture tests —
// records the bytes that landed and returns a deterministic key.
type captureStore struct {
	mu     sync.Mutex
	puts   map[string][]byte
	puttCT map[string]string
	failPut bool
}

func newCaptureStore() *captureStore {
	return &captureStore{puts: map[string][]byte{}, puttCT: map[string]string{}}
}

func (s *captureStore) PutAttachment(ctx context.Context, profile, vault, sha, ext string, body []byte, ct string) (string, error) {
	return s.PutAttachmentWithTags(ctx, profile, vault, sha, ext, body, ct, nil)
}

func (s *captureStore) PutAttachmentWithTags(_ context.Context, profile, vault, sha, ext string, body []byte, ct string, _ []string) (string, error) {
	if s.failPut {
		return "", errors.New("fake put fail")
	}
	key := fmt.Sprintf("%s/%s/attachments/%s%s", profile, vault, sha, ext)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts[key] = append([]byte(nil), body...)
	s.puttCT[key] = ct
	return key, nil
}

func (s *captureStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://example.test/" + key + "?sig=fake", nil
}

func (s *captureStore) GetAttachmentBytes(_ context.Context, key string, _ int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.puts[key]
	if !ok {
		return nil, errors.New("no such key: " + key)
	}
	return append([]byte(nil), b...), nil
}

func TestCaptureURL_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html><body>hello world</body></html>"))
	}))
	defer server.Close()
	store := newCaptureStore()
	res, err := CaptureURL(context.Background(), store, "p", "v", "deadbeef",
		server.URL, 0, "", 5*time.Second)
	if err != nil {
		t.Fatalf("CaptureURL: %v", err)
	}
	if !strings.HasSuffix(res.Key, "captures-deadbeef.html") {
		t.Errorf("key suffix mismatch: %q", res.Key)
	}
	if res.SizeBytes == 0 {
		t.Error("size 0; expected non-zero")
	}
	if !strings.HasPrefix(res.ContentType, "text/html") {
		t.Errorf("content type = %q", res.ContentType)
	}
	if got := store.puts[res.Key]; len(got) != int(res.SizeBytes) {
		t.Errorf("stored bytes len = %d, want %d", len(got), res.SizeBytes)
	}
}

func TestCaptureURL_RespectsMaxBytes(t *testing.T) {
	// Serve 2 KB; cap at 100 bytes; should error rather than truncate.
	body := strings.Repeat("a", 2048)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer server.Close()
	store := newCaptureStore()
	_, err := CaptureURL(context.Background(), store, "p", "v", "x",
		server.URL, 100, "", 5*time.Second)
	if err == nil {
		t.Fatal("expected oversize error")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("err = %v; want exceeded-byte-cap message", err)
	}
	// Nothing should have landed in the store on oversize rejection.
	if len(store.puts) != 0 {
		t.Errorf("oversize put should not write to store; got %d entries", len(store.puts))
	}
}

func TestCaptureURL_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer server.Close()
	_, err := CaptureURL(context.Background(), newCaptureStore(), "p", "v", "x",
		server.URL, 0, "", 5*time.Second)
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v; want 403 in message", err)
	}
}

func TestCaptureURL_NoStore(t *testing.T) {
	_, err := CaptureURL(context.Background(), nil, "p", "v", "x", "https://x.test", 0, "", 5*time.Second)
	if err == nil {
		t.Fatal("expected error when AttachmentStore is nil")
	}
}

func TestCaptureURL_StoreFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer server.Close()
	store := newCaptureStore()
	store.failPut = true
	_, err := CaptureURL(context.Background(), store, "p", "v", "x",
		server.URL, 0, "", 5*time.Second)
	if err == nil {
		t.Fatal("expected error when store put fails")
	}
}

func TestCaptureURL_UserAgentSent(t *testing.T) {
	gotUA := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Write([]byte("x"))
	}))
	defer server.Close()
	_, err := CaptureURL(context.Background(), newCaptureStore(), "p", "v", "x",
		server.URL, 0, "phantom-test/0", 5*time.Second)
	if err != nil {
		t.Fatalf("CaptureURL: %v", err)
	}
	if gotUA != "phantom-test/0" {
		t.Errorf("User-Agent header = %q", gotUA)
	}
}

func TestExtFromContentType(t *testing.T) {
	cases := map[string]string{
		"text/html":                     ".html",
		"text/html; charset=utf-8":      ".html",
		"application/json; charset=ascii": ".json",
		"application/xhtml+xml":         ".html",
		"text/markdown":                 ".md",
		"application/pdf":               ".pdf",
		"":                              ".bin",
		"application/zip":               ".bin",
		"TEXT/HTML":                     ".html", // case-insensitive
	}
	for in, want := range cases {
		if got := extFromContentType(in); got != want {
			t.Errorf("extFromContentType(%q) = %q, want %q", in, got, want)
		}
	}
}
