package server

import "testing"

func TestValidateMemoryFields_AcceptsKnown(t *testing.T) {
	cases := []MemoryFields{
		{},                                                          // all empty — caller didn't classify
		{Kind: "note"},
		{Kind: "web_scrape", MemoryType: "semantic"},
		{Kind: "task_summary", MemoryType: "episodic", Source: []string{"task:42"}},
		{Kind: "attachment_stub", MemoryType: "procedural"},
		{Kind: "email_import"},
		{Kind: "manual_curate"},
	}
	for _, c := range cases {
		if msg := validateMemoryFields(c); msg != "" {
			t.Errorf("validateMemoryFields(%+v) rejected: %s", c, msg)
		}
	}
}

func TestValidateMemoryFields_RejectsUnknown(t *testing.T) {
	cases := []MemoryFields{
		{Kind: "experiment"},      // typo / new value
		{Kind: "Note"},            // wrong case
		{Kind: "web-scrape"},      // wrong separator
		{MemoryType: "implicit"},  // unknown taxonomy bucket
		{MemoryType: "Semantic"},  // wrong case
	}
	for _, c := range cases {
		if msg := validateMemoryFields(c); msg == "" {
			t.Errorf("validateMemoryFields(%+v) accepted; expected rejection", c)
		}
	}
}
