// Package vaultxfer is the canonical round-trip vault format shared by
// `pbrainctl client export` and `pbrainctl client import`. Export and import
// MUST be true inverses, so the encode/decode boundary lives here (one place)
// rather than in each command.
//
// One file per record:
//
//	---
//	<metadata YAML>
//	---
//	<raw body — verbatim, byte-for-byte, starting at the very next byte>
//
// The body is the record's RAW body (pre-synthesis), written with NO injected
// leading blank line and NO appended trailing newline. That matters because the
// daemon derives a record's identity from the body via internal/canonicalize:
// canonicalize is robust to trailing whitespace and key ordering, but a leading
// blank line before a body that itself begins with `---` (e.g. a skill's own
// frontmatter) would flip frontmatter detection and change the SHA. Preserving
// the body region exactly lets a re-import recompute the same identity and dedup.
//
// The metadata block is informational + reconstruction fields (kind, tags, …)
// that import replays onto the record; it is NOT part of the record's identity
// (the daemon re-derives that from the body). `sha` is a checksum only.
package vaultxfer

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Meta is the per-record frontmatter. Field order here is the emit order.
type Meta struct {
	SHA         string   `yaml:"sha,omitempty"`
	Kind        string   `yaml:"kind,omitempty"`
	Title       string   `yaml:"title,omitempty"`
	Tags        []string `yaml:"tags,omitempty"`
	Topic       string   `yaml:"topic,omitempty"`
	Reliability string   `yaml:"reliability,omitempty"`
	MemoryType  string   `yaml:"memory_type,omitempty"`
	CapturedAt  string   `yaml:"captured_at,omitempty"`
	Source      []string `yaml:"source,omitempty"`
	SourceURL   string   `yaml:"source_url,omitempty"`
}

// Encode renders one record file: metadata frontmatter, then the body verbatim.
func Encode(m Meta, body string) ([]byte, error) {
	var y bytes.Buffer
	enc := yaml.NewEncoder(&y)
	enc.SetIndent(2)
	if err := enc.Encode(&m); err != nil {
		return nil, fmt.Errorf("vaultxfer: marshal meta: %w", err)
	}
	_ = enc.Close()

	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(y.Bytes()) // yaml ends with a newline
	out.WriteString("---\n")
	out.WriteString(body) // verbatim: no leading blank line, no trailing newline added
	return out.Bytes(), nil
}

// Decode splits a record file into its metadata and the verbatim body. The body
// is every byte after the FIRST frontmatter block's closing fence — untouched,
// even if it begins with its own `---` frontmatter.
func Decode(raw []byte) (Meta, string, error) {
	const opener = "---\n"
	const closer = "\n---\n"
	if !bytes.HasPrefix(raw, []byte(opener)) {
		return Meta{}, "", fmt.Errorf("vaultxfer: no leading frontmatter")
	}
	rest := raw[len(opener):]
	idx := bytes.Index(rest, []byte(closer))
	if idx < 0 {
		return Meta{}, "", fmt.Errorf("vaultxfer: unterminated frontmatter")
	}
	var m Meta
	if err := yaml.Unmarshal(rest[:idx], &m); err != nil {
		return Meta{}, "", fmt.Errorf("vaultxfer: parse meta: %w", err)
	}
	body := string(rest[idx+len(closer):]) // verbatim
	return m, body, nil
}

var _slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

// Filename is the on-disk name for a record: a slug of its name plus a short SHA
// so same-named records with different content never collide. Identity is the
// SHA, not the filename.
func Filename(name, sha string) string {
	slug := strings.Trim(_slugStrip.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if slug == "" {
		slug = "record"
	}
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	if short == "" {
		return slug + ".md"
	}
	return slug + "-" + short + ".md"
}
