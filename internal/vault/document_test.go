package vault

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestParseEmpty(t *testing.T) {
	d, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Body != "" {
		t.Errorf("body = %q, want empty", d.Body)
	}
	if d.Frontmatter != nil {
		t.Errorf("frontmatter = %v, want nil", d.Frontmatter)
	}
}

func TestParsePlainBody(t *testing.T) {
	d, err := Parse([]byte("hello world\n"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Body != "hello world\n" {
		t.Errorf("body = %q", d.Body)
	}
	if d.Frontmatter != nil {
		t.Errorf("frontmatter = %v, want nil", d.Frontmatter)
	}
}

func TestParseCRLFNormalized(t *testing.T) {
	d, err := Parse([]byte("hello\r\nworld\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Body != "hello\nworld\n" {
		t.Errorf("body should normalize CRLF; got %q", d.Body)
	}
}

func TestParseFrontmatterAndBody(t *testing.T) {
	in := "---\ntitle: hello\ntags:\n  - a\n  - b\n---\nbody line\n"
	d, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if d.FrontmatterString("title") != "hello" {
		t.Errorf("title = %q", d.FrontmatterString("title"))
	}
	if got := d.FrontmatterStrings("tags"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("tags = %v", got)
	}
	if d.Body != "body line\n" {
		t.Errorf("body = %q", d.Body)
	}
}

func TestParseUnterminatedFrontmatterIsBody(t *testing.T) {
	in := "---\nfoo: bar\nstill body, no closer\n"
	d, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if d.Frontmatter != nil {
		t.Errorf("unterminated frontmatter should not parse; got %v", d.Frontmatter)
	}
	if d.Body != in {
		t.Errorf("body should be the whole input; got %q", d.Body)
	}
}

func TestRenderDeterministic(t *testing.T) {
	// Render the same data multiple times; output must be byte-identical.
	d := &Document{
		Frontmatter: map[string]any{
			"zebra": "z",
			"apple": "a",
			"mango": "m",
			"nested": map[string]any{
				"y": 2,
				"x": 1,
			},
		},
		Body: "body\n",
	}
	first, err := d.Render()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		next, err := d.Render()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, next) {
			t.Fatalf("Render is non-deterministic across runs:\n  first: %q\n  next:  %q", first, next)
		}
	}
}

func TestRenderAlphaSortedKeys(t *testing.T) {
	d := &Document{
		Frontmatter: map[string]any{
			"zebra": "z",
			"apple": "a",
			"mango": "m",
		},
		Body: "body\n",
	}
	got, err := d.Render()
	if err != nil {
		t.Fatal(err)
	}
	// Find the order in which keys appear.
	s := string(got)
	a := strings.Index(s, "apple:")
	m := strings.Index(s, "mango:")
	z := strings.Index(s, "zebra:")
	if !(a < m && m < z) {
		t.Errorf("keys not alphabetically ordered in output: %q", s)
	}
}

func TestRenderSortsNestedMaps(t *testing.T) {
	d := &Document{
		Frontmatter: map[string]any{
			"outer": map[string]any{
				"zed": 26,
				"abe": 1,
			},
		},
	}
	got, err := d.Render()
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "abe:") || !strings.Contains(s, "zed:") {
		t.Fatalf("missing nested keys: %q", s)
	}
	if strings.Index(s, "abe:") > strings.Index(s, "zed:") {
		t.Errorf("nested map not alphabetically ordered: %q", s)
	}
}

func TestRenderEmptyBody(t *testing.T) {
	d := &Document{
		Frontmatter: map[string]any{"title": "x"},
		Body:        "",
	}
	got, err := d.Render()
	if err != nil {
		t.Fatal(err)
	}
	// Expect just the frontmatter block, no trailing body section,
	// no extra blank line.
	want := "---\ntitle: x\n---\n"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderNoFrontmatter(t *testing.T) {
	d := &Document{Body: "hello\n"}
	got, err := d.Render()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello\n" {
		t.Errorf("got %q", got)
	}
}

func TestRoundTrip(t *testing.T) {
	in := "---\napple: 1\nzebra: z\n---\nbody\n"
	d, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(in), out) {
		t.Errorf("round-trip changed bytes:\n  in:  %q\n  out: %q", in, out)
	}
}

func TestFrontmatterStringMissingOrWrongType(t *testing.T) {
	d := &Document{Frontmatter: map[string]any{"n": 42}}
	if got := d.FrontmatterString("missing"); got != "" {
		t.Errorf("missing key should return empty; got %q", got)
	}
	if got := d.FrontmatterString("n"); got != "" {
		t.Errorf("wrong-type value should return empty; got %q", got)
	}
}

func TestFrontmatterStringsAcceptsBothEncodings(t *testing.T) {
	// yaml.v3 decodes ["a","b"] into []any{"a","b"} and the explicit
	// []string is the form returned by Document mutators. We accept both.
	d1 := &Document{Frontmatter: map[string]any{"k": []any{"a", "b"}}}
	if got := d1.FrontmatterStrings("k"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("[]any path: %v", got)
	}
	d2 := &Document{Frontmatter: map[string]any{"k": []string{"a", "b"}}}
	if got := d2.FrontmatterStrings("k"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("[]string path: %v", got)
	}
}
