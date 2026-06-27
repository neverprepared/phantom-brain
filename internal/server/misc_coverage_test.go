package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neverprepared/phantom-brain/internal/osearch"
)

// --- config.go ----------------------------------------------------

func TestDefaultConfigDir(t *testing.T) {
	t.Setenv("PHANTOM_BRAIN_CONFIG_DIR", "/custom/cfg")
	if got := DefaultConfigDir(); got != "/custom/cfg" {
		t.Errorf("env override = %q, want /custom/cfg", got)
	}
	// Whitespace-only env is ignored → falls back to home-relative.
	t.Setenv("PHANTOM_BRAIN_CONFIG_DIR", "   ")
	if got := DefaultConfigDir(); !strings.HasSuffix(got, filepath.Join(".config", "phantom-brain-server")) {
		t.Errorf("blank env should fall back to home default, got %q", got)
	}
}

func TestOpenSearchConfig_Enabled(t *testing.T) {
	var off OpenSearchConfig
	if off.Enabled() {
		t.Error("empty addresses should be disabled")
	}
	on := OpenSearchConfig{Addresses: []string{"http://os:9200"}}
	if !on.Enabled() {
		t.Error("non-empty addresses should be enabled")
	}
}

func TestMergedDefaults_AllFieldsOverride(t *testing.T) {
	global := VaultDefaults{
		ReaperPollIntervalSecs:       5,
		MaxTarballBytes:              100,
		MaxUncompressedBytes:         200,
		ContributorQuotaBytesPerHour: 300,
	}
	over := VaultOverrides{
		ReaperPollIntervalSecs:       9,
		MaxTarballBytes:              111,
		MaxUncompressedBytes:         222,
		ContributorQuotaBytesPerHour: 333,
	}
	out := MergedDefaults(global, over)
	if out.ReaperPollIntervalSecs != 9 || out.MaxTarballBytes != 111 ||
		out.MaxUncompressedBytes != 222 || out.ContributorQuotaBytesPerHour != 333 {
		t.Errorf("all nonzero overrides should apply, got %+v", out)
	}
	// Zero overrides leave global in place.
	out2 := MergedDefaults(global, VaultOverrides{})
	if out2 != global {
		t.Errorf("empty overrides should leave global, got %+v", out2)
	}
}

// --- paths.go -----------------------------------------------------

func TestDefaultDataDir(t *testing.T) {
	t.Setenv("PHANTOM_BRAIN_DATA_DIR", "/data/root")
	if got := DefaultDataDir(); got.String() != "/data/root" {
		t.Errorf("env override = %q, want /data/root", got.String())
	}
	t.Setenv("PHANTOM_BRAIN_DATA_DIR", "  ")
	if got := DefaultDataDir(); got.String() != "/var/lib/phantom-brain" {
		t.Errorf("blank env should fall back to production path, got %q", got.String())
	}
}

func TestEnsureDaemonSkeleton(t *testing.T) {
	d := DataDir(t.TempDir())
	if err := EnsureDaemonSkeleton(d); err != nil {
		t.Fatalf("EnsureDaemonSkeleton: %v", err)
	}
	info, err := os.Stat(d.DaemonLocksDir())
	if err != nil {
		t.Fatalf("locks dir should exist: %v", err)
	}
	if !info.IsDir() {
		t.Error("daemon locks path should be a directory")
	}
}

// --- registry.go --------------------------------------------------

func TestValidateStorageOverridePrefix(t *testing.T) {
	valid := []string{"", "client_x_", "abc123", "a_b_c", "9", "_"}
	for _, p := range valid {
		if err := ValidateStorageOverridePrefix(p); err != nil {
			t.Errorf("prefix %q should be valid, got %v", p, err)
		}
	}
	invalid := []string{"Client_x", "has-dash", "has space", "semi;colon", "UPPER", "a.b", "x/y"}
	for _, p := range invalid {
		if err := ValidateStorageOverridePrefix(p); err == nil {
			t.Errorf("prefix %q should be rejected", p)
		}
	}
}

// --- synth_queue.go small helpers ---------------------------------

func TestGateSourceType(t *testing.T) {
	curated := &osearch.SummaryDoc{
		Reliability: osearch.ReliabilityMedium,
		GateReason:  "curated note",
	}
	if got := gateSourceType(curated); got != "curated" {
		t.Errorf("curated medium doc should be %q, got %q", "curated", got)
	}
	// Medium reliability but no curated reason → gathered.
	notCurated := &osearch.SummaryDoc{
		Reliability: osearch.ReliabilityMedium,
		GateReason:  "auto",
	}
	if got := gateSourceType(notCurated); got != "gathered" {
		t.Errorf("non-curated reason should be gathered, got %q", got)
	}
	// Curated reason but wrong reliability → gathered.
	wrongRel := &osearch.SummaryDoc{
		Reliability: osearch.ReliabilityHigh,
		GateReason:  "curated",
	}
	if got := gateSourceType(wrongRel); got != "gathered" {
		t.Errorf("non-medium reliability should be gathered, got %q", got)
	}
	if got := gateSourceType(&osearch.SummaryDoc{}); got != "gathered" {
		t.Errorf("empty doc should be gathered, got %q", got)
	}
}

func TestIsImageExt(t *testing.T) {
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".bmp", ".tif", ".tiff", ".webp"} {
		if !isImageExt(ext) {
			t.Errorf("%q should be an image ext", ext)
		}
	}
	for _, ext := range []string{".pdf", ".txt", ".docx", "", ".PNG"} {
		if isImageExt(ext) {
			t.Errorf("%q should not be an image ext", ext)
		}
	}
}

// --- entities_llm.go ----------------------------------------------

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short string should be unchanged, got %q", got)
	}
	if got := truncate("hello", 5); got != "hello" {
		t.Errorf("exact-length string should be unchanged, got %q", got)
	}
	got := truncate("hello world", 5)
	if got != "hello..." {
		t.Errorf("over-length string should truncate + ellipsis, got %q", got)
	}
}
