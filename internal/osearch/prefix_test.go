package osearch

import (
	"context"
	"testing"
	"time"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func TestIndexNameWithPrefix_Pure(t *testing.T) {
	cases := []struct {
		prefix, logical, want string
	}{
		{"", IndexSummaries, "pb_summaries"},
		{"test_", IndexSummaries, "test_pb_summaries"},
		{"client_x_", IndexEntities, "client_x_pb_entities"},
		{"a_b_", IndexAttachments, "a_b_pb_attachments"},
	}
	for _, tc := range cases {
		if got := IndexNameWithPrefix(tc.prefix, tc.logical); got != tc.want {
			t.Errorf("IndexNameWithPrefix(%q, %q) = %q, want %q", tc.prefix, tc.logical, got, tc.want)
		}
	}
}

func TestClient_DefaultPrefix(t *testing.T) {
	c := &Client{prefix: "pinned_"}
	if got := c.DefaultPrefix(); got != "pinned_" {
		t.Errorf("DefaultPrefix() = %q, want pinned_", got)
	}
	// Legacy IndexName still honors the stored prefix.
	if got := c.IndexName(IndexSummaries); got != "pinned_pb_summaries" {
		t.Errorf("IndexName legacy = %q, want pinned_pb_summaries", got)
	}
}

// TestLive_EnsurePrefixes_IsIdempotent runs against a real OS and is
// the only way to exercise the create path. Each call to
// EnsureIndicesWithPrefix on the same prefix must succeed without
// error; a second daemon startup against an already-populated cluster
// is the production case we have to keep cheap.
func TestLive_EnsurePrefixes_IsIdempotent(t *testing.T) {
	c, ctx, cleanup := testClient(t)
	defer cleanup()

	// Phase D2b: EnsurePrefixes was removed; the per-prefix create path is
	// the retained ensureIndex primitive (via EnsureIndexWithMapping),
	// driven here by the ensureLegacyIndices test helper. Idempotency is the
	// invariant under test — a second pass must short-circuit on the exists
	// probe rather than re-create.
	for _, p := range []string{c.prefix + "a_", c.prefix + "b_"} {
		ensureLegacyIndices(t, ctx, c, p)
	}
	// Re-run to prove idempotency.
	for _, p := range []string{c.prefix + "a_", c.prefix + "b_"} {
		ensureLegacyIndices(t, ctx, c, p)
	}

	// Drop the per-prefix indices we created so cleanup is honest.
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, p := range []string{c.prefix + "a_", c.prefix + "b_"} {
			names := []string{
				IndexNameWithPrefix(p, IndexSummaries),
				IndexNameWithPrefix(p, IndexEntities),
				IndexNameWithPrefix(p, IndexAttachments),
			}
			_, _ = c.api.Indices.Delete(dctx, osapi.IndicesDeleteReq{Indices: names})
		}
	})
}

// TestLive_PerPrefixRouting confirms that a doc written via
// UpsertSummaryWithPrefix lands in the prefixed physical index and is
// invisible to a Get against the default prefix. This is the core
// isolation guarantee for Level 2 storage overrides.
func TestLive_PerPrefixRouting(t *testing.T) {
	c, ctx, cleanup := testClient(t)
	defer cleanup()

	overridePrefix := c.prefix + "ovr_"
	ensureLegacyIndices(t, ctx, c, overridePrefix)
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		names := []string{
			IndexNameWithPrefix(overridePrefix, IndexSummaries),
			IndexNameWithPrefix(overridePrefix, IndexEntities),
			IndexNameWithPrefix(overridePrefix, IndexAttachments),
		}
		_, _ = c.api.Indices.Delete(dctx, osapi.IndicesDeleteReq{Indices: names})
	})

	doc := SummaryDoc{
		Profile:     "pp",
		Vault:       "vv",
		SHA:         "router-test-sha",
		Title:       "router test",
		Body:        "isolation check",
		Kind:        KindNote,
		Reliability: ReliabilityMedium,
		Synthesised: true,
	}
	if err := c.UpsertSummaryWithPrefix(ctx, overridePrefix, doc, true); err != nil {
		t.Fatalf("UpsertSummaryWithPrefix: %v", err)
	}

	// Fetch via the same prefix — must find it.
	got, err := c.GetSummaryWithPrefix(ctx, overridePrefix, "pp", "vv", "router-test-sha")
	if err != nil {
		t.Fatalf("GetSummaryWithPrefix override: %v", err)
	}
	if got == nil {
		t.Fatalf("doc not found via override prefix; isolation broken (write didn't route)")
	}
	if got.Title != "router test" {
		t.Errorf("title = %q, want router test", got.Title)
	}

	// Fetch via the default prefix — must NOT find it. That's the
	// whole point of per-binding overrides.
	def, err := c.GetSummaryWithPrefix(ctx, c.prefix, "pp", "vv", "router-test-sha")
	if err != nil {
		t.Fatalf("GetSummaryWithPrefix default: %v", err)
	}
	if def != nil {
		t.Errorf("doc unexpectedly visible via default prefix; per-prefix isolation broken")
	}
}
