package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
)

// --- /merge/init -----------------------------------------------------

type mergeInitRequest struct {
	BrainID     string `json:"brain_id"`
	TTLSecs     int    `json:"ttl_secs,omitempty"`
	PayloadSize int64  `json:"payload_size,omitempty"`
}

type mergeInitResponse struct {
	UploadID string `json:"upload_id"`
	URL      string `json:"url"`
	Token    string `json:"token"`
	Expires  int64  `json:"expires"` // unix seconds
}

// handleMergeInit allocates an upload_id and returns the URL the
// brain should PUT its death tarball to. Backend-agnostic: local
// returns a daemon URL, MinIO would return a presigned S3 URL.
func (d *Daemon) handleMergeInit(w http.ResponseWriter, r *http.Request) {
	binding, _ := BindingFromContext(r.Context())
	var req mergeInitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "invalid JSON body", nil)
		return
	}
	if req.BrainID == "" {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "brain_id required", nil)
		return
	}
	if req.PayloadSize > binding.Defaults.MaxTarballBytes && binding.Defaults.MaxTarballBytes > 0 {
		WriteErrorEnvelope(w, http.StatusRequestEntityTooLarge, ErrCodePayloadTooLarge,
			fmt.Sprintf("payload_size %d exceeds max %d", req.PayloadSize, binding.Defaults.MaxTarballBytes), nil)
		return
	}
	if InMaintenance(d.DataDir, binding.Key) {
		WriteErrorEnvelope(w, http.StatusServiceUnavailable, ErrCodeMaintenanceMode,
			"vault is in maintenance mode", nil)
		return
	}
	ttl := time.Duration(req.TTLSecs) * time.Second
	if ttl <= 0 {
		ttl = 1 * time.Hour
	}
	handle, err := d.storage.NewUpload(req.BrainID, ttl)
	if err != nil {
		WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeStorageBackendErr, err.Error(), nil)
		return
	}
	// Backend-specific handshake: local needs the (profile, vault)
	// stashed before AcceptUpload can validate; MinIO finishes the
	// presign once we know which prefix to sign under. Both end up
	// with a populated URL on the handle.
	switch b := d.storage.(type) {
	case *LocalBackend:
		b.RegisterUpload(handle.UploadID, req.BrainID, binding.Key.Profile, binding.Key.Vault, handle.Expires)
	case *MinIOBackend:
		presigned, objKey, perr := b.PresignedPutForUpload(r.Context(), binding.Key.Profile, binding.Key.Vault, handle.UploadID, ttl)
		if perr != nil {
			WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeStorageBackendErr, perr.Error(), nil)
			return
		}
		handle.URL = presigned
		b.RegisterUpload(handle.UploadID, req.BrainID, binding.Key.Profile, binding.Key.Vault, objKey, handle.Expires)
	}
	writeJSON(w, http.StatusOK, mergeInitResponse{
		UploadID: handle.UploadID,
		URL:      handle.URL,
		Token:    handle.Token,
		Expires:  handle.Expires.Unix(),
	})
}

// --- /merge/upload/{upload_id} (local backend only) ------------------

// handleMergeUpload accepts the streamed tarball body. Local-backend
// only; MinIO uploads go directly to S3 and never hit this route.
// Token + expires are query-string params (so curl one-liners work
// without custom headers).
func (d *Daemon) handleMergeUpload(w http.ResponseWriter, r *http.Request) {
	lb, ok := d.storage.(*LocalBackend)
	if !ok {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest,
			"upload route is local-backend only", nil)
		return
	}
	uploadID := chi.URLParam(r, "uploadID")
	token := r.URL.Query().Get("token")
	if uploadID == "" || token == "" {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "missing upload_id or token", nil)
		return
	}
	if _, err := lb.VerifyToken(uploadID, token); err != nil {
		WriteErrorEnvelope(w, http.StatusForbidden, ErrCodeInvalidToken, err.Error(), nil)
		return
	}
	st, _ := lb.LookupUpload(uploadID)
	maxBytes := int64(5 << 30)
	if b, ok := d.registry.LookupByVault(st.Profile2VaultKey()); ok {
		if b.Defaults.MaxTarballBytes > 0 {
			maxBytes = b.Defaults.MaxTarballBytes
		}
	}
	n, err := lb.AcceptUpload(uploadID, r.Body, maxBytes)
	if err != nil {
		WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeStorageBackendErr, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"received_bytes": n})
}

// Profile2VaultKey turns a localUploadState into a VaultKey. Helper
// for handlers that need to look up the binding from upload state.
func (s localUploadState) Profile2VaultKey() VaultKey {
	return VaultKey{Profile: s.Profile, Vault: s.Vault}
}

// --- /merge/complete/{upload_id} -------------------------------------

type mergeCompleteRequest struct {
	BrainID string `json:"brain_id"`
}

