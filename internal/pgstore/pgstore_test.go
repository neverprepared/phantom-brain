package pgstore

import "testing"

// Validation rule under test: the profile is lowercased + trimmed, then must
// match ^[a-z0-9_]+$ and be non-empty. Lowercasing normalizes ("GSA"→"gsa"),
// but no other character class is accepted — hyphens, dots, spaces, and
// semicolons are all rejected.
func TestProfileDBName(t *testing.T) {
	valid := []struct{ in, want string }{
		{"personal", "pb_personal"},
		{"gsa", "pb_gsa"},
		{"GSA", "pb_gsa"},      // uppercase normalized down
		{"  gsa  ", "pb_gsa"},  // trimmed
		{"client_x", "pb_client_x"},
		{"a1", "pb_a1"},
		{"123", "pb_123"},
	}
	for _, tc := range valid {
		got, err := ProfileDBName(tc.in)
		if err != nil {
			t.Errorf("ProfileDBName(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ProfileDBName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	invalid := []string{
		"",            // empty
		"   ",         // whitespace only
		"a;b",         // semicolon (injection vector)
		"a-b",         // hyphen
		"a b",         // space
		"a.b",         // dot
		"a/b",         // slash
		`a"b`,         // quote
		"drop table",  // space again, sanity
		"naïve",       // non-ASCII
	}
	for _, in := range invalid {
		if _, err := ProfileDBName(in); err == nil {
			t.Errorf("ProfileDBName(%q): expected error, got nil", in)
		}
	}
}

func TestDSNForProfile(t *testing.T) {
	cases := []struct {
		name    string
		baseDSN string
		profile string
		want    string
	}{
		{
			name:    "basic path swap",
			baseDSN: "postgres://u:p@h:5433/phantom_brain",
			profile: "gsa",
			want:    "postgres://u:p@h:5433/pb_gsa",
		},
		{
			name:    "preserves query params",
			baseDSN: "postgres://u:p@h:5433/phantom_brain?sslmode=disable&pool_max_conns=4",
			profile: "personal",
			want:    "postgres://u:p@h:5433/pb_personal?sslmode=disable&pool_max_conns=4",
		},
		{
			name:    "postgresql scheme preserved",
			baseDSN: "postgresql://u:p@h:5432/postgres",
			profile: "client_x",
			want:    "postgresql://u:p@h:5432/pb_client_x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DSNForProfile(tc.baseDSN, tc.profile)
			if err != nil {
				t.Fatalf("DSNForProfile: unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("DSNForProfile(%q, %q) = %q, want %q", tc.baseDSN, tc.profile, got, tc.want)
			}
		})
	}

	// Invalid profile propagates the validation error.
	if _, err := DSNForProfile("postgres://h/db", "a-b"); err == nil {
		t.Error("DSNForProfile with invalid profile: expected error, got nil")
	}
}

func TestToPgxURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"postgres://u:p@h/db", "pgx5://u:p@h/db"},
		{"postgresql://u:p@h/db", "pgx5://u:p@h/db"},
		{"pgx5://u:p@h/db", "pgx5://u:p@h/db"},
	}
	for _, tc := range cases {
		if got := toPgxURL(tc.in); got != tc.want {
			t.Errorf("toPgxURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
