package server

import (
	"encoding/json"
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
	// Rebuild the snapshot so the forget reaches agents on their next
	// birth. The debouncer collapses bursts; a single forget still
	// kicks a rebuild. Nil in legacy / test daemons that don't run it.
	if d.debouncer != nil {
		d.debouncer.Trigger(binding.Key.Profile, binding.Key.Vault)
	}

	writeJSON(w, http.StatusOK, ForgetResponse{SHA: req.SHA, Forgotten: true})
}