// handleMergeComplete finalises the upload: moves the staged tarball
// into brains/_pending/<brain_id>.tar so the reaper finds it on its
// next tick. Idempotent at the brain-id level — if a tar already
// exists in _pending for this brain_id, complete returns 409 so the
// brain knows it raced itself.
func (d *Daemon) handleMergeComplete(w http.ResponseWriter, r *http.Request) {
	binding, _ := BindingFromContext(r.Context())
	uploadID := chi.URLParam(r, "uploadID")
	var req mergeCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "invalid JSON body", nil)
		return
	}
	if req.BrainID == "" {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "brain_id required", nil)
		return
	}
	// Block double-complete: if _pending already has the brain's tar
	// we refuse silently. Reaper or a previous complete already took
	// it.
	pendingPath := filepath.Join(d.DataDir.BrainsDir(binding.Key.Profile, binding.Key.Vault),
		"_pending", req.BrainID+".tar")
	if _, err := os.Stat(pendingPath); err == nil {
		WriteErrorEnvelope(w, http.StatusConflict, ErrCodeMergeInProgress,
			"this brain_id is already pending merge", nil)
		return
	}
	finalPath, err := d.storage.CompleteUpload(binding.Key.Profile, binding.Key.Vault, req.BrainID, uploadID)
	if err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"brain_id":    req.BrainID,
		"pending_path": finalPath,
	})
}

// --- GET /merge/{brain_id} -------------------------------------------

// handleMergeStatus reports the merge state for a brain_id: pending
// (waiting for reaper), merged (ledger row present), or not-found.
// Lets a brain poll after complete without reading the ledger
// directly.
func (d *Daemon) handleMergeStatus(w http.ResponseWriter, r *http.Request) {
	binding, _ := BindingFromContext(r.Context())
	brainID := chi.URLParam(r, "brainID")
	if brainID == "" {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "brain_id required", nil)
		return
	}
	// Pending check.
	pendingPath := filepath.Join(d.DataDir.BrainsDir(binding.Key.Profile, binding.Key.Vault),
		"_pending", brainID+".tar")
	if _, err := os.Stat(pendingPath); err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"brain_id": brainID, "state": "pending"})
		return
	}
	// Ledger check.
	ledger, err := OpenLedger(d.DataDir, binding.Key.Profile, binding.Key.Vault)
	if err != nil {
		WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeInternal, err.Error(), nil)
		return
	}
	defer ledger.Close()
	rec, err := ledger.Get(brainID)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"brain_id":         rec.BrainID,
			"state":            "merged",
			"merged_at":        rec.MergedAt.UTC().Format(time.RFC3339),
			"raw_count":        rec.RawCount,
			"attachment_count": rec.AttachmentCount,
		})
		return
	}
	WriteErrorEnvelope(w, http.StatusNotFound, ErrCodeNotFound,
		"brain_id not pending and not merged", nil)
}

// --- POST /birth/claim -----------------------------------------------

type birthClaimRequest struct {
	BrainID string `json:"brain_id"`
	Gen     uint64 `json:"gen"`
	TTLSecs int    `json:"ttl_secs,omitempty"`
}

// handleBirthClaim drops a .claims/<brain_id> marker under
// staged/snapshot-<gen>/ so the snapshot prune logic knows that gen
// is in use. Returns 409 STALE_SNAPSHOT if the gen has already been
// pruned (it would be a race with retention; the brain should re-call
// snapshot/current and try the new gen).
func (d *Daemon) handleBirthClaim(w http.ResponseWriter, r *http.Request) {
	binding, _ := BindingFromContext(r.Context())
	var req birthClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "invalid JSON body", nil)
		return
	}
	if req.BrainID == "" || req.Gen == 0 {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "brain_id + gen required", nil)
		return
	}
	stagedGen := filepath.Join(d.DataDir.StagedDir(binding.Key.Profile, binding.Key.Vault),
		fmt.Sprintf("snapshot-%d", req.Gen))
	if _, err := os.Stat(stagedGen); errors.Is(err, os.ErrNotExist) {
		WriteErrorEnvelope(w, http.StatusConflict, ErrCodeStaleSnapshot,
			fmt.Sprintf("snapshot gen %d has been pruned; re-fetch snapshot/current", req.Gen), nil)
		return
	}
	claimsDir := filepath.Join(stagedGen, ".claims")
	if err := os.MkdirAll(claimsDir, 0o755); err != nil {
		WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeInternal, err.Error(), nil)
		return
	}
	marker := filepath.Join(claimsDir, req.BrainID)
	if err := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeInternal, err.Error(), nil)
		return
	}
	ttl := req.TTLSecs
	if ttl < 3600 {
		ttl = 3600
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"brain_id": req.BrainID,
		"gen":      req.Gen,
		"expires":  time.Now().Add(time.Duration(ttl) * time.Second).Unix(),
	})
}

// --- maintenance ------------------------------------------------------

// handleMaintenance handles both /maintenance/enter and
// /maintenance/exit via the chi URL param to keep the surface small.
// GET /maintenance reports the current state.
func (d *Daemon) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	binding, _ := BindingFromContext(r.Context())
	action := chi.URLParam(r, "action")
	switch action {
	case "enter":
		if err := SetMaintenance(d.DataDir, binding.Key); err != nil {
			WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeInternal, err.Error(), nil)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"maintenance": true})
	case "exit":
		if err := ClearMaintenance(d.DataDir, binding.Key); err != nil {
			WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeInternal, err.Error(), nil)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"maintenance": false})
	default:
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest,
			fmt.Sprintf("unknown action %q (expected enter|exit)", action), nil)
	}
}

// handleMaintenanceGet returns the current maintenance state for the
// caller's vault. Lets ops tooling poll without round-tripping
// /enter|/exit.
func (d *Daemon) handleMaintenanceGet(w http.ResponseWriter, r *http.Request) {
	binding, _ := BindingFromContext(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"maintenance": InMaintenance(d.DataDir, binding.Key)})
}

