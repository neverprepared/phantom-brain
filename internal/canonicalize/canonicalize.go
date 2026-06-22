// Package canonicalize defines the byte-stable canonical form used for
// SHA256 content-dedup across phantom-brain.
//
// The canonical form is a syntactic normalization, not a semantic one.
// Two inputs that differ only in line endings, trailing whitespace,
// frontmatter key ordering, or trailing blank lines produce identical
// canonical bytes. Inputs that differ in word choice, paragraph order,
// or markdown structure produce different canonical bytes — semantic
// dedup is the synthesizer's job, not this package's.
//
// Transformations applied, in order:
//
//  1. Line endings: CRLF -> LF, lone CR -> LF. Applied to the entire
//     input before any structural parsing so the rest of the pipeline
//     can treat \n as the only line terminator.
//
//  2. YAML frontmatter (if present at byte 0): parsed into a yaml.Node
//     tree, all MappingNode children recursively sorted alphabetically
//     by key, then re-marshaled. Two documents with the same key/value
//     pairs in different orders canonicalize identically.
//
//  3. Body: each line has trailing spaces and tabs stripped.
//
//  4. Trailing newlines: collapsed so the body section ends with
//     exactly one LF (when there is a body). Pure-frontmatter documents
//     emit no body section.
//
// Invariant: Canonicalize is idempotent. Canonicalize(Canonicalize(x))
// always equals Canonicalize(x).
package canonicalize

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Canonicalize returns the canonical byte form of raw markdown.
// See the package doc for the transformation rules.
func Canonicalize(raw []byte) ([]byte, error) {
	// Step 1: normalize line endings up front.
	normalized := normalizeLineEndings(raw)

	fmBytes, bodyBytes := splitFrontmatter(normalized)

	var out bytes.Buffer

	if fmBytes != nil {
		sorted, err := sortYAMLBytes(fmBytes)
		if err != nil {
			return nil, fmt.Errorf("canonicalize: %w", err)
		}
		out.WriteString("---\n")
		out.Write(sorted)
		out.WriteString("---\n")
	}

	body := string(bodyBytes)
	// A frontmatter block is followed by zero or more blank lines before
	// the body proper. Collapse them so canonical form has at most one
	// blank line separator (or none).
	body = strings.TrimLeft(body, "\n")

	if body != "" {
		out.WriteString(canonicalizeBody(body))
	}

	return out.Bytes(), nil
}

// Sum returns the hex-encoded SHA256 of the canonical form. Equivalent
// to running Canonicalize then sha256, but kept as a single call so
// callers don't have to materialize the intermediate buffer twice.
func Sum(raw []byte) (string, error) {
	canon, err := Canonicalize(raw)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(canon)
	return hex.EncodeToString(h[:]), nil
}

// SumBody returns the hex-encoded SHA256 of just the canonical BODY,
// excluding any frontmatter. Use this when you want a content-stable
// fingerprint: two documents with identical bodies but different
// frontmatter (e.g. ingestion timestamps) produce the same SumBody,
// where Sum would diverge.
//
// Phase 6: ingest paths key dedup off SumBody so re-perceiving the
// same content across a wall-clock second boundary (which would
// produce different `gathered_at` / `learned_at` / `attached_at`
// stamps at RFC3339 precision) still dedups correctly.
//
// Edge cases: a document with no frontmatter hashes identically
// under Sum and SumBody. An empty body hashes the empty string.
func SumBody(raw []byte) (string, error) {
	normalized := normalizeLineEndings(raw)
	_, bodyBytes := splitFrontmatter(normalized)
	body := strings.TrimLeft(string(bodyBytes), "\n")
	canon := canonicalizeBody(body)
	h := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(h[:]), nil
}

func normalizeLineEndings(raw []byte) []byte {
	out := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	out = bytes.ReplaceAll(out, []byte("\r"), []byte("\n"))
	return out
}

// splitFrontmatter recognizes the "---\n...---\n" YAML frontmatter block
// at byte 0. Returns (frontmatterBytes, bodyBytes) where neither
// includes the --- delimiters. If no frontmatter is present returns
// (nil, raw). A missing closing delimiter is treated as "no frontmatter"
// so we never accidentally canonicalize an entire document into the
// frontmatter slot.
func splitFrontmatter(raw []byte) ([]byte, []byte) {
	const opener = "---\n"
	const closer = "\n---\n"
	const closerEOF = "\n---"

	if !bytes.HasPrefix(raw, []byte(opener)) {
		return nil, raw
	}
	rest := raw[len(opener):]

	if idx := bytes.Index(rest, []byte(closer)); idx >= 0 {
		return rest[:idx], rest[idx+len(closer):]
	}
	// Frontmatter that runs to end-of-file with no trailing newline.
	if bytes.HasSuffix(rest, []byte(closerEOF)) {
		return rest[:len(rest)-len(closerEOF)], nil
	}
	// Unterminated frontmatter: refuse to treat any of this as frontmatter.
	return nil, raw
}

// sortYAMLBytes parses fm as YAML, recursively sorts all mapping keys,
// and re-marshals. Preserves scalar values and sequence order exactly;
// only mapping order changes.
func sortYAMLBytes(fm []byte) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(fm, &root); err != nil {
		return nil, fmt.Errorf("invalid frontmatter: %w", err)
	}
	sortNodeMappings(&root)

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, fmt.Errorf("re-marshal frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("re-marshal frontmatter: %w", err)
	}
	return out.Bytes(), nil
}

// sortNodeMappings walks a yaml.Node tree and reorders every
// MappingNode's Content slice so its (key, value) pairs appear in
// alphabetical order by scalar key. Recurses into all children.
func sortNodeMappings(n *yaml.Node) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode, yaml.AliasNode:
		for _, c := range n.Content {
			sortNodeMappings(c)
		}
	case yaml.MappingNode:
		pairs := len(n.Content) / 2
		if pairs == 0 {
			return
		}
		idx := make([]int, pairs)
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool {
			return n.Content[idx[a]*2].Value < n.Content[idx[b]*2].Value
		})
		sorted := make([]*yaml.Node, 0, len(n.Content))
		for _, i := range idx {
			sorted = append(sorted, n.Content[i*2], n.Content[i*2+1])
		}
		n.Content = sorted
		for i := 0; i < pairs; i++ {
			sortNodeMappings(n.Content[i*2+1])
		}
	}
}

func canonicalizeBody(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	joined := strings.Join(lines, "\n")
	// Collapse trailing newlines to exactly one.
	joined = strings.TrimRight(joined, "\n") + "\n"
	return joined
}
