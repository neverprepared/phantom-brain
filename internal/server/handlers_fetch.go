package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// FetchDTO is the 200 body of GET /api/brain/fetch/{sha}. It carries the
// full (untruncated) stored document so the agent's brain_fetch can render
// the whole thing. Body is the distilled (synthesised) body, falling back
// to raw_body when distill hasn't run / produced nothing — there is always
// SOMETHING to show for a record that exists.
type FetchDTO struct {
	SHA        string   `json:"sha"`
	Title      string   `json:"title"`
	Kind       string   `json:"kind"`
	SourcePath string   `json:"source_path,omitempty"`
	SourceURL  string   `json:"source_url,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Body       string   `json:"body"`
}

// handleFetch serves the always-online brain_fetch retrieval path (Phase
// D2a). It reads one record from the binding's Postgres SoR by
// (profile, vault, sha) and returns its full body — the read companion to
// handleRecall. Mirrors handleRecall's status-code contract:
//   - 400 BAD_REQUEST           — malformed SHA
//   - 404 NOT_FOUND             — no record for that SHA
//   - 503 STORAGE_BACKEND_ERROR — Postgres not enabled for this binding
//   - 500 STORAGE_BACKEND_ERROR — other binding-resolution failure
//     (canonicalised via resolvePGOrError; was INTERNAL_ERROR pre-audit-D)
//   - 502 STORAGE_BACKEND_ERROR — Postgres query failed
func (d *Daemon) handleFetch(w http.ResponseWriter, r *http.Request) {
	binding, ok := BindingFromContext(r.Context())
	if !ok {
		WriteErrorEnvelope(w, http.StatusUnauthorized, ErrCodeInvalidToken,
			"missing binding on request context", nil)
		return
	}

	sha := chi.URLParam(r, "sha")
	if err := validateSHA(sha); err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, err.Error(), nil)
		return
	}

	view, ok := d.resolvePGOrError(w, binding, "online fetch")
	if !ok {
		return
	}

	rec, err := d.getRecordBySHA(r.Context(), view, binding, sha)
	if err != nil {
		if errors.Is(err, errRecordNotFound) {
			WriteErrorEnvelope(w, http.StatusNotFound, ErrCodeNotFound, "no document with that SHA", nil)
			return
		}
		d.Logger.Error("phantom-brain: fetch query failed", slog.String("err", err.Error()))
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"fetch query failed: "+err.Error(), nil)
		return
	}

	body := strings.TrimSpace(rec.Body.String)
	if body == "" {
		body = rec.RawBody.String
	}

	writeJSON(w, http.StatusOK, FetchDTO{
		SHA:        rec.Sha,
		Title:      rec.Title,
		Kind:       rec.Kind,
		SourcePath: "", // SoR has no on-disk path; reserved for parity with the snapshot shape.
		SourceURL:  rec.SourceUrl.String,
		Tags:       rec.Tags,
		Body:       body,
	})
}
