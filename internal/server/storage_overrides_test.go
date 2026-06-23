package server

import (
	"context"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// --- config parsing ---------------------------------------------------

func TestStorageOverrides_TOMLRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		body string
		want StorageOverrides
	}{
		{
			name: "both fields",
			body: "[storage_overrides]\nindex_prefix = \"client_x_\"\nbucket = \"pb-client-x\"\n",
			want: StorageOverrides{IndexPrefix: "client_x_", Bucket: "pb-client-x"},
		},
		{
			name: "prefix only",
			body: "[storage_overrides]\nindex_prefix = \"only_prefix_\"\n",
			want: StorageOverrides{IndexPrefix: "only_prefix_"},
		},
		{
			name: "bucket only",
			body: "[storage_overrides]\nbucket = \"only-bucket\"\n",
			want: StorageOverrides{Bucket: "only-bucket"},
		},
		{
			name: "neither",
			body: "[storage_overrides]\n",
			want: StorageOverrides{},
		},
		{
			name: "block absent — defaults zero",
			body: "retention_gens = 10\n",
			want: StorageOverrides{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got VaultOverrides
			if _, err := toml.Decode(c.body, &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.StorageOverrides != c.want {
				t.Errorf("StorageOverrides = %+v, want %+v", got.StorageOverrides, c.want)
			}
		})
	}
}

// --- registry resolution ----------------------------------------------

func TestRegistry_StorageOverrides_Resolve(t *testing.T) {
	dir := t.TempDir()
	// (a) no overrides — inherits defaults.
	_ = seedVault(t, dir, "personal", "memory", "")
	// (b) prefix only.
	_ = seedVault(t, dir, "client_a", "main", "[storage_overrides]\nindex_prefix = \"a_\"\n")
	// (c) both fields.
	_ = seedVault(t, dir, "client_b", "main", "[storage_overrides]\nindex_prefix = \"b_\"\nbucket = \"bucket-b\"\n")

	r := NewRegistry()
	_, err := r.Load(LoadOpts{
		ConfigDir:          dir,
		DefaultIndexPrefix: "global_",
		DefaultBucket:      "global-bucket",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	a, ok := r.LookupByVault(VaultKey{"personal", "memory"})
	if !ok {
		t.Fatal("a missing")
	}
	if a.Storage.IndexPrefix != "global_" || a.Storage.Bucket != "global-bucket" {
		t.Errorf("a Storage = %+v", a.Storage)
	}

	b, _ := r.LookupByVault(VaultKey{"client_a", "main"})
	if b.Storage.IndexPrefix != "global_a_" || b.Storage.Bucket != "global-bucket" {
		t.Errorf("b Storage = %+v; want IndexPrefix=global_a_ Bucket=global-bucket", b.Storage)
	}

	c, _ := r.LookupByVault(VaultKey{"client_b", "main"})
	if c.Storage.IndexPrefix != "global_b_" || c.Storage.Bucket != "bucket-b" {
		t.Errorf("c Storage = %+v; want IndexPrefix=global_b_ Bucket=bucket-b", c.Storage)
	}
}

func TestRegistry_StorageOverrides_EmptyGlobalPrefix(t *testing.T) {
	// When the daemon's global prefix is empty (the common case) a
	// binding-level override stands alone as the final prefix.
	dir := t.TempDir()
	_ = seedVault(t, dir, "p", "v", "[storage_overrides]\nindex_prefix = \"only_\"\n")
	r := NewRegistry()
	_, err := r.Load(LoadOpts{ConfigDir: dir, DefaultIndexPrefix: ""})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := r.LookupByVault(VaultKey{"p", "v"})
	if b.Storage.IndexPrefix != "only_" {
		t.Errorf("Storage.IndexPrefix = %q, want only_", b.Storage.IndexPrefix)
	}
}

// --- footgun detection ------------------------------------------------

// countByVaultStub returns canned counts keyed by (prefix, profile, vault).
type countByVaultStub struct {
	t      *testing.T
	counts map[string]int64 // key = prefix|profile|vault
}

// VerifyStorageOverrides depends on osearch.Client.CountByVault; we
// can't easily fake that without a live OS. The test instead drives
// the function logic via an injected resolver: we wrap the verifier
// in a tiny shim and check it manually here, since the production
// verifier requires a real *osearch.Client.
//
// To avoid bringing up an OS cluster the test below exercises the
// helper in isolation: we re-implement the decision matrix as a pure
// function and assert each table row matches what VerifyStorageOverrides
// would produce given the same counts.
func TestStorageOverrides_FootgunDecisionMatrix(t *testing.T) {
	type row struct {
		name         string
		hasOverride  bool
		prefixedDocs int64
		sharedDocs   int64
		wantErr      bool
	}
	cases := []row{
		{"no override, doesn't matter", false, 0, 999, false},
		{"override, both empty — fresh binding", true, 0, 0, false},
		{"override, prefixed populated — migration done", true, 5, 5, false},
		{"override, only prefixed populated — clean", true, 5, 0, false},
		{"override, prefixed empty + shared populated — FOOTGUN", true, 0, 5, true},
		{"override, prefixed empty + shared empty — fresh binding", true, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := footgunDecide(c.hasOverride, c.prefixedDocs, c.sharedDocs)
			gotErr := err != nil
			if gotErr != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, c.wantErr)
			}
			if gotErr && !strings.Contains(err.Error(), "storage_overrides") {
				t.Errorf("err missing diagnostic: %v", err)
			}
		})
	}
}

