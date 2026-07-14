package main

import (
	"strings"
	"testing"
)

func TestRenderPlist_IntervalMode(t *testing.T) {
	interval := 900
	env := map[string]string{
		"PATH":               "/opt/homebrew/bin:/usr/bin",
		"CL_BRAIN_API_TOKEN": "tok&<danger>", // must be XML-escaped
	}
	args := []string{"/usr/local/bin/pbrainctl", "client", "mart", "sync", "taxes"}
	out := renderPlist("com.phantom-brain.mart.taxes", args, env, "/tmp/o.out", "/tmp/o.err", &interval, "")

	wants := []string{
		`<key>Label</key><string>com.phantom-brain.mart.taxes</string>`,
		`<key>StartInterval</key><integer>900</integer>`,
		`<string>/usr/local/bin/pbrainctl</string>`,
		`<string>sync</string>`,
		`<key>CL_BRAIN_API_TOKEN</key><string>tok&amp;&lt;danger&gt;</string>`, // escaped
		`<key>StandardOutPath</key><string>/tmp/o.out</string>`,
		`<key>RunAtLoad</key><true/>`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("plist missing %q\n---\n%s", w, out)
		}
	}
	if strings.Contains(out, "StartCalendarInterval") {
		t.Error("interval-mode plist should not have StartCalendarInterval")
	}
	// The raw (unescaped) token must never appear.
	if strings.Contains(out, "tok&<danger>") {
		t.Error("token was not XML-escaped")
	}
}

func TestRenderPlist_CalendarMode(t *testing.T) {
	out := renderPlist("com.phantom-brain.mart.taxes.reconcile",
		[]string{"pbrainctl", "client", "mart", "build", "taxes"},
		map[string]string{"PATH": "/bin"}, "/tmp/r.out", "/tmp/r.err", nil, "02:30")
	for _, w := range []string{
		"<key>StartCalendarInterval</key>",
		"<key>Hour</key><integer>2</integer>",
		"<key>Minute</key><integer>30</integer>",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("calendar plist missing %q\n%s", w, out)
		}
	}
	if strings.Contains(out, "StartInterval</key>") {
		t.Error("calendar-mode plist should not have StartInterval")
	}
}

func TestParseHHMM(t *testing.T) {
	ok := map[string][2]int{"00:00": {0, 0}, "9:5": {9, 5}, "23:59": {23, 59}}
	for in, want := range ok {
		h, m, err := parseHHMM(in)
		if err != nil || h != want[0] || m != want[1] {
			t.Errorf("parseHHMM(%q) = %d,%d,%v want %d:%d", in, h, m, err, want[0], want[1])
		}
	}
	for _, bad := range []string{"24:00", "12:60", "noon", "12", "-1:00", "12:-1"} {
		if _, _, err := parseHHMM(bad); err == nil {
			t.Errorf("parseHHMM(%q) should error", bad)
		}
	}
}
