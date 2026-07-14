package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

const (
	recordsDefaultLimit = 100
	recordsMaxLimit     = 500
)

// RecordDTO is one record on the wire for the mart-projection endpoint
// (GET /api/brain/records). It carries the FULL record — body included —
// so a bulk consumer (pbrainctl mart) never has to do an O(N) follow-up
// /fetch per row. Body is the distilled (synthesised) body, falling back
// to raw_body, matching handleFetch. UpdatedAt is carried so the future
// mart-daemon change-feed can cursor on it without an endpoint change.
type RecordDTO struct {
	SHA         string     `json:"sha"`
	Kind        string     `json:"kind"`
	MemoryType  string     `json:"memory_type,omitempty"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	SourceURL   string     `json:"source_url,omitempty"`
	Source      []string   `json:"source,omitempty"`
	Tags        []string   `json:"tags,omitempty"`
	Topic       string     `json:"topic,omitempty"`
	Reliability string     `json:"reliability,omitempty"`
	CapturedAt  *time.Time `json:"captured_at,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// Attachment metadata — populated for kind=attachment_stub records so a
	// consumer (pbrainctl mart) can materialize the MinIO blob. The bytes
	// themselves are fetched separately via GET /api/brain/attach/{sha}.
	OriginalFilename string `json:"original_filename,omitempty"`
	MimeType         string `json:"mime_type,omitempty"`
	SizeBytes        int64  `json:"size_bytes,omitempty"`
}

// ListRecordsResponse is the 200 body of GET /api/brain/records. NextAfterID
// is the keyset cursor for the next page; it is 0 when the page was not full
// (end of stream), which is how the client loop terminates.
type ListRecordsResponse struct {
	Records     []RecordDTO `json:"records"`
	NextAfterID int64       `json:"next_after_id"`
}

// handleListRecords serves the generic, keyset-paginated enumeration of a
// tenant's records — the read that powers `pbrainctl mart build`. The
// (profile, vault) scope is derived from the bearer token like every other
// authed route; filters arrive as query params (repeated keys for the
// multi-valued facets). Read-only; the core stays ignorant of marts.
//
// Status contract mirrors handleFetch:
//   - 400 BAD_REQUEST           — malformed after_id / limit / synthesised
//   - 401 INVALID_TOKEN         — no binding on context
//   - 503 STORAGE_BACKEND_ERROR — Postgres not enabled for this binding
//   - 502 STORAGE_BACKEND_ERROR — query failed
func (d *Daemon) handleListRecords(w http.ResponseWriter, r *http.Request) {
	binding, ok := BindingFromContext(r.Context())
	if !ok {
		WriteErrorEnvelope(w, http.StatusUnauthorized, ErrCodeInvalidToken,
			"missing binding on request context", nil)
		return
	}

	q := r.URL.Query()

	afterID := int64(0)
	if s := strings.TrimSpace(q.Get("after_id")); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v < 0 {
			WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "after_id must be a non-negative integer", nil)
			return
		}
		afterID = v
	}

	limit := recordsDefaultLimit
	if s := strings.TrimSpace(q.Get("limit")); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v <= 0 {
			WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "limit must be a positive integer", nil)
			return
		}
		limit = v
	}
	if limit > recordsMaxLimit {
		limit = recordsMaxLimit
	}

	// synthesised defaults true — marts render distilled bodies. An explicit
	// synthesised=false lets a caller project the pre-synth backlog.
	synthesised := true
	if s := strings.TrimSpace(q.Get("synthesised")); s != "" {
		v, err := strconv.ParseBool(s)
		if err != nil {
			WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "synthesised must be a boolean", nil)
			return
		}
		synthesised = v
	}

	// topic is single-valued on the wire but the query takes a slice; wrap it.
	var topics []string
	if t := strings.TrimSpace(q.Get("topic")); t != "" {
		topics = []string{t}
	}

	view, ok := d.resolvePGOrError(w, binding, "records enumeration")
	if !ok {
		return
	}

	recs, err := pgstore.New(view.Pool()).ListRecords(r.Context(), pgdb.ListRecordsParams{
		Profile:       binding.Key.Profile,
		Vault:         binding.Key.Vault,
		Synthesised:   synthesised,
		Kinds:         q["kind"],
		Topics:        topics,
		Reliabilities: q["reliability"],
		TagsAny:       q["tag"],
		SourceAny:     q["source"],
		AfterID:       afterID,
		Lim:           int32(limit),
	})
	if err != nil {
		d.Logger.Error("phantom-brain: list records query failed", slog.String("err", err.Error()))
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"list records query failed: "+err.Error(), nil)
		return
	}

	resp := ListRecordsResponse{Records: make([]RecordDTO, 0, len(recs))}
	for _, rec := range recs {
		body := strings.TrimSpace(rec.Body.String)
		if body == "" {
			body = rec.RawBody.String
		}
		dto := RecordDTO{
			SHA:         rec.Sha,
			Kind:        rec.Kind,
			MemoryType:  rec.MemoryType.String,
			Title:       rec.Title,
			Body:        body,
			SourceURL:   rec.SourceUrl.String,
			Source:      rec.Source,
			Tags:        rec.Tags,
			Topic:       rec.Topic.String,
			Reliability: rec.Reliability.String,
		}
		if rec.CapturedAt.Valid {
			t := rec.CapturedAt.Time
			dto.CapturedAt = &t
		}
		if rec.UpdatedAt.Valid {
			dto.UpdatedAt = rec.UpdatedAt.Time
		}
		dto.OriginalFilename = rec.OriginalFilename.String
		dto.MimeType = rec.MimeType.String
		if rec.SizeBytes.Valid {
			dto.SizeBytes = rec.SizeBytes.Int64
		}
		resp.Records = append(resp.Records, dto)
	}
	// A full page implies there may be more; a short page is end-of-stream.
	if len(recs) == limit && len(recs) > 0 {
		resp.NextAfterID = recs[len(recs)-1].ID
	}

	writeJSON(w, http.StatusOK, resp)
}
