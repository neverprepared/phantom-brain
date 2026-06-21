package canonicalize

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoldenFixtures walks testdata/ and verifies every <name>.input.md
// canonicalizes to its paired <name>.expected.md byte-for-byte.
//
// To add a regression test: drop a new input/expected pair into
// testdata/. To regenerate expected output after an intentional change
// to canonicalization rules, run with -update.
func TestGoldenFixtures(t *testing.T) {
	matches, err := filepath.Glob("testdata/*.input.md")
	if err != nil {
		t.Fatalf("glob testdata: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no testdata/*.input.md fixtures found")
	}

	for _, inputPath := range matches {
		name := strings.TrimSuffix(filepath.Base(inputPath), ".input.md")
		expectedPath := filepath.Join("testdata", name+".expected.md")

		t.Run(name, func(t *testing.T) {
			in, err := os.ReadFile(inputPath)
			if err != nil {
				t.Fatalf("read input: %v", err)
			}

			got, err := Canonicalize(in)
			if err != nil {
				t.Fatalf("Canonicalize: %v", err)
			}

			if updateGolden {
				if err := os.WriteFile(expectedPath, got, 0o644); err != nil {
					t.Fatalf("write expected: %v", err)
				}
				t.Logf("updated %s", expectedPath)
				return
			}

			want, err := os.ReadFile(expectedPath)
			if err != nil {
				t.Fatalf("read expected: %v (run with UPDATE_GOLDEN=1 to generate)", err)
			}

			if !bytes.Equal(got, want) {
				t.Errorf("canonical form mismatch for %s\n--- want ---\n%s\n--- got ---\n%s",
					name, dumpBytes(want), dumpBytes(got))
			}
		})
	}
}

func TestIdempotent(t *testing.T) {
	matches, err := filepath.Glob("testdata/*.input.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, inputPath := range matches {
		name := strings.TrimSuffix(filepath.Base(inputPath), ".input.md")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(inputPath)
			if err != nil {
				t.Fatal(err)
			}
			once, err := Canonicalize(raw)
			if err != nil {
				t.Fatalf("Canonicalize once: %v", err)
			}
			twice, err := Canonicalize(once)
			if err != nil {
				t.Fatalf("Canonicalize twice: %v", err)
			}
			if !bytes.Equal(once, twice) {
				t.Errorf("Canonicalize is not idempotent on %s:\n--- after once ---\n%s\n--- after twice ---\n%s",
					name, dumpBytes(once), dumpBytes(twice))
			}
		})
	}
}

func TestSumStable(t *testing.T) {
	// Two inputs that differ only in normalized-away features must
	// produce the same SHA. This is the dedup guarantee in practice.
	pairs := []struct {
		name   string
		a, b   string
		wantEq bool
	}{
		{
			name: "crlf vs lf",
			a:    "hello\r\nworld\r\n",
			b:    "hello\nworld\n",
			wantEq: true,
		},
		{
			name: "trailing whitespace",
			a:    "hello   \nworld\t\n",
			b:    "hello\nworld\n",
			wantEq: true,
		},
		{
			name: "frontmatter key order",
			a:    "---\nb: 2\na: 1\n---\nbody\n",
			b:    "---\na: 1\nb: 2\n---\nbody\n",
			wantEq: true,
		},
		{
			name: "trailing blank lines",
			a:    "hello\n\n\n\n",
			b:    "hello\n",
			wantEq: true,
		},
		{
			name: "different body content",
			a:    "hello\n",
			b:    "world\n",
			wantEq: false,
		},
		{
			name: "different frontmatter values",
			a:    "---\nfoo: 1\n---\nbody\n",
			b:    "---\nfoo: 2\n---\nbody\n",
			wantEq: false,
		},
	}

	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			sa, err := Sum([]byte(p.a))
			if err != nil {
				t.Fatalf("Sum(a): %v", err)
			}
			sb, err := Sum([]byte(p.b))
			if err != nil {
				t.Fatalf("Sum(b): %v", err)
			}
			if (sa == sb) != p.wantEq {
				t.Errorf("Sum equality = %v, want %v\n  a=%q -> %s\n  b=%q -> %s",
					sa == sb, p.wantEq, p.a, sa, p.b, sb)
			}
		})
	}
}

func TestUnterminatedFrontmatterIsBody(t *testing.T) {
	// An opening --- with no closing --- must not be misinterpreted as
	// frontmatter. The whole document is body.
	in := "---\nfoo: bar\nstill body\nno closer here\n"
	got, err := Canonicalize([]byte(in))
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if !bytes.HasPrefix(got, []byte("---\n")) {
		t.Fatalf("output should start with --- since input does, got %q", string(got[:min(20, len(got))]))
	}
	// Should NOT have a re-emitted frontmatter block.
	if bytes.Count(got, []byte("---\n")) != 1 {
		t.Errorf("unterminated frontmatter should not be re-emitted as a block; got %q", string(got))
	}
}

// --- helpers ---

var updateGolden = os.Getenv("UPDATE_GOLDEN") == "1"

func dumpBytes(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		switch {
		case c == '\n':
			sb.WriteString("\\n\n")
		case c == '\t':
			sb.WriteString("\\t")
		case c < 0x20:
			sb.WriteString("?")
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}
