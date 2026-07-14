package mart

import (
	"testing"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// TestAttachmentKindMatchesSoR pins the mart's attachment-detection constant to
// the kind string the daemon actually writes to the Postgres SoR (and returns
// from GET /api/brain/records). The first cut used "attachment_stub" (the
// osearch enum), but osearch.SoRKind collapses that to "attachment" on write,
// so blob materialization silently no-op'd against real data. If SoRKind ever
// changes the attachment kind again, this fails instead of shipping a dud.
func TestAttachmentKindMatchesSoR(t *testing.T) {
	want := osearch.SoRKind(osearch.KindAttachmentStub)
	if attachmentKind != want {
		t.Fatalf("attachmentKind = %q, but the SoR stores %q — attachment blob materialization would break", attachmentKind, want)
	}
}