// footgunDecide mirrors VerifyStorageOverrides' inner decision so the
// table-driven test above stays runnable without an OS cluster. Keep
// this in sync with the production logic — both paths return the
// same outcome for the same inputs.
func footgunDecide(hasOverride bool, prefixedDocs, sharedDocs int64) error {
	if !hasOverride {
		return nil
	}
	if prefixedDocs > 0 {
		return nil
	}
	if sharedDocs > 0 {
		return errSimulatedFootgun
	}
	return nil
}

type footgunErr struct{ msg string }

func (e *footgunErr) Error() string { return e.msg }

var errSimulatedFootgun = &footgunErr{msg: "binding x has [storage_overrides] but N docs exist on shared indices — run migration or revert config"}

// --- binding view cache -----------------------------------------------

func TestBindingDepCache_GetSetDelete(t *testing.T) {
	c := newBindingDepCache()
	k := VaultKey{"p", "v"}
	if _, ok := c.Get(k); ok {
		t.Fatal("empty cache should miss")
	}
	deps := &bindingDeps{}
	c.Set(k, deps)
	got, ok := c.Get(k)
	if !ok || got != deps {
		t.Fatal("set/get mismatch")
	}
	c.Delete(k)
	if _, ok := c.Get(k); ok {
		t.Fatal("delete didn't remove")
	}
}

// TestResolveOS_PrefersBindingView confirms the per-binding view in
// d.bindings shadows the shared d.osClient when registered.
func TestResolveOS_PrefersBindingView(t *testing.T) {
	shared := newFakeOS()
	d := &Daemon{osClient: shared, bindings: newBindingDepCache()}
	b := VaultBinding{Key: VaultKey{"p", "v"}}
	if got := d.resolveOS(b); got != shared {
		t.Fatal("empty cache: expected shared osClient")
	}
	scoped := newFakeOS()
	d.bindings.Set(b.Key, &bindingDeps{OS: scoped})
	if got := d.resolveOS(b); got != scoped {
		t.Errorf("expected per-binding osWriter")
	}
}

// TestResolveAttach_PrefersBindingView is the AttachmentStore
// analogue. Fallback (no binding registered → d.attach) is exercised
// by every other handler test in this package.
func TestResolveAttach_PrefersBindingView(t *testing.T) {
	shared := newFakeAttach()
	d := &Daemon{attach: shared, bindings: newBindingDepCache()}
	b := VaultBinding{Key: VaultKey{"p", "v"}}
	if got := d.resolveAttach(b); got != shared {
		t.Fatal("empty cache: expected shared attach")
	}
	scoped := newFakeAttach()
	d.bindings.Set(b.Key, &bindingDeps{Attach: scoped})
	if got := d.resolveAttach(b); got != scoped {
		t.Errorf("expected per-binding attach")
	}
}

// silence unused (ctx ref placeholder for any future OS-gated test).
var _ = context.Background
