package brain

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neverprepared/phantom-brain/internal/brain/wqueue"
)

// programmableServer is a daemon stand-in whose response code per
// path can be flipped at runtime. All write endpoints return the
// same code; the body is a fixed accept envelope on 2xx.
type programmableServer struct {
	code     atomic.Int32 // http status to return
	hits     atomic.Int32 // total write requests received
	attachOK atomic.Int32 // attach with non-empty bytes_b64
}

func (p *programmableServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/brain/perceive", p.write)
	mux.HandleFunc("/api/brain/learn", p.write)
	mux.HandleFunc("/api/brain/attach", p.writeAttach)
	mux.HandleFunc("/api/brain/trace", p.write)
	return mux
}

func (p *programmableServer) write(w http.ResponseWriter, r *http.Request) {
	p.hits.Add(1)
	code := int(p.code.Load())
	if code == 0 {
		code = http.StatusAccepted
	}
	w.WriteHeader(code)
	if code >= 200 && code < 300 {
		_, _ = io.WriteString(w, `{"sha":"x","indexed_at":1,"synth_enqueued":true}`)
	}
}

func (p *programmableServer) writeAttach(w http.ResponseWriter, r *http.Request) {
	p.hits.Add(1)
	var req AttachRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.BytesB64 != "" {
		p.attachOK.Add(1)
	}
	code := int(p.code.Load())
	if code == 0 {
		code = http.StatusAccepted
	}
	w.WriteHeader(code)
	if code >= 200 && code < 300 {
		_, _ = io.WriteString(w, `{"sha":"x","indexed_at":1,"synth_enqueued":true}`)
	}
}

