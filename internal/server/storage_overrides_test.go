package server

import (
	"context"
	"fmt"
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

// fakeCounts seeds a storageCountFn from an in-memory map keyed by
// "<prefix>|<profile>|<vault>". Missing keys return 0, matching the
// production CountByVault's "404 → 0" behaviour on a never-created
// prefixed index.
func fakeCounts(t *testing.T, m map[string]int64) storageCountFn {
	t.Helper()
	return func(_ context.Context, prefix, profile, vault string) (int64, error) {
		return m[prefix+"|"+profile+"|"+vault], nil
	}
}

// TestVerifyStorageOverrides_FootgunDetected drives the REAL
// verifyStorageOverridesWith — no parallel re-implementation — against
// an injected counter. The matrix mirrors the production decision tree
// for a single binding with [storage_overrides].
func TestVerifyStorageOverrides_FootgunDetected(t *testing.T) {
	binding := VaultBinding{
		Key:     VaultKey{Profile: "client_x", Vault: "main"},
		Storage: ResolvedStorage{IndexPrefix: "client_x_"},
	}
	const defaultPrefix = ""

	type row struct {
		name         string
		prefixedDocs int64
		sharedDocs   int64
		wantErr      bool
	}
	cases := []row{
		{"both empty — fresh binding", 0, 0, false},
		{"prefixed populated — migration done", 5, 5, false},
		{"only prefixed populated — clean", 5, 0, false},
		{"prefixed empty + shared populated — FOOTGUN", 0, 5, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			counts := fakeCounts(t, map[string]int64{
				"client_x_|client_x|main": c.prefixedDocs,
				"|client_x|main":          c.sharedDocs,
			})
			err := verifyStorageOverridesWith(context.Background(), defaultPrefix, []VaultBinding{binding}, counts)
			gotErr := err != nil
			if gotErr != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, c.wantErr)
			}
			if !gotErr {
				return
			}
			// Diagnostic must name the offending binding AND the count
			// (we hand the operator a useful message, not just "fail").
			msg := err.Error()
			if !strings.Contains(msg, "client_x/main") {
				t.Errorf("err should mention binding key; got %q", msg)
			}
			if !strings.Contains(msg, fmt.Sprintf("%d docs", c.sharedDocs)) {
				t.Errorf("err should mention shared doc count %d; got %q", c.sharedDocs, msg)
			}
			if !strings.Contains(msg, "storage_overrides") {
				t.Errorf("err should mention storage_overrides; got %q", msg)
			}
		})
	}
}

// TestVerifyStorageOverrides_SkipsBindingsWithoutOverride confirms a
// binding whose resolved prefix matches the default (i.e. NO override)
// is not checked at all, no matter how many docs sit on shared.
func TestVerifyStorageOverrides_SkipsBindingsWithoutOverride(t *testing.T) {
	binding := VaultBinding{
		Key:     VaultKey{Profile: "personal", Vault: "memory"},
		Storage: ResolvedStorage{IndexPrefix: ""},
	}
	counts := fakeCounts(t, map[string]int64{"|personal|memory": 999})
	if err := verifyStorageOverridesWith(context.Background(), "", []VaultBinding{binding}, counts); err != nil {
		t.Fatalf("binding without override should be skipped; got %v", err)
	}
}

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
// d.bindings is returned when registered.
func TestResolveOS_PrefersBindingView(t *testing.T) {
	scoped := newFakeOS()
	d := &Daemon{bindings: newBindingDepCache()}
	b := VaultBinding{Key: VaultKey{"p", "v"}}
	d.bindings.Set(b.Key, &bindingDeps{OS: scoped})
	got, err := d.resolveOS(b)
	if err != nil {
		t.Fatalf("registered binding: unexpected err %v", err)
	}
	if got != scoped {
		t.Errorf("expected per-binding osWriter")
	}
}

// TestResolveOS_FailsLoudOnCacheMiss confirms v3.2's blocker fix:
// when no per-binding view is registered the resolver returns an
// error mentioning the binding key rather than silently routing
// the write to the shared daemon-global osClient.
func TestResolveOS_FailsLoudOnCacheMiss(t *testing.T) {
	shared := newFakeOS()
	d := &Daemon{osClient: shared, bindings: newBindingDepCache()}
	b := VaultBinding{Key: VaultKey{"p", "v"}}
	got, err := d.resolveOS(b)
	if err == nil {
		t.Fatal("expected error on cache miss; got nil (silent fallback)")
	}
	if got != nil {
		t.Errorf("expected nil osWriter on error; got %v", got)
	}
	if !strings.Contains(err.Error(), "p/v") {
		t.Errorf("err should mention binding key; got %q", err.Error())
	}
}

// TestResolveOS_SharedFallbackOptIn confirms the explicit
// allowSharedFallback flag re-enables the legacy single-tenant
// path (tests, single-binding daemons).
func TestResolveOS_SharedFallbackOptIn(t *testing.T) {
	shared := newFakeOS()
	d := &Daemon{osClient: shared, bindings: newBindingDepCache(), allowSharedFallback: true}
	b := VaultBinding{Key: VaultKey{"p", "v"}}
	got, err := d.resolveOS(b)
	if err != nil {
		t.Fatalf("unexpected err with allowSharedFallback: %v", err)
	}
	if got != shared {
		t.Errorf("expected shared osClient fallback")
	}
}

// TestResolveAttach_PrefersBindingView is the AttachmentStore analogue.
func TestResolveAttach_PrefersBindingView(t *testing.T) {
	scoped := newFakeAttach()
	d := &Daemon{bindings: newBindingDepCache()}
	b := VaultBinding{Key: VaultKey{"p", "v"}}
	d.bindings.Set(b.Key, &bindingDeps{Attach: scoped})
	got, err := d.resolveAttach(b)
	if err != nil {
		t.Fatalf("registered binding: unexpected err %v", err)
	}
	if got != scoped {
		t.Errorf("expected per-binding attach")
	}
}

// TestResolveAttach_FailsLoudOnCacheMiss mirrors the OS analogue.
func TestResolveAttach_FailsLoudOnCacheMiss(t *testing.T) {
	shared := newFakeAttach()
	d := &Daemon{attach: shared, bindings: newBindingDepCache()}
	b := VaultBinding{Key: VaultKey{"p", "v"}}
	got, err := d.resolveAttach(b)
	if err == nil {
		t.Fatal("expected error on cache miss; got nil (silent fallback)")
	}
	if got != nil {
		t.Errorf("expected nil AttachmentStore on error; got %v", got)
	}
	if !strings.Contains(err.Error(), "p/v") {
		t.Errorf("err should mention binding key; got %q", err.Error())
	}
}

// silence unused (ctx ref placeholder for any future OS-gated test).
var _ = context.Background
