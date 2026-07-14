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
// (end of stream), which is how the client loop terminates. NextSince is the
// updated_at half of the compound cursor, set only in change-feed mode
// (?since=...) — the caller persists (NextSince, NextAfterID) as its cursor.
type ListRecordsResponse struct {
	Records     []RecordDTO `json:"records"`
	NextAfterID int64       `json:"next_after_id"`
	NextSince   string      `json:"next_since,omitempty"`
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

	// Optional change-feed mode: ?since=<RFC3339> switches from the id-keyset
	// full enumeration (ListRecords) to the (updated_at, id) compound-keyset
	// change feed (ListRecordsSince). after_id is the id half of the cursor in
	// both modes.
	var (
		since     time.Time
		sinceMode bool
	)
	if s := strings.TrimSpace(q.Get("since")); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "since must be an RFC3339 timestamp", nil)
			return
		}
		since, sinceMode = t, true
	}

	view, ok := d.resolvePGOrError(w, binding, "records enumeration")
	if !ok {
		return
	}

	store := pgstore.New(view.Pool())
	var recs []pgdb.Record
	var err error
	if sinceMode {
		recs, err = store.ListRecordsSince(r.Context(), pgdb.ListRecordsSinceParams{
			Profile:       binding.Key.Profile,
			Vault:         binding.Key.Vault,
			Synthesised:   synthesised,
			Kinds:         q["kind"],
			Topics:        topics,
			Reliabilities: q["reliability"],
			TagsAny:       q["tag"],
			SourceAny:     q["source"],
			Since:         pgstore.OptTimestamptz(&since),
			AfterID:       afterID,
			Lim:           int32(limit),
		})
	} else {
		recs, err = store.ListRecords(r.Context(), pgdb.ListRecordsParams{
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
	}
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
	if len(recs) > 0 {
		last := recs[len(recs)-1]
		if sinceMode {
			// Change feed: always echo the last row's (updated_at, id) so the
			// caller can persist it as its resume cursor even on the final
			// short page. The caller detects end-of-stream by len < limit, not
			// by a zero cursor.
			resp.NextAfterID = last.ID
			if last.UpdatedAt.Valid {
				resp.NextSince = last.UpdatedAt.Time.Format(time.RFC3339Nano)
			}
		} else if len(recs) == limit {
			// id-keyset mode: a full page implies more; a short page ends the
			// stream (NextAfterID stays 0). Build's loop depends on this.
			resp.NextAfterID = last.ID
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
