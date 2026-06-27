package brain

import (
	"errors"
	"testing"
)

// TestAPIError_ErrorString covers both branches of APIError.Error():
// with a code (the daemon-envelope case) and without (a bare proxy 5xx).
func TestAPIError_ErrorString(t *testing.T) {
	withCode := &APIError{StatusCode: 409, Code: "CONFLICT", Message: "gone"}
	if got := withCode.Error(); got != "brain: daemon API error 409 CONFLICT: gone" {
		t.Errorf("with code: %q", got)
	}
	noCode := &APIError{StatusCode: 502, Message: "Bad Gateway"}
	if got := noCode.Error(); got != "brain: daemon API error 502: Bad Gateway" {
		t.Errorf("no code: %q", got)
	}
}

// TestIsAPIErrorCode_NonAPIError confirms the false branch when the
// error chain holds no *APIError at all.
func TestIsAPIErrorCode_NonAPIError(t *testing.T) {
	if IsAPIErrorCode(errors.New("plain"), "CONFLICT") {
		t.Error("plain error must not match an API code")
	}
	if IsAPIErrorCode(nil, "CONFLICT") {
		t.Error("nil error must not match")
	}
	// Right type, wrong code → false.
	if IsAPIErrorCode(&APIError{Code: "OTHER"}, "CONFLICT") {
		t.Error("mismatched code must not match")
	}
}
