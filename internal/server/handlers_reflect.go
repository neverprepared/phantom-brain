package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// brain_reflect / brain_forget HTTP surface (issue #72, Phase 1).
//
//   GET  /api/brain/reflect  — read-only report of forget-candidates.
//   POST /api/brain/forget   — delete one summary by SHA + trigger a
//                              snapshot rebuild so the forget propagates.
//
// Both go through resolveOS so per-binding storage overrides (v3.2)
// route to the right tenant's indices. Modeled on handlePerceive.

// ReflectResponse is the GET /api/brain/reflect envelope.
type ReflectResponse struct {
	Candidates []ReflectCandidate `json:"candidates"`
}

// ForgetRequest is the POST /api/brain/forget body.
type ForgetRequest struct {
	SHA string `json:"sha"`
}

// ForgetResponse is the POST /api/brain/forget envelope.
type ForgetResponse struct {
	SHA       string `json:"sha"`
	Forgotten bool   `json:"forgotten"`
}

func (d *Daemon) handleReflect(w http.ResponseWriter, r *http.Request) {
	if d.osClient == nil {
		WriteErrorEnvelope(w, http.StatusServiceUnavailable, ErrCodeStorageBackendErr,
			"opensearch not configured; reflect disabled", nil)
		return
	}
	binding, _ := BindingFromContext(r.Context())
	osc, err := d.resolveOS(binding)
	if err != nil {
		d.Logger.Error("phantom-brain: binding configuration error", slog.String("err", err.Error()))
		WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeStorageBackendErr, "binding configuration error", nil)
		return
	}

	candidates, err := staleGateCandidates(r.Context(), osc, binding.Key.Profile, binding.Key.Vault)
	if err != nil {
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"reflect scan failed: "+err.Error(), nil)
		return
	}
	// Never marshal nil — callers expect an empty array, not null.
	if candidates == nil {
		candidates = []ReflectCandidate{}
	}
	writeJSON(w, http.StatusOK, ReflectResponse{Candidates: candidates})
}

func (d *Daemon) handleForget(w http.ResponseWriter, r *http.Request) {
	if d.osClient == nil {
		WriteErrorEnvelope(w, http.StatusServiceUnavailable, ErrCodeStorageBackendErr,
			"opensearch not configured; forget disabled", nil)
		return
	}
	binding, _ := BindingFromContext(r.Context())
	osc, err := d.resolveOS(binding)
	if err != nil {
		d.Logger.Error("phantom-brain: binding configuration error", slog.String("err", err.Error()))
		WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeStorageBackendErr, "binding configuration error", nil)
		return
	}

	var req ForgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "invalid JSON body", nil)
		return
	}
	if err := validateSHA(req.SHA); err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, err.Error(), nil)
		return
	}

	// DeleteSummary is idempotent — a missing doc returns nil. We treat
	// the forget as succeeded regardless so a double-forget is benign.
	if err := osc.DeleteSummary(r.Context(), binding.Key.Profile, binding.Key.Vault, req.SHA); err != nil {
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"opensearch delete failed: "+err.Error(), nil)
		return
	}
	// Phase D2b: the forget takes effect on the next online recall —
	// the pb_records projection is the canonical read path now, so there
	// is no snapshot to rebuild.

	writeJSON(w, http.StatusOK, ForgetResponse{SHA: req.SHA, Forgotten: true})
}

// ResynthRequest is the POST /api/brain/resynth body. Both fields are
// optional — an empty body means dry_run=false, limit=0 (process all).
type ResynthRequest struct {
	DryRun bool `json:"dry_run"`
	Limit  int  `json:"limit"`
}

// handleResynth is the fix-it apply-companion to handleReflect (issue
// #82). It scans Synthesised=false summaries and, when not a dry run,
// kicks a background backfill that re-processes them WITHOUT going
// through the lossy SynthWorker.Enqueue channel — re-pushing dropped
// jobs through Enqueue could just drop them again. The backfill
// serializes with the live worker via processMu (entity-upsert safety).
//
//	POST /api/brain/resynth {"dry_run": true}   — report backlog, mutate nothing.
//	POST /api/brain/resynth {"dry_run": false}  — start the backfill.
func (d *Daemon) handleResynth(w http.ResponseWriter, r *http.Request) {
	binding, _ := BindingFromContext(r.Context())

	// Tolerate an empty body — both fields default. EOF (no body) is not
	// an error; anything else malformed is a 400.
	var req ResynthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "invalid JSON body", nil)
		return
	}

	// resynth needs the real worker — the noop SynthQueue (tests / legacy
	// single-process daemons) has no backfill machinery. Unavailable there,
	// which is the correct answer.
	worker, ok := d.synth.(*SynthWorker)
	if !ok {
		WriteErrorEnvelope(w, http.StatusServiceUnavailable, ErrCodeStorageBackendErr,
			"synth worker not available", nil)
		return
	}

	result, err := worker.ResynthBacklog(r.Context(), binding.Key.Profile, binding.Key.Vault, req.DryRun, req.Limit)
	if err != nil {
		if errors.Is(err, ErrResynthInProgress) {
			WriteErrorEnvelope(w, http.StatusConflict, ErrCodeStorageBackendErr, err.Error(), nil)
			return
		}
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"resynth failed: "+err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
