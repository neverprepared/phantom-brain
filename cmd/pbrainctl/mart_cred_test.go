package main

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/mart"
)

func credTestCmd(configDir string) *cobra.Command {
	c := &cobra.Command{}
	c.Flags().String("config-dir", configDir, "")
	return c
}

func clearMartAgentEnv(t *testing.T) {
	for _, k := range []string{"CL_BRAIN_API", "CL_BRAIN_API_TOKEN", "CL_WORKSPACE_PROFILE", "CL_BRAIN_VAULT"} {
		t.Setenv(k, "")
	}
}

func setMartAgentEnv(t *testing.T, api, token, profile, vault string) {
	t.Setenv("CL_BRAIN_API", api)
	t.Setenv("CL_BRAIN_API_TOKEN", token)
	t.Setenv("CL_WORKSPACE_PROFILE", profile)
	t.Setenv("CL_BRAIN_VAULT", vault)
}

func TestResolveMartCreds_StoreHit(t *testing.T) {
	dir := t.TempDir()
	store := mart.Credentials{}
	store.Set(mart.Credential{Profile: "personal", Vault: "memory", API: "https://store", Token: "stok"})
	if err := mart.SaveCredentials(dir, store); err != nil {
		t.Fatal(err)
	}
	clearMartAgentEnv(t)
	api, token, err := resolveMartCreds(credTestCmd(dir), mart.Spec{Profile: "personal", Vault: "memory"})
	if err != nil || api != "https://store" || token != "stok" {
		t.Fatalf("got (%q,%q,%v), want store creds", api, token, err)
	}
}

func TestResolveMartCreds_EnvFallbackWhenMatching(t *testing.T) {
	dir := t.TempDir() // empty store
	setMartAgentEnv(t, "https://env", "etok", "personal", "memory")
	api, token, err := resolveMartCreds(credTestCmd(dir), mart.Spec{Profile: "personal", Vault: "memory"})
	if err != nil || api != "https://env" || token != "etok" {
		t.Fatalf("got (%q,%q,%v), want env creds", api, token, err)
	}
}

func TestResolveMartCreds_ErrorWhenEnvMismatch(t *testing.T) {
	dir := t.TempDir() // empty store
	// env is bound to a DIFFERENT tenant → must be ignored, not trusted.
	setMartAgentEnv(t, "https://env", "etok", "other", "memory")
	if _, _, err := resolveMartCreds(credTestCmd(dir), mart.Spec{Profile: "personal", Vault: "memory"}); err == nil {
		t.Fatal("expected an error when neither store nor a matching env has creds")
	}
}

func TestResolveMartCreds_StoreWinsOverEnv(t *testing.T) {
	dir := t.TempDir()
	store := mart.Credentials{}
	store.Set(mart.Credential{Profile: "personal", Vault: "memory", API: "https://store", Token: "stok"})
	_ = mart.SaveCredentials(dir, store)
	setMartAgentEnv(t, "https://env", "etok", "personal", "memory") // also matches
	api, token, _ := resolveMartCreds(credTestCmd(dir), mart.Spec{Profile: "personal", Vault: "memory"})
	if api != "https://store" || token != "stok" {
		t.Fatalf("store should win, got (%q,%q)", api, token)
	}
}

func TestForEachMart_ResolvesPerSpecAndContinuesOnError(t *testing.T) {
	dir := t.TempDir()
	reg := mart.OpenRegistry(dir)
	if err := reg.Save(mart.Spec{Name: "alpha", Profile: "personal", Vault: "memory", Dest: "/tmp/a"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(mart.Spec{Name: "bravo", Profile: "gsa", Vault: "memory", Dest: "/tmp/b"}); err != nil {
		t.Fatal(err)
	}
	// Only alpha's binding has creds; bravo has none (and no matching env).
	store := mart.Credentials{}
	store.Set(mart.Credential{Profile: "personal", Vault: "memory", API: "https://a", Token: "atok"})
	_ = mart.SaveCredentials(dir, store)
	clearMartAgentEnv(t)

	var ran []string
	err := forEachMart(credTestCmd(dir), func(spec mart.Spec, _ *mart.Registry, _ *brain.Client) error {
		ran = append(ran, spec.Name)
		return nil
	})
	if err == nil {
		t.Error("expected an error because bravo could not resolve creds")
	}
	if len(ran) != 1 || ran[0] != "alpha" {
		t.Errorf("ran = %v, want only [alpha] (bravo skipped, no creds)", ran)
	}
}
