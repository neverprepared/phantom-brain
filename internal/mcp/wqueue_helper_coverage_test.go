package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/neverprepared/phantom-brain/internal/brain"
)

// TestHumanizeAge covers every unit bucket plus the negative-clamp guard.
func TestHumanizeAge(t *testing.T) {
	cases := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"negative-clamps-to-zero", -5 * time.Second, "0s"},
		{"sub-minute", 42 * time.Second, "42s"},
		{"minute-boundary", time.Minute, "1m"},
		{"minutes", 5 * time.Minute, "5m"},
		{"hour-boundary", time.Hour, "1h"},
		{"hours", 3 * time.Hour, "3h"},
		{"day-boundary", 24 * time.Hour, "1d"},
		{"days", 50 * time.Hour, "2d"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := humanizeAge(c.d); got != c.want {
				t.Errorf("humanizeAge(%s) = %q, want %q", c.d, got, c.want)
			}
		})
	}
}

// TestFormatQueueNotice_ProcessStart: with no prior success the notice
// reads "since process start" and pluralizes by depth.
func TestFormatQueueNotice_ProcessStart(t *testing.T) {
	notice := formatQueueNotice(brain.ConnectivitySnapshot{}, 3)
	if !strings.Contains(notice, "since process start") {
		t.Errorf("expected process-start phrasing, got: %q", notice)
	}
	if !strings.Contains(notice, "3 writes pending sync") {
		t.Errorf("expected plural writes, got: %q", notice)
	}
}

// TestFormatQueueNotice_SinceLastSuccess: a prior success produces a
// "since <age> ago" outage window, and depth==1 renders the singular
// "write".
func TestFormatQueueNotice_SinceLastSuccess(t *testing.T) {
	snap := brain.ConnectivitySnapshot{
		LastSuccessAt: time.Now().Add(-90 * time.Second),
	}
	notice := formatQueueNotice(snap, 1)
	if !strings.Contains(notice, "since 1m ago") {
		t.Errorf("expected '1m ago' window, got: %q", notice)
	}
	if !strings.Contains(notice, "1 write pending sync") || strings.Contains(notice, "writes") {
		t.Errorf("expected singular 'write', got: %q", notice)
	}
}
