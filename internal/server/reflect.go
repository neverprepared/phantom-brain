package server

import (
	"context"
	"fmt"

	"github.com/neverprepared/phantom-brain/internal/pgstore/pgdb"
)

// brain_reflect maintenance cycle (issue #72, Phase 1).
//
// Phase 1 ships the propose-then-apply skeleton:
//   - brain_reflect REPORTS forget-candidate SHAs (read-only).
//   - brain_forget APPLIES a single forget (delete from the SoR).
//
// The SHA is the handle — there is no separate candidate-ID system.
// Phase 1 implements ONE detector: stale-gate (records the synth gate
// never enriched, i.e. synthesised == false). Richer detectors (orphan
// entities, near-duplicates) and a bulk-apply path are deferred to
// Phase 2 — see issue #72.
//
// Phase D: the detector scans the Postgres System-of-Record (records)
// via pgdb.ListUnsynthesised — the legacy pb_summaries index is no
// longer written, so a scroll over it returned an empty report forever.

// ReflectCandidate is one forget-candidate surfaced by a detector.
// JSON tags mirror the brain-client mirror type in internal/brain.
type ReflectCandidate struct {
	SHA    string `json:"sha"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// unsynthScanner is the slice of the SoR query set the reflect detector
// needs: list every record the synth gate never enriched for a
// (profile, vault). *pgdb.Queries satisfies it directly (the production
// caller passes pgstore.New(view.Pool())); tests inject a slice-backed
// fake — mirrors the verifyStorageOverridesWith injection style in
// storage_overrides_check.go.
type unsynthScanner interface {
	ListUnsynthesised(ctx context.Context, arg pgdb.ListUnsynthesisedParams) ([]pgdb.Record, error)
}

// staleGateCandidates lists the Synthesised=false backlog for
// (profile, vault) from the Postgres SoR and maps each row to a
// forget-candidate. The SQL already filters NOT synthesised, so every
// returned row IS a candidate. Pure + injectable — production passes
// the binding's *pgdb.Queries; tests pass a slice-backed fake.
//
// Phase 2 will add additional detectors (orphan-entity, near-dup) and
// merge their outputs here behind a detector-set abstraction.
func staleGateCandidates(ctx context.Context, sc unsynthScanner, profile, vault string) ([]ReflectCandidate, error) {
	recs, err := sc.ListUnsynthesised(ctx, pgdb.ListUnsynthesisedParams{
		Profile: profile,
		Vault:   vault,
		Lim:     resynthScanLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("reflect: list unsynthesised: %w", err)
	}
	out := make([]ReflectCandidate, 0, len(recs))
	for _, rec := range recs {
		out = append(out, ReflectCandidate{
			SHA:    rec.Sha,
			Title:  rec.Title,
			Reason: "stale-gate: never synthesised",
		})
	}
	return out, nil
}
