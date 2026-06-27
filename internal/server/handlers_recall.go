package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/neverprepared/phantom-brain/internal/osproject"
)

// recallDefaultLimit / recallMaxLimit bound the online recall page size,
// mirroring the agent-side brain_recall defaults so the daemon enforces
// the same contract regardless of caller.
const (
	recallDefaultLimit = 10
	recallMaxLimit     = 50
)

// RecallRequest is the JSON body of POST /api/brain/recall. Only `query`
// is required; embedding + the facet filters are optional. When the
// embedding is absent the Recaller falls back to BM25-only (degraded)
// mode. The (profile, vault) tenant scope is NOT in the body — it is
// derived from the bearer token's binding, same as every write handler.
type RecallRequest struct {
	Query       string    `json:"query"`
	Embedding   []float32 `json:"embedding,omitempty"`
	Limit       int       `json:"limit,omitempty"`
	Kinds       []string  `json:"kinds,omitempty"`
	Topic       string    `json:"topic,omitempty"`
	MemoryType  string    `json:"memory_type,omitempty"`
	Reliability []string  `json:"reliability,omitempty"`
}

// RecallHitDTO is one ranked result on the wire. It mirrors the agent's
// brain.RecallHitDTO field-for-field (identical JSON tags) — the wire
// type is intentionally duplicated per package, matching the
// PerceiveRequest/WriteResponse pattern.
type RecallHitDTO struct {
	SHA              string  `json:"sha"`
	Title            string  `json:"title"`
	Kind             string  `json:"kind"`
	MemoryType       string  `json:"memory_type,omitempty"`
	Topic            string  `json:"topic,omitempty"`
	Reliability      string  `json:"reliability,omitempty"`
	SourceURL        string  `json:"source_url,omitempty"`
	MimeType         string  `json:"mime_type,omitempty"`
	OriginalFilename string  `json:"original_filename,omitempty"`
	Snippet          string  `json:"snippet,omitempty"`
	Score            float64 `json:"score"`
}

// RecallResponse is the 200 body of POST /api/brain/recall.
type RecallResponse struct {
	Hits []RecallHitDTO `json:"hits"`
}

// handleRecall serves the always-online recall path (Phase C of the
// daemon cutover). It runs the binding's pb_records Recaller (Postgres
// projection, hybrid BM25+kNN) and returns fresh hits — no snapshot lag.
//
// Status-code contract (the agent treats ANY non-2xx as "fall back to
// the local snapshot", so the codes are advisory, not load-bearing):
//   - 400 BAD_REQUEST          — empty query / malformed JSON
//   - 503 STORAGE_BACKEND_ERROR — online recall not enabled for this
//     binding (Postgres not configured): resolvePG → ErrPostgresDisabled
//   - 500 STORAGE_BACKEND_ERROR — other binding-resolution failure
//     (canonicalised via resolvePGOrError; was INTERNAL_ERROR pre-audit-D)
//   - 502 STORAGE_BACKEND_ERROR — Recaller (OpenSearch) query failed
func (d *Daemon) handleRecall(w http.ResponseWriter, r *http.Request) {
	binding, ok := BindingFromContext(r.Context())
	if !ok {
		// Auth middleware guarantees the binding is present; this is a
		// defensive 401 for the unauthed-route-misconfiguration case.
		WriteErrorEnvelope(w, http.StatusUnauthorized, ErrCodeInvalidToken,
			"missing binding on request context", nil)
		return
	}

	var req RecallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "invalid JSON body", nil)
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "query required", nil)
		return
	}

	view, ok := d.resolvePGOrError(w, binding, "online recall")
	if !ok {
		return
	}

	limit := req.Limit
	if limit <= 0 {
		limit = recallDefaultLimit
	}
	if limit > recallMaxLimit {
		limit = recallMaxLimit
	}

	q := osproject.RecallQuery{
		Profile:     binding.Key.Profile,
		Vault:       binding.Key.Vault,
		Text:        query,
		Vector:      req.Embedding,
		Kinds:       req.Kinds,
		Topic:       req.Topic,
		MemoryType:  req.MemoryType,
		Reliability: req.Reliability,
		Size:        limit,
	}
	hits, err := view.Recaller().Recall(r.Context(), q)
	if err != nil {
		d.Logger.Error("phantom-brain: recall query failed", slog.String("err", err.Error()))
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"recall query failed: "+err.Error(), nil)
		return
	}

	resp := RecallResponse{Hits: make([]RecallHitDTO, 0, len(hits))}
	for _, h := range hits {
		resp.Hits = append(resp.Hits, RecallHitDTO{
			SHA:              h.SHA,
			Title:            h.Title,
			Kind:             h.Kind,
			MemoryType:       h.MemoryType,
			Topic:            h.Topic,
			Reliability:      h.Reliability,
			SourceURL:        h.SourceURL,
			MimeType:         h.MimeType,
			OriginalFilename: h.OriginalFilename,
			Snippet:          h.Snippet,
			Score:            h.Score,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}
