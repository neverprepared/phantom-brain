package server

import (
	"context"
	"fmt"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// brain_reflect maintenance cycle (issue #72, Phase 1).
//
// Phase 1 ships the propose-then-apply skeleton:
//   - brain_reflect REPORTS forget-candidate SHAs (read-only).
//   - brain_forget APPLIES a single forget (delete + snapshot rebuild).
//
// The SHA is the handle — there is no separate candidate-ID system.
// Phase 1 implements ONE detector: stale-gate (summaries the synth
// gate never enriched, i.e. Synthesised == false). Richer detectors
// (orphan entities, near-duplicates) and a bulk-apply path are
// deferred to Phase 2 — see issue #72.

// ReflectCandidate is one forget-candidate surfaced by a detector.
// JSON tags mirror the brain-client mirror type in internal/brain.
type ReflectCandidate struct {
	SHA    string `json:"sha"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// summaryScroller is the slice of osearch the reflect detectors need:
// walk every pb_summaries doc for a (profile, vault). Defined as an
// interface so staleGateCandidates is unit-testable with an in-memory
// fake — mirrors the verifyStorageOverridesWith injection style in
// storage_overrides_check.go.
type summaryScroller interface {
	ScrollSummaries(ctx context.Context, profile, vault string, batchSize int, fn func(osearch.SummaryDoc) error) error
}

// staleGateCandidates scrolls every summary for (profile, vault) and
// collects those the synth gate never enriched (Synthesised == false).
// Pure + injectable — the production caller passes the binding's
// osWriter view; tests pass a fake slice-backed scroller.
//
// Phase 2 will add additional detectors (orphan-entity, near-dup) and
// merge their outputs here behind a detector-set abstraction.
func staleGateCandidates(ctx context.Context, sc summaryScroller, profile, vault string) ([]ReflectCandidate, error) {
	var out []ReflectCandidate
	err := sc.ScrollSummaries(ctx, profile, vault, 0, func(doc osearch.SummaryDoc) error {
		if !doc.Synthesised {
			out = append(out, ReflectCandidate{
				SHA:    doc.SHA,
				Title:  doc.Title,
				Reason: "stale-gate: never synthesised",
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reflect: scroll summaries: %w", err)
	}
	return out, nil
}
