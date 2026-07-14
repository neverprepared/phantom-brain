package mart

import (
	"os"
	"testing"
)

func TestCredentials_RoundtripLookupUpsertRemove(t *testing.T) {
	dir := t.TempDir()

	// Missing file → empty store, no error.
	store, err := LoadCredentials(dir)
	if err != nil || len(store.Bindings) != 0 {
		t.Fatalf("missing store: %+v err %v", store, err)
	}

	store.Set(Credential{Profile: "personal", Vault: "memory", API: "https://a", Token: "t1"})
	store.Set(Credential{Profile: "gsa", Vault: "memory", API: "https://b", Token: "t2"})
	if err := SaveCredentials(dir, store); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 0600 perms (holds tokens).
	if fi, err := os.Stat(CredentialsPath(dir)); err != nil {
		t.Fatalf("stat: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("perms = %o, want 600", fi.Mode().Perm())
	}

	got, err := LoadCredentials(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if c, ok := got.Lookup("gsa", "memory"); !ok || c.Token != "t2" || c.API != "https://b" {
		t.Fatalf("lookup gsa: %+v ok=%v", c, ok)
	}
	if _, ok := got.Lookup("nope", "memory"); ok {
		t.Error("lookup of missing binding should be !ok")
	}

	// Upsert replaces in place (no dup).
	got.Set(Credential{Profile: "personal", Vault: "memory", API: "https://a2", Token: "t1b"})
	if len(got.Bindings) != 2 {
		t.Errorf("upsert grew the store to %d, want 2", len(got.Bindings))
	}
	if c, _ := got.Lookup("personal", "memory"); c.Token != "t1b" || c.API != "https://a2" {
		t.Errorf("upsert did not replace: %+v", c)
	}

	// Remove.
	if !got.Remove("personal", "memory") {
		t.Error("Remove should report true")
	}
	if _, ok := got.Lookup("personal", "memory"); ok {
		t.Error("binding still present after Remove")
	}
	if got.Remove("personal", "memory") {
		t.Error("Remove of missing binding should report false")
	}
}

func TestResolveCredential(t *testing.T) {
	dir := t.TempDir()
	store := Credentials{}
	store.Set(Credential{Profile: "personal", Vault: "memory", API: "https://store", Token: "stok"})
	if err := SaveCredentials(dir, store); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Profile: "personal", Vault: "memory"}

	// Store hit (env ignored).
	if api, tok, err := ResolveCredential(dir, spec, AgentEnv{}); err != nil || api != "https://store" || tok != "stok" {
		t.Fatalf("store hit: (%q,%q,%v)", api, tok, err)
	}
	// Store wins over a matching env.
	envMatch := AgentEnv{API: "https://env", Token: "etok", Profile: "personal", Vault: "memory"}
	if api, _, _ := ResolveCredential(dir, spec, envMatch); api != "https://store" {
		t.Errorf("store should win over env, got %q", api)
	}

	// No store → env fallback ONLY when it matches the binding.
	empty := t.TempDir()
	if api, tok, err := ResolveCredential(empty, spec, envMatch); err != nil || api != "https://env" || tok != "etok" {
		t.Fatalf("env fallback: (%q,%q,%v)", api, tok, err)
	}
	// Mismatched env is ignored → error.
	envOther := AgentEnv{API: "https://env", Token: "etok", Profile: "gsa", Vault: "memory"}
	if _, _, err := ResolveCredential(empty, spec, envOther); err == nil {
		t.Error("mismatched env must not resolve creds")
	}
	// No store, no env → error.
	if _, _, err := ResolveCredential(empty, spec, AgentEnv{}); err == nil {
		t.Error("no store + no env must error")
	}
}
