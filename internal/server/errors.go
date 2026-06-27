package server

import (
	"encoding/json"
	"net/http"
)

// Error envelope codes used across the API. These are STRING constants
// (not int enums) because operators and clients pattern-match on them
// in logs and code; integer enums would force a translation layer at
// every consumption point. Values match the v4.4 §8 documented set.
const (
	ErrCodeInvalidToken       = "INVALID_TOKEN"
	ErrCodeVaultNotFound      = "VAULT_NOT_FOUND"
	ErrCodeMaintenanceMode    = "MAINTENANCE_MODE"
	ErrCodeMergeInProgress    = "MERGE_IN_PROGRESS"
	ErrCodePayloadTooLarge    = "PAYLOAD_TOO_LARGE"
	ErrCodeStorageBackendErr  = "STORAGE_BACKEND_ERROR"
	ErrCodeInternal           = "INTERNAL_ERROR"
	ErrCodeNotFound           = "NOT_FOUND"
	ErrCodeBadRequest         = "BAD_REQUEST"
)

// ErrorEnvelope is the on-the-wire shape every error response uses.
// Matches v4.4 §8 verbatim so the Phase 1 agent code's planned
// response parser (Phase 2.5) does not need conditional handling.
type ErrorEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details,omitempty"`
	} `json:"error"`
}

// WriteErrorEnvelope writes a JSON error response with the given
// status, code, and message. Details is optional context (e.g. the
// snapshot gen that was stale). Any encoding failure falls back to
// http.Error so the client at least sees the status code; we don't
// loop trying to re-encode.
func WriteErrorEnvelope(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	env := ErrorEnvelope{}
	env.Error.Code = code
	env.Error.Message = message
	env.Error.Details = details
	body, err := json.Marshal(env)
	if err != nil {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
