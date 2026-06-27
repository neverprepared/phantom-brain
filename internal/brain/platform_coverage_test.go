package brain

import (
	"os"
	"regexp"
	"testing"
)

// TestRealPlatform_HostUUIDStableAndCached exercises the production
// host-detection path against the real host. The contract is: HostUUID
// resolves to a non-empty, stable value (machine-id / ioreg / hostname
// fallback) and is cached so repeated calls don't re-probe.
func TestRealPlatform_HostUUIDStableAndCached(t *testing.T) {
	p := NewPlatform()
	first, err := p.HostUUID()
	if err != nil {
		t.Fatalf("HostUUID: %v", err)
	}
	if first == "" {
		t.Fatal("HostUUID returned empty — contract is a non-empty stable id")
	}
	second, err := p.HostUUID()
	if err != nil {
		t.Fatalf("HostUUID (2nd): %v", err)
	}
	if first != second {
		t.Errorf("HostUUID not stable/cached: %q vs %q", first, second)
	}
}

func TestRealPlatform_BootIDNonEmptyAndCached(t *testing.T) {
	p := NewPlatform()
	first, err := p.BootID()
	if err != nil {
		t.Fatalf("BootID: %v", err)
	}
	if first == "" {
		t.Fatal("BootID returned empty")
	}
	if second, _ := p.BootID(); second != first {
		t.Errorf("BootID not cached: %q vs %q", first, second)
	}
}

func TestRealPlatform_HostnameMatchesOS(t *testing.T) {
	want, err := os.Hostname()
	if err != nil {
		t.Skipf("os.Hostname unavailable: %v", err)
	}
	got, err := NewPlatform().Hostname()
	if err != nil {
		t.Fatalf("Hostname: %v", err)
	}
	if got != want {
		t.Errorf("Hostname = %q, want %q", got, want)
	}
}

func TestRealPlatform_ProcessAlive(t *testing.T) {
	p := NewPlatform()
	if !p.ProcessAlive(os.Getpid()) {
		t.Error("our own pid should be alive")
	}
	if p.ProcessAlive(0) {
		t.Error("pid 0 must be reported not-alive")
	}
	if p.ProcessAlive(-1) {
		t.Error("negative pid must be reported not-alive")
	}
}

func TestRealPlatform_InContainerReturnsBool(t *testing.T) {
	// We can't assert a specific value (depends on the runner), but the
	// probe must not panic and must return deterministically across calls.
	p := NewPlatform()
	if p.InContainer() != p.InContainer() {
		t.Error("InContainer flipped between calls — probe is not deterministic")
	}
}

// TestDetectHelpers covers the package-level detect* functions directly.
func TestDetectHelpers(t *testing.T) {
	host, err := detectHostUUID()
	if err != nil || host == "" {
		t.Errorf("detectHostUUID: id=%q err=%v", host, err)
	}
	boot, err := detectBootID()
	if err != nil || boot == "" {
		t.Errorf("detectBootID: id=%q err=%v", boot, err)
	}
}

// TestFallbackHostID asserts the deterministic hostname-derived id: a
// 32-hex-char digest, stable across calls.
func TestFallbackHostID(t *testing.T) {
	a, err := fallbackHostID()
	if err != nil {
		t.Fatalf("fallbackHostID: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(a) {
		t.Errorf("fallback id not 32 hex chars: %q", a)
	}
	b, _ := fallbackHostID()
	if a != b {
		t.Errorf("fallbackHostID not deterministic: %q vs %q", a, b)
	}
}

// TestExtractIOPlatformUUID_MalformedBranches covers the missing-"=" and
// missing-closing-quote branches the existing happy-path test skips.
func TestExtractIOPlatformUUID_MalformedBranches(t *testing.T) {
	cases := map[string]string{
		"no equals after key": `"IOPlatformUUID"  no-eq-here`,
		"no opening quote":    `"IOPlatformUUID" = no-quote`,
		"no closing quote":    `"IOPlatformUUID" = "DEADBEEF-unterminated`,
		"key absent":          `"SomethingElse" = "x"`,
	}
	for name, in := range cases {
		if got := extractIOPlatformUUID(in); got != "" {
			t.Errorf("%s: expected empty, got %q", name, got)
		}
	}
}
