// Package vault contains the file-layout primitives shared by every
// pbrainctl tool that reads or writes the vault: parsing YAML
// frontmatter, generating slugs, atomic file writes, and creating the
// per-brain directory skeleton.
//
// This package does NOT include canonicalization (see
// internal/canonicalize for SHA-stable byte form), the vector or FTS
// indexes (internal/index), or working memory (internal/working).
package vault

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Document is the parsed representation of a single markdown file with
// optional YAML frontmatter.
//
// Frontmatter is a Go map so callers can read and mutate it directly.
// Render() emits keys in alphabetical order (top-level and nested) so
// the wire form is deterministic regardless of caller mutation order.
// If you need raw byte-stable hashing (e.g. for SHA256 dedup), do not
// rely on Document.Render() — use internal/canonicalize.Canonicalize.
type Document struct {
	Frontmatter map[string]any
	Body        string
}

// Parse reads markdown with optional YAML frontmatter into a Document.
// Line endings are normalized to LF. An empty input yields an empty
// Document with no frontmatter. Unterminated frontmatter (an opening
// "---\n" with no matching "---\n" closer) is treated as body — same
// rule as canonicalize, so the two stay aligned.
func Parse(raw []byte) (*Document, error) {
	normalized := normalizeLineEndings(raw)
	fmBytes, body := splitFrontmatter(normalized)

	doc := &Document{Body: string(body)}
	if fmBytes != nil {
		fm := make(map[string]any)
		if err := yaml.Unmarshal(fmBytes, &fm); err != nil {
			return nil, fmt.Errorf("vault: parse frontmatter: %w", err)
		}
		doc.Frontmatter = fm
	}
	return doc, nil
}

// MustParse is Parse but panics on error. Useful in tests; do not use
// in production code paths.
func MustParse(raw []byte) *Document {
	d, err := Parse(raw)
	if err != nil {
		panic(err)
	}
	return d
}

// Render emits the document as bytes. Frontmatter keys are emitted in
// alphabetical order at every depth so the output is deterministic.
// A document with no frontmatter renders as just the body (no leading
// "---" block). A document with an empty body renders as just the
// frontmatter block.
//
// Body always ends with exactly one LF (added if missing, collapsed if
// multiple). Empty body emits nothing trailing the frontmatter block.
func (d *Document) Render() ([]byte, error) {
	var out bytes.Buffer

	if len(d.Frontmatter) > 0 {
		sortedYAML, err := marshalSorted(d.Frontmatter)
		if err != nil {
			return nil, fmt.Errorf("vault: render frontmatter: %w", err)
		}
		out.WriteString("---\n")
		out.Write(sortedYAML)
		out.WriteString("---\n")
	}

	body := d.Body
	if body != "" {
		// Leading newlines between frontmatter and body collapse to none.
		body = strings.TrimLeft(body, "\n")
		// Trailing newlines collapse to exactly one.
		body = strings.TrimRight(body, "\n") + "\n"
		out.WriteString(body)
	}
	return out.Bytes(), nil
}

// FrontmatterString returns d.Frontmatter[key] as a string, or "" if
// the key is missing or the value is a non-string type. Convenience
// for the common case of reading title/source_url/etc.
func (d *Document) FrontmatterString(key string) string {
	if d.Frontmatter == nil {
		return ""
	}
	v, ok := d.Frontmatter[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// FrontmatterStrings returns d.Frontmatter[key] as a []string. Handles
// both YAML sequence-of-strings and the YAML "tags: ['a', 'b']" inline
// form. Returns nil if the key is missing or unsuitable.
func (d *Document) FrontmatterStrings(key string) []string {
	if d.Frontmatter == nil {
		return nil
	}
	v, ok := d.Frontmatter[key]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// --- shared helpers ---

func normalizeLineEndings(raw []byte) []byte {
	out := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(out, []byte("\r"), []byte("\n"))
}

// splitFrontmatter mirrors canonicalize.splitFrontmatter exactly so a
// document the canonicalize package treats as body-only will parse as
// body-only here too. Duplicated rather than imported to keep this
// package free of internal/canonicalize as a dependency.
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
	if bytes.HasSuffix(rest, []byte(closerEOF)) {
		return rest[:len(rest)-len(closerEOF)], nil
	}
	return nil, raw
}

// marshalSorted emits a map[string]any as YAML with all mapping-node
// children alphabetically sorted at every depth, regardless of Go map
// iteration order. Without this, yaml.v3 emits keys in randomized
// order because Go map iteration is intentionally non-deterministic.
func marshalSorted(m map[string]any) ([]byte, error) {
	if m == nil {
		return nil, errors.New("nil map")
	}
	node, err := toSortedNode(m)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// toSortedNode builds a yaml.Node for v with deterministic mapping
// order. Nested maps recurse; sequences preserve their original index
// order; scalars round-trip via yaml.v3 default encoding.
func toSortedNode(v any) (*yaml.Node, error) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, k := range keys {
			child, err := toSortedNode(t[k])
			if err != nil {
				return nil, err
			}
			keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}
			n.Content = append(n.Content, keyNode, child)
		}
		return n, nil
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, e := range t {
			child, err := toSortedNode(e)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, child)
		}
		return n, nil
	default:
		// Round-trip through yaml.v3's default scalar encoding so we
		// don't have to reproduce its type-tagging rules manually.
		buf, err := yaml.Marshal(v)
		if err != nil {
			return nil, err
		}
		var n yaml.Node
		if err := yaml.Unmarshal(buf, &n); err != nil {
			return nil, err
		}
		// Document node wraps a single scalar; unwrap.
		if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
			return n.Content[0], nil
		}
		return &n, nil
	}
}