func newProgrammable(t *testing.T, code int) (*Client, *programmableServer) {
	t.Helper()
	p := &programmableServer{}
	p.code.Store(int32(code))
	srv := httptest.NewServer(p.handler())
	t.Cleanup(srv.Close)
	c, err := NewClient(ClientOpts{BaseURL: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	return c, p
}

func openQueue(t *testing.T) *wqueue.Queue {
	t.Helper()
	q, err := wqueue.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { q.Close() })
	return q
}

func TestDrainOnceSendsThenDeletes(t *testing.T) {
	q := openQueue(t)
	client, p := newProgrammable(t, http.StatusAccepted)
	conn := NewConnectivity()
	ctx := context.Background()
	for _, s := range []string{"a", "b", "c"} {
		body, _ := json.Marshal(PerceiveRequest{SHA: s, Title: s, Body: s})
		if _, err := q.Enqueue(ctx, wqueue.EnqueueOpts{
			Kind: wqueue.KindPerceive, SHA: s, PayloadJSON: body,
		}); err != nil {
			t.Fatal(err)
		}
	}
	sent, failed, err := DrainOnce(ctx, q, client, conn, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if sent != 3 || failed != 0 {
		t.Fatalf("sent=%d failed=%d, want 3, 0", sent, failed)
	}
	if p.hits.Load() != 3 {
		t.Fatalf("daemon hits = %d", p.hits.Load())
	}
	if n, _ := q.Depth(ctx); n != 0 {
		t.Fatalf("Depth = %d", n)
	}
	if conn.Snapshot().State != ConnOnline {
		t.Fatalf("connectivity = %s", conn.Snapshot().State)
	}
}

func TestDrainOnceFailureMarksAttempt(t *testing.T) {
	q := openQueue(t)
	client, _ := newProgrammable(t, http.StatusServiceUnavailable)
	conn := NewConnectivity()
	ctx := context.Background()
	body, _ := json.Marshal(LearnRequest{SHA: "x", Title: "x", Body: "x"})
	q.Enqueue(ctx, wqueue.EnqueueOpts{Kind: wqueue.KindLearn, SHA: "x", PayloadJSON: body})
	sent, failed, _ := DrainOnce(ctx, q, client, conn, slog.New(slog.DiscardHandler))
	if sent != 0 || failed != 1 {
		t.Fatalf("sent=%d failed=%d", sent, failed)
	}
	if n, _ := q.Depth(ctx); n != 1 {
		t.Fatalf("Depth = %d, want 1 (still queued)", n)
	}
	// Within backoff window, next pass should send nothing.
	sent, failed, _ = DrainOnce(ctx, q, client, conn, slog.New(slog.DiscardHandler))
	if sent != 0 || failed != 0 {
		t.Fatalf("within backoff: sent=%d failed=%d", sent, failed)
	}
	// Connectivity should remain offline (never had a prior success).
	if conn.Snapshot().State != ConnOffline {
		t.Fatalf("connectivity = %s, want offline (no prior success)", conn.Snapshot().State)
	}
}

func TestDrainOnceAttachRereadsBytes(t *testing.T) {
	q := openQueue(t)
	client, p := newProgrammable(t, http.StatusAccepted)
	ctx := context.Background()
	blob := []byte("the pdf bytes")
	body, _ := json.Marshal(AttachRequest{SHA: "att", OriginalFilename: "x.pdf"})
	if _, err := q.Enqueue(ctx, wqueue.EnqueueOpts{
		Kind: wqueue.KindAttach, SHA: "att", PayloadJSON: body, Bytes: blob, Ext: ".pdf",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DrainOnce(ctx, q, client, NewConnectivity(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if p.attachOK.Load() != 1 {
		t.Fatalf("attach POST missing bytes_b64")
	}
	if n, _ := q.Depth(ctx); n != 0 {
		t.Fatalf("Depth = %d", n)
	}
}

func TestDrainOnceIdempotentOnRedrain(t *testing.T) {
	// Simulates a crash mid-drain: row left behind after a successful
	// POST. Re-draining hits the daemon again (daemon dedups by SHA);
	// row deletes on the new success.
	q := openQueue(t)
	client, p := newProgrammable(t, http.StatusAccepted)
	ctx := context.Background()
	body, _ := json.Marshal(PerceiveRequest{SHA: "dup", Title: "t", Body: "b"})
	q.Enqueue(ctx, wqueue.EnqueueOpts{Kind: wqueue.KindPerceive, SHA: "dup", PayloadJSON: body})
	DrainOnce(ctx, q, client, NewConnectivity(), slog.New(slog.DiscardHandler))
	// Re-enqueue same SHA (simulates row that survived the crash).
	q.Enqueue(ctx, wqueue.EnqueueOpts{Kind: wqueue.KindPerceive, SHA: "dup", PayloadJSON: body})
	DrainOnce(ctx, q, client, NewConnectivity(), slog.New(slog.DiscardHandler))
	if p.hits.Load() != 2 {
		t.Fatalf("expected 2 daemon hits, got %d", p.hits.Load())
	}
	if n, _ := q.Depth(ctx); n != 0 {
		t.Fatalf("Depth = %d", n)
	}
}

// TestDrainOncePermanent4xxDeadLetters proves a daemon 4xx rejection is
// classified PERMANENT and dead-lettered immediately (not retried forever).
func TestDrainOncePermanent4xxDeadLetters(t *testing.T) {
	q := openQueue(t)
	client, _ := newProgrammable(t, http.StatusBadRequest)
	conn := NewConnectivity()
	ctx := context.Background()
	body, _ := json.Marshal(PerceiveRequest{SHA: "bad", Title: "t", Body: "b"})
	q.Enqueue(ctx, wqueue.EnqueueOpts{Kind: wqueue.KindPerceive, SHA: "bad", PayloadJSON: body})

	sent, failed, _ := DrainOnce(ctx, q, client, conn, slog.New(slog.DiscardHandler))
	if sent != 0 || failed != 1 {
		t.Fatalf("sent=%d failed=%d, want 0,1", sent, failed)
	}
	dead, err := q.ListDead(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 1 || !dead[0].Dead || dead[0].DeadReason == "" {
		t.Fatalf("expected 1 dead row with reason, got %+v", dead)
	}
	// Not retried: a second pass selects nothing (dead excluded from NextEligible).
	sent, failed, _ = DrainOnce(ctx, q, client, conn, slog.New(slog.DiscardHandler))
	if sent != 0 || failed != 0 {
		t.Fatalf("dead row was retried: sent=%d failed=%d", sent, failed)
	}
}

// TestDrainOncePermanentUnmarshalDeadLetters proves a corrupt payload that
// will never unmarshal is dead-lettered immediately.
func TestDrainOncePermanentUnmarshalDeadLetters(t *testing.T) {
	q := openQueue(t)
	client, p := newProgrammable(t, http.StatusAccepted)
	ctx := context.Background()
	// Corrupt JSON — dispatch's json.Unmarshal fails before any POST.
	q.Enqueue(ctx, wqueue.EnqueueOpts{Kind: wqueue.KindPerceive, SHA: "corrupt", PayloadJSON: []byte("{not valid json")})

	sent, failed, _ := DrainOnce(ctx, q, client, NewConnectivity(), slog.New(slog.DiscardHandler))
	if sent != 0 || failed != 1 {
		t.Fatalf("sent=%d failed=%d, want 0,1", sent, failed)
	}
	if p.hits.Load() != 0 {
		t.Fatalf("corrupt payload should never reach the daemon, hits=%d", p.hits.Load())
	}
	dead, _ := q.ListDead(ctx, 0)
	if len(dead) != 1 {
		t.Fatalf("expected 1 dead row, got %d", len(dead))
	}
}

// TestDrainOnceMaxAttemptsDeadLetters proves a row that exhausts
// MaxAttempts on transient failures is finally dead-lettered.
func TestDrainOnceMaxAttemptsDeadLetters(t *testing.T) {
	q := openQueue(t)
	client, _ := newProgrammable(t, http.StatusServiceUnavailable) // 5xx ⇒ transient
	ctx := context.Background()
	body, _ := json.Marshal(LearnRequest{SHA: "tired", Title: "t", Body: "b"})
	it, err := q.Enqueue(ctx, wqueue.EnqueueOpts{Kind: wqueue.KindLearn, SHA: "tired", PayloadJSON: body})
	if err != nil {
		t.Fatal(err)
	}
	// Burn attempts up to one shy of the cap, using an old timestamp so the
	// backoff window is always elapsed (the row stays eligible).
	old := time.Unix(0, 0)
	for i := 0; i < wqueue.MaxAttempts-1; i++ {
		if err := q.MarkAttempt(ctx, it.ID, old, nil); err != nil {
			t.Fatal(err)
		}
	}
	// The next transient failure crosses the cap ⇒ dead.
	sent, failed, _ := DrainOnce(ctx, q, client, NewConnectivity(), slog.New(slog.DiscardHandler))
	if sent != 0 || failed != 1 {
		t.Fatalf("sent=%d failed=%d, want 0,1", sent, failed)
	}
	dead, _ := q.ListDead(ctx, 0)
	if len(dead) != 1 {
		t.Fatalf("expected row dead-lettered at MaxAttempts, got %d dead", len(dead))
	}
}

// TestDrainOnceTransientRetriesNotDead proves a single transient failure is
// retried (attempt bumped, stays live), NOT dead-lettered.
func TestDrainOnceTransientRetriesNotDead(t *testing.T) {
	q := openQueue(t)
	client, _ := newProgrammable(t, http.StatusServiceUnavailable)
	ctx := context.Background()
	body, _ := json.Marshal(LearnRequest{SHA: "later", Title: "t", Body: "b"})
	q.Enqueue(ctx, wqueue.EnqueueOpts{Kind: wqueue.KindLearn, SHA: "later", PayloadJSON: body})

	DrainOnce(ctx, q, client, NewConnectivity(), slog.New(slog.DiscardHandler))
	dead, _ := q.ListDead(ctx, 0)
	if len(dead) != 0 {
		t.Fatalf("transient failure should not dead-letter, got %d dead", len(dead))
	}
	all, _ := q.List(ctx, 0)
	if len(all) != 1 || all[0].Dead || all[0].Attempts != 1 {
		t.Fatalf("expected 1 live row with attempts=1, got %+v", all)
	}
}

func TestDrainerNilGuards(t *testing.T) {
	// nil queue / nil client are no-ops, not panics.
	sent, failed, err := DrainOnce(context.Background(), nil, nil, nil, nil)
	if err != nil || sent != 0 || failed != 0 {
		t.Fatalf("nil-guard failed: %d %d %v", sent, failed, err)
	}
}

func TestDispatchUnknownKind(t *testing.T) {
	client, _ := newProgrammable(t, http.StatusAccepted)
	err := dispatch(context.Background(), client, &wqueue.Item{Kind: "bogus", SHA: "x"})
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestDrainerStateTransitionFailureThenSuccess(t *testing.T) {
	q := openQueue(t)
	client, p := newProgrammable(t, http.StatusServiceUnavailable)
	conn := NewConnectivity()
	conn.NoteSuccess(time.Now()) // pretend we were online once
	ctx := context.Background()
	body, _ := json.Marshal(LearnRequest{SHA: "r", Title: "r", Body: "r"})
	q.Enqueue(ctx, wqueue.EnqueueOpts{Kind: wqueue.KindLearn, SHA: "r", PayloadJSON: body})
	DrainOnce(ctx, q, client, conn, slog.New(slog.DiscardHandler))
	if conn.Snapshot().State != ConnDegraded {
		t.Fatalf("after failure state = %s, want degraded", conn.Snapshot().State)
	}
	// Flip server to OK and enqueue a fresh row (skips the backoff
	// window). Recovery should land state back at ConnOnline.
	p.code.Store(int32(http.StatusAccepted))
	body2, _ := json.Marshal(LearnRequest{SHA: "r2", Title: "r2", Body: "r2"})
	q.Enqueue(ctx, wqueue.EnqueueOpts{Kind: wqueue.KindLearn, SHA: "r2", PayloadJSON: body2})
	DrainOnce(ctx, q, client, conn, slog.New(slog.DiscardHandler))
	if conn.Snapshot().State != ConnOnline {
		t.Fatalf("after recovery state = %s, want online", conn.Snapshot().State)
	}
}
