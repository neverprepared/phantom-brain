package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/neverprepared/phantom-brain/internal/pgstore"
	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
	"github.com/neverprepared/phantom-brain/internal/projection"
)

// brain_reflect / brain_forget HTTP surface (issue #72, Phase 1).
//
//   GET  /api/brain/reflect  — read-only report of forget-candidates.
//   POST /api/brain/forget   — delete one record by SHA from the SoR +
//                              enqueue a projection delete so it leaves
//                              the pb_records read path.
//
// Phase D: both target the Postgres System-of-Record via resolvePG (the
// legacy pb_summaries index is no longer written — scanning/deleting it
// produced an empty report and a no-op delete that lied "forgotten").

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
	binding, _ := BindingFromContext(r.Context())
	view, ok := d.resolvePGOrError(w, binding, "reflect")
	if !ok {
		return
	}

	q := pgstore.New(view.Pool())
	candidates, err := staleGateCandidates(r.Context(), q, binding.Key.Profile, binding.Key.Vault)
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
	binding, _ := BindingFromContext(r.Context())
	view, ok := d.resolvePGOrError(w, binding, "forget")
	if !ok {
		return
	}

	var req ForgetRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if err := validateSHA(req.SHA); err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, err.Error(), nil)
		return
	}

	// Delete from the Postgres SoR and enqueue a projection DELETE in the
	// SAME tx (the outbox) so the record leaves both the SoR and the
	// pb_records read path atomically. Mirrors writeSynthResult's
	// begin / defer-rollback / commit pattern.
	ctx := r.Context()
	tx, err := view.Pool().Begin(ctx)
	if err != nil {
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"forget begin failed: "+err.Error(), nil)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	q := pgstore.New(tx)
	if _, err := q.DeleteRecordBySHA(ctx, pgdb.DeleteRecordBySHAParams{
		Profile: binding.Key.Profile,
		Vault:   binding.Key.Vault,
		Sha:     req.SHA,
	}); err != nil {
		if errIsNoRows(err) {
			// Honest negative: no record for this SHA, nothing to forget.
			writeJSON(w, http.StatusOK, ForgetResponse{SHA: req.SHA, Forgotten: false})
			return
		}
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"record delete failed: "+err.Error(), nil)
		return
	}

	if err := projection.EnqueueDeleteTx(ctx, view.River(), tx, binding.Key.Profile, binding.Key.Vault, req.SHA); err != nil {
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"forget enqueue failed: "+err.Error(), nil)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		WriteErrorEnvelope(w, http.StatusBadGateway, ErrCodeStorageBackendErr,
			"forget commit failed: "+err.Error(), nil)
		return
	}
	committed = true

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
