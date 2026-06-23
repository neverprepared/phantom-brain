package server

import (
	"context"
	"fmt"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// storageCountFn is the slice of OpenSearch the footgun verifier
// needs: count the docs in pb_summaries matching (profile, vault)
// inside the index identified by prefix. Defined as a callback so
// tests can inject an in-memory equivalent without bringing up a
// real OpenSearch cluster. Production wiring closes over a
// *osearch.Client and delegates to CountByVault on the prefixed
// view.
type storageCountFn func(ctx context.Context, prefix, profile, vault string) (int64, error)

// VerifyStorageOverrides walks every binding with a [storage_overrides]
// active and refuses startup when the override would silently strand
// existing data on the shared indices (operator-footgun: an operator
// adds [storage_overrides] to a binding that already has docs on the
// shared prefix; the binding suddenly stops seeing its own data).
//
// For each binding whose resolved IndexPrefix differs from defaultPrefix
// (i.e. an override is in play):
//
//  1. Count docs at the binding's PREFIXED pb_summaries matching
//     (profile, vault).
//  2. Count docs at the SHARED pb_summaries (default prefix) matching
//     the same (profile, vault).
//  3. If (1) == 0 AND (2) > 0, refuse to start with the diagnostic:
//     "binding <profile>/<vault> has [storage_overrides] but N docs
//     exist on shared indices — run migration or revert config".
//
// A missing prefixed index is treated as count=0 (handled by
// CountByVault returning 0 on 404), which is exactly the trigger
// condition for the footgun. A missing shared index is also count=0,
// which means a fully-overridden deployment that has decommissioned
// the shared prefix passes the check fine.
//
// Bindings with no override (resolved prefix == defaultPrefix) are
// skipped entirely.
func VerifyStorageOverrides(ctx context.Context, oc *osearch.Client, defaultPrefix string, bindings []VaultBinding) error {
	if oc == nil {
		return nil
	}
	count := func(ctx context.Context, prefix, profile, vault string) (int64, error) {
		return oc.WithPrefix(prefix).CountByVault(ctx, osearch.IndexSummaries, profile, vault)
	}
	return verifyStorageOverridesWith(ctx, defaultPrefix, bindings, count)
}

// verifyStorageOverridesWith is the inner implementation parameterised
// by the count adapter. Kept un-exported; the public surface above
// closes over a *osearch.Client.
func verifyStorageOverridesWith(
	ctx context.Context,
	defaultPrefix string,
	bindings []VaultBinding,
	count storageCountFn,
) error {
	for _, b := range bindings {
		if b.Storage.IndexPrefix == defaultPrefix {
			continue
		}
		prefixedCount, err := count(ctx, b.Storage.IndexPrefix, b.Key.Profile, b.Key.Vault)
		if err != nil {
			return fmt.Errorf("verify storage overrides: count prefixed for %s: %w", b.Key, err)
		}
		if prefixedCount > 0 {
			// Migration is either complete or in progress — operator is
			// fine. Don't second-guess.
			continue
		}
		sharedCount, err := count(ctx, defaultPrefix, b.Key.Profile, b.Key.Vault)
		if err != nil {
			return fmt.Errorf("verify storage overrides: count shared for %s: %w", b.Key, err)
		}
		if sharedCount > 0 {
			return fmt.Errorf(
				"server: binding %s has [storage_overrides] (prefix=%q) but %d docs exist on shared indices — run migration or revert config",
				b.Key, b.Storage.IndexPrefix, sharedCount,
			)
		}
	}
	return nil
}
