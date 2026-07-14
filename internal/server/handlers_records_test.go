package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These cover the pre-Postgres branches of handleListRecords (auth + query
// param validation), which need no database. The happy path + keyset + filter
// passthrough are covered by handlers_records_integration_test.go (Docker).

func recordsReq(target string, withBinding bool) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if withBinding {
		b := VaultBinding{Key: VaultKey{Profile: "p", Vault: "v"}}
		req = req.WithContext(context.WithValue(req.Context(), authCtxKey{}, b))
	}
	return req
}

func TestHandleListRecords_NoBinding401(t *testing.T) {
	d := &Daemon{}
	rec := httptest.NewRecorder()
	d.handleListRecords(rec, recordsReq("/api/brain/records", false))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleListRecords_BadParams400(t *testing.T) {
	cases := map[string]string{
		"bad after_id":     "/api/brain/records?after_id=abc",
		"negative after_id": "/api/brain/records?after_id=-1",
		"bad limit":        "/api/brain/records?limit=0",
		"non-numeric limit": "/api/brain/records?limit=x",
		"bad synthesised":   "/api/brain/records?synthesised=maybe",
	}
	d := &Daemon{}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			d.handleListRecords(rec, recordsReq(target, true))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}
