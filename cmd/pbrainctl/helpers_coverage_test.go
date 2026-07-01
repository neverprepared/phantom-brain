package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/brain/wqueue"
	pbserver "github.com/neverprepared/phantom-brain/internal/server"
)

func TestHumanizeAge_Boundaries(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"negative clamps to zero", -5 * time.Second, "0s"},
		{"sub-second", 250 * time.Millisecond, "0s"},
		{"seconds", 45 * time.Second, "45s"},
		{"just under a minute", 59 * time.Second, "59s"},
		{"exactly a minute", time.Minute, "1m"},
		{"minutes", 90 * time.Second, "1m"},
		{"just under an hour", 59 * time.Minute, "59m"},
		{"exactly an hour", time.Hour, "1h"},
		{"hours", 5 * time.Hour, "5h"},
		{"just under a day", 23 * time.Hour, "23h"},
		{"exactly a day", 24 * time.Hour, "1d"},
		{"days", 50 * time.Hour, "2d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanizeAge(tc.in); got != tc.want {
				t.Fatalf("humanizeAge(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no tilde absolute", "/etc/passwd", "/etc/passwd"},
		{"no tilde relative", "foo/bar", "foo/bar"},
		{"empty", "", ""},
		{"bare tilde", "~", home},
		{"tilde slash", "~/x/y", filepath.Join(home, "x/y")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandHome(tc.in); got != tc.want {
				t.Fatalf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestResolveLegacyIndexDir(t *testing.T) {
	t.Run("BRAIN_INDEX_PATH override wins and expands tilde", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("BRAIN_INDEX_PATH", "~/custom/index")
		got, err := resolveLegacyIndexDir("/some/vault")
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Join(home, "custom/index") {
			t.Fatalf("override not honored: %q", got)
		}
	})

	t.Run("default layout uses XDG_CONFIG_HOME and basename of vault", func(t *testing.T) {
		t.Setenv("BRAIN_INDEX_PATH", "")
		t.Setenv("HOME", t.TempDir())
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		t.Setenv("WORKSPACE_PROFILE", "")
		got, err := resolveLegacyIndexDir("/data/vaults/memory")
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(xdg, "phantom-brain", "profiles", "default", "memory", "_index")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("honors WORKSPACE_PROFILE", func(t *testing.T) {
		t.Setenv("BRAIN_INDEX_PATH", "")
		t.Setenv("HOME", t.TempDir())
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		t.Setenv("WORKSPACE_PROFILE", "work")
		got, err := resolveLegacyIndexDir("/v/main")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, filepath.Join("profiles", "work", "main")) {
			t.Fatalf("profile not honored: %q", got)
		}
	})
}

func TestResolveDBDSN(t *testing.T) {
	// dbDSNCmd builds a cobra command with a config-dir flag pointed at an
	// empty temp dir, so the server.toml fallback finds nothing unless a
	// subtest writes one. Keeps the env/flag subtests hermetic — no stray
	// server.toml on the box can leak in.
	dbDSNCmd := func(t *testing.T, configDir string) *cobra.Command {
		c := &cobra.Command{}
		c.Flags().String("config-dir", configDir, "")
		return c
	}

	t.Run("explicit flag wins", func(t *testing.T) {
		t.Setenv("PB_POSTGRES_DSN", "postgres://env")
		t.Setenv("DATABASE_URL", "postgres://url")
		got, err := resolveDBDSN(dbDSNCmd(t, t.TempDir()), "postgres://flag")
		if err != nil || got != "postgres://flag" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("PB_POSTGRES_DSN beats DATABASE_URL", func(t *testing.T) {
		t.Setenv("PB_POSTGRES_DSN", "postgres://env")
		t.Setenv("DATABASE_URL", "postgres://url")
		got, err := resolveDBDSN(dbDSNCmd(t, t.TempDir()), "")
		if err != nil || got != "postgres://env" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("DATABASE_URL fallback", func(t *testing.T) {
		t.Setenv("PB_POSTGRES_DSN", "")
		t.Setenv("DATABASE_URL", "postgres://url")
		got, err := resolveDBDSN(dbDSNCmd(t, t.TempDir()), "")
		if err != nil || got != "postgres://url" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("server.toml [postgres] dsn fallback", func(t *testing.T) {
		t.Setenv("PB_POSTGRES_DSN", "")
		t.Setenv("DATABASE_URL", "")
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "server.toml"),
			[]byte("[postgres]\ndsn = \"postgres://toml/base\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := resolveDBDSN(dbDSNCmd(t, dir), "")
		if err != nil || got != "postgres://toml/base" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("nothing set is an actionable error", func(t *testing.T) {
		t.Setenv("PB_POSTGRES_DSN", "")
		t.Setenv("DATABASE_URL", "")
		_, err := resolveDBDSN(dbDSNCmd(t, t.TempDir()), "")
		if err == nil {
			t.Fatal("expected error when no DSN resolves")
		}
		if !strings.Contains(err.Error(), "--dsn") {
			t.Fatalf("error should mention --dsn, got %v", err)
		}
	})
}

func TestResolveDataHomeFromEnv(t *testing.T) {
	t.Run("XDG_DATA_HOME wins", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "/xdg/data")
		t.Setenv("HOME", "/home/ignored")
		got, err := resolveDataHomeFromEnv()
		if err != nil || got != "/xdg/data" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("falls back to HOME/.local/share", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "/home/me")
		got, err := resolveDataHomeFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Join("/home/me", ".local", "share") {
			t.Fatalf("unexpected: %q", got)
		}
	})
	t.Run("neither set errors", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "")
		if _, err := resolveDataHomeFromEnv(); err == nil {
			t.Fatal("expected error with no data dir")
		}
	})
}

func TestResolveXDGDataHome(t *testing.T) {
	t.Run("XDG set", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "/x")
		got, err := resolveXDGDataHome()
		if err != nil || got != "/x" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("fallback to HOME/.local/share", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "/home/abc")
		got, err := resolveXDGDataHome()
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Join("/home/abc", ".local", "share") {
			t.Fatalf("unexpected: %q", got)
		}
	})
}

func TestErrRetentionDisabled(t *testing.T) {
	err := errRetentionDisabled()
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !strings.Contains(err.Error(), "--dry-run") || !strings.Contains(err.Error(), "--older-than") {
		t.Fatalf("message should guide the operator, got: %v", err)
	}
}

func TestVaultArgFromArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
		profile string
		vault   string
	}{
		{"valid", []string{"personal/memory"}, false, "personal", "memory"},
		{"no args", nil, true, "", ""},
		{"too many", []string{"a", "b"}, true, "", ""},
		{"missing slash", []string{"justprofile"}, true, "", ""},
		{"empty profile", []string{"/vault"}, true, "", ""},
		{"empty vault", []string{"profile/"}, true, "", ""},
		{"extra slash kept in vault", []string{"p/v/extra"}, false, "p", "v/extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, err := vaultArgFromArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %v", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if key.Profile != tc.profile || key.Vault != tc.vault {
				t.Fatalf("got %+v, want profile=%q vault=%q", key, tc.profile, tc.vault)
			}
		})
	}
}

func TestCountQueueDir(t *testing.T) {
	data := t.TempDir()
	d := pbserver.DataDir(data)
	key := pbserver.VaultKey{Profile: "p", Vault: "v"}

	// Missing dir => 0.
	if n := countQueueDir(d, key, "claimed"); n != 0 {
		t.Fatalf("missing dir should count 0, got %d", n)
	}

	sub := filepath.Join(d.VaultDir(key.Profile, key.Vault), "queue", "claimed")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two .json files, one non-json, one nested dir (ignored).
	for _, name := range []string{"a.json", "b.json", "note.txt"} {
		if err := os.WriteFile(filepath.Join(sub, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(sub, "nested.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := countQueueDir(d, key, "claimed"); n != 2 {
		t.Fatalf("expected 2 json files, got %d", n)
	}
}

func TestReadDaemonPID(t *testing.T) {
	data := t.TempDir()
	d := pbserver.DataDir(data)

	t.Run("missing sidecar errors", func(t *testing.T) {
		if _, err := readDaemonPID(d); err == nil {
			t.Fatal("expected error for missing pid file")
		}
	})

	pidPath := d.GlobalFlockPath()
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("valid pid with surrounding whitespace", func(t *testing.T) {
		if err := os.WriteFile(pidPath, []byte("  4242\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		pid, err := readDaemonPID(d)
		if err != nil {
			t.Fatal(err)
		}
		if pid != 4242 {
			t.Fatalf("got pid %d, want 4242", pid)
		}
	})

	t.Run("unparseable pid errors", func(t *testing.T) {
		if err := os.WriteFile(pidPath, []byte("not-a-number"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readDaemonPID(d); err == nil {
			t.Fatal("expected parse error")
		}
	})
}

func TestResolveDataAndConfigDirFlags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cmd := &cobra.Command{Use: "x"}
	opsCommonFlags(cmd)

	// Defaults (no flags set) should resolve to the daemon defaults, not error.
	if got := resolveDataDir(cmd); string(got) == "" {
		t.Fatal("default data dir should be non-empty")
	}
	if got := resolveConfigDir(cmd); got == "" {
		t.Fatal("default config dir should be non-empty")
	}

	// Explicit overrides with tilde expansion.
	if err := cmd.Flags().Set("data-dir", "~/dd"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("config-dir", "~/cc"); err != nil {
		t.Fatal(err)
	}
	if got := resolveDataDir(cmd); string(got) != filepath.Join(home, "dd") {
		t.Fatalf("data-dir override not expanded: %q", got)
	}
	if got := resolveConfigDir(cmd); got != filepath.Join(home, "cc") {
		t.Fatalf("config-dir override not expanded: %q", got)
	}
}

func TestNewStderrLogger(t *testing.T) {
	if newStderrLogger() == nil {
		t.Fatal("expected a non-nil logger")
	}
}

func TestResolveQueueDir(t *testing.T) {
	t.Run("queue-dir escape hatch expands tilde", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		got, err := resolveQueueDir(queueResolveOpts{queueDir: "~/q"})
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Join(home, "q") {
			t.Fatalf("unexpected: %q", got)
		}
	})

	t.Run("profile without vault errors", func(t *testing.T) {
		_, err := resolveQueueDir(queueResolveOpts{profile: "p"})
		if err == nil || !strings.Contains(err.Error(), "must be set together") {
			t.Fatalf("expected paired-flag error, got %v", err)
		}
	})

	t.Run("profile+vault resolve under XDG data home", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdg)
		got, err := resolveQueueDir(queueResolveOpts{profile: "pers", vault: "mem"})
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(xdg, "phantom-brain", "pers", "mem")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("path traversal in vault rejected", func(t *testing.T) {
		_, err := resolveQueueDir(queueResolveOpts{profile: "p", vault: "../x"})
		if err == nil || !strings.Contains(err.Error(), "--vault") {
			t.Fatalf("expected vault traversal rejection, got %v", err)
		}
	})
}

func TestDispatch_DecodeErrorsAndUnknownKind(t *testing.T) {
	client, err := brain.NewClient(brain.ClientOpts{BaseURL: "http://127.0.0.1:0", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Bad JSON for each decode-first kind fails before any network call.
	kinds := []wqueue.Kind{
		wqueue.KindPerceive,
		wqueue.KindLearn,
		wqueue.KindTaskPromote,
		wqueue.KindAttach,
		wqueue.KindTrace,
	}
	for _, k := range kinds {
		it := &wqueue.Item{Kind: k, PayloadJSON: []byte("{not json")}
		if err := dispatch(ctx, client, it); err == nil {
			t.Fatalf("kind %q: expected decode error", k)
		}
	}

	// Unknown kind is rejected explicitly.
	it := &wqueue.Item{Kind: wqueue.Kind("bogus"), PayloadJSON: []byte("{}")}
	err = dispatch(ctx, client, it)
	if err == nil || !strings.Contains(err.Error(), "unknown queue kind") {
		t.Fatalf("expected unknown-kind error, got %v", err)
	}

	// Attach with a staged path that doesn't exist surfaces a read error
	// (valid JSON payload so it gets past the decode step).
	it = &wqueue.Item{
		Kind:        wqueue.KindAttach,
		PayloadJSON: []byte(`{"sha":"s","title":"t"}`),
		StagedPath:  filepath.Join(t.TempDir(), "missing.bin"),
	}
	if err := dispatch(ctx, client, it); err == nil || !strings.Contains(err.Error(), "read staged attach bytes") {
		t.Fatalf("expected staged read error, got %v", err)
	}
}

func TestWriteQueueTable_TruncatesAndFormats(t *testing.T) {
	now := time.Now()
	longSHA := "0123456789abcdef0123456789"
	longErr := strings.Repeat("x", 100)
	items := []*wqueue.Item{
		{
			ID:            1,
			Kind:          wqueue.KindLearn,
			SHA:           longSHA,
			Attempts:      3,
			EnqueuedAt:    now.Add(-90 * time.Second),
			LastAttemptAt: now.Add(-30 * time.Second),
			LastError:     longErr,
		},
		{
			ID:         2,
			Kind:       wqueue.KindPerceive,
			SHA:        "short",
			EnqueuedAt: now.Add(-time.Hour),
			// LastAttemptAt zero => "-"
		},
	}
	var buf bytes.Buffer
	if err := writeQueueTable(&buf, "/tmp/qdir", items, now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// SHA truncated to 12 chars.
	if !strings.Contains(out, longSHA[:12]) || strings.Contains(out, longSHA) {
		t.Fatalf("SHA not truncated to 12 chars:\n%s", out)
	}
	// Long error truncated with ellipsis.
	if !strings.Contains(out, "...") {
		t.Fatalf("long error should be truncated with ...:\n%s", out)
	}
	// Never-attempted row shows the dash sentinel (tabwriter pads columns
	// with spaces, so match the field on the perceive row).
	var perceiveLine string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "perceive") {
			perceiveLine = ln
		}
	}
	if perceiveLine == "" {
		t.Fatalf("missing perceive row:\n%s", out)
	}
	hasDashField := false
	for _, f := range strings.Fields(perceiveLine) {
		if f == "-" {
			hasDashField = true
		}
	}
	if !hasDashField {
		t.Fatalf("expected '-' for never-attempted last attempt:\n%s", out)
	}
	// "ago" markers for relative ages.
	if !strings.Contains(out, "ago") {
		t.Fatalf("expected relative age markers:\n%s", out)
	}
}

func TestAliveIsEligible_TooYoung(t *testing.T) {
	dir := t.TempDir()
	m := &brain.Manifest{PID: 999999}
	ok, reason := aliveIsEligible(m, dir, 1*time.Minute, 1*time.Hour, nil)
	if ok {
		t.Fatal("a brain younger than retention must not be eligible")
	}
	if !strings.Contains(reason, "too young") {
		t.Fatalf("reason should explain youth, got %q", reason)
	}
}
