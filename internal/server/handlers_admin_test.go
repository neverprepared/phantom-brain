package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// PB_ADMIN_KEY is read when buildRouter mounts the admin middleware, so every
// test sets it BEFORE newRouterRig. The create path (which reloads the daemon)
// is validated live against the real daemon; here we cover the security-
// critical auth middleware + the read-only GET (which also proves the operator
// key passes the middleware through to a handler).

func adminReq(t *testing.T, method, url, key string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// An unset PB_ADMIN_KEY disables the admin surface entirely (fail closed).
func TestAdminAuth_DisabledWithoutKey(t *testing.T) {
	t.Setenv("PB_ADMIN_KEY", "")
	rig := newRouterRig(t)
	resp := adminReq(t, "GET", rig.server.URL+"/api/brain/admin/profiles/personal/memory", "anything")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 when PB_ADMIN_KEY unset, got %d", resp.StatusCode)
	}
}

func TestAdminAuth_WrongKeyForbidden(t *testing.T) {
	t.Setenv("PB_ADMIN_KEY", "op-secret")
	rig := newRouterRig(t)
	resp := adminReq(t, "GET", rig.server.URL+"/api/brain/admin/profiles/personal/memory", "wrong")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 for wrong operator key, got %d", resp.StatusCode)
	}
}

// Correct operator key → middleware passes → GET returns the seeded binding's
// observed state (no secret).
func TestAdminProfileGet_ReturnsBinding(t *testing.T) {
	t.Setenv("PB_ADMIN_KEY", "op-secret")
	rig := newRouterRig(t) // seeds personal/memory
	resp := adminReq(t, "GET", rig.server.URL+"/api/brain/admin/profiles/personal/memory", "op-secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Profile string `json:"profile"`
		Vault   string `json:"vault"`
		Exists  bool   `json:"exists"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Profile != "personal" || out.Vault != "memory" || !out.Exists {
		t.Fatalf("unexpected body: %+v", out)
	}
}

func TestAdminProfileGet_NotFound(t *testing.T) {
	t.Setenv("PB_ADMIN_KEY", "op-secret")
	rig := newRouterRig(t)
	resp := adminReq(t, "GET", rig.server.URL+"/api/brain/admin/profiles/nope/memory", "op-secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for missing binding, got %d", resp.StatusCode)
	}
}
