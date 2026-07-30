package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/provision"
	pbserver "github.com/neverprepared/phantom-brain/internal/server"
)

// profileCmd groups the all-in-one binding lifecycle. Where `binding
// create`, `db provision`, and `bucket create` each do one thing,
// `profile create` orchestrates all of them from just a profile + vault
// name. The actual provisioning lives in internal/provision so the daemon's
// admin HTTP handler can drive the same steps; this command is a thin CLI
// wrapper that resolves deps from flags/env and reloads via SIGHUP.
func profileCmd() *cobra.Command {
	c := &cobra.Command{Use: "profile", Short: "All-in-one binding lifecycle (provision everything from a name)"}
	c.AddCommand(profileCreateCmd())
	return c
}

func profileCreateCmd() *cobra.Command {
	var (
		dsn         string
		bucket      string
		indexPrefix string
		token       string
		noReload    bool
	)
	c := &cobra.Command{
		Use:   "create <profile> <vault>",
		Short: "Provision a complete binding: Postgres db + MinIO bucket + config, then reload",
		Long: `Create everything a new binding needs from just a profile and vault name:

  1. Postgres System-of-Record  (pb_<profile>)         — db provision
  2. MinIO bucket               (<profile>-archives)    — bucket create
  3. Binding config             (auth.toml + config.toml with [storage_overrides])
  4. Validate the config
  5. SIGHUP the running daemon so the binding goes live

Every step is idempotent: re-running on a partially-provisioned profile
heals it instead of erroring. Derived names (bucket, index_prefix) are
overridable via flags. The base Postgres DSN resolves from --dsn, then
$PB_POSTGRES_DSN, then $DATABASE_URL, then server.toml [postgres] dsn.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, vault := args[0], args[1]

			// Resolve deps from CLI concerns, then hand off to the core.
			base, err := resolveDBDSN(cmd, dsn)
			if err != nil {
				return err
			}
			var mb *pbserver.MinIOBackend
			if m, err := openMinIOForOps(cmd); err != nil {
				// backend != minio is a legitimate local-storage config —
				// the bucket step self-skips on a nil backend.
				if !strings.Contains(err.Error(), "backend != ") {
					return err
				}
			} else {
				mb = m
			}
			configDir := resolveConfigDir(cmd)

			ctx, cancel := signalCancel(cmd.Context())
			defer cancel()

			res, err := provision.ProfileCreate(ctx, provision.Deps{
				BaseDSN:   base,
				MinIO:     mb,
				ConfigDir: configDir,
			}, provision.Spec{
				Profile:     profile,
				Vault:       vault,
				Bucket:      bucket,
				IndexPrefix: indexPrefix,
				Token:       token,
			})
			if err != nil {
				return err // precondition failure (invalid name/prefix/token)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "provisioning binding %s\n", res.Key)
			fmt.Fprintf(out, "  bucket       : %s\n", res.Bucket)
			fmt.Fprintf(out, "  index_prefix : %s\n\n", res.IndexPrefix)

			steps := res.Steps
			// Annotate a Postgres connect failure with the host/container hint.
			for i := range steps {
				if steps[i].Err != nil && strings.HasPrefix(steps[i].Name, "postgres db") {
					steps[i].Err = annotatePGConnectErr(steps[i].Err)
				}
			}
			// Reload (best-effort) so a running daemon picks up the binding.
			if noReload {
				steps = append(steps, provision.StepResult{Name: "daemon reload", Result: "skipped", Detail: "--no-reload set"})
			} else {
				steps = append(steps, reloadDaemonStep(cmd))
			}
			return reportSteps(out, steps)
		},
	}
	opsCommonFlags(c)
	c.Flags().StringVar(&dsn, "dsn", "", "base/maintenance Postgres DSN (default: $PB_POSTGRES_DSN, then $DATABASE_URL, then server.toml [postgres] dsn)")
	c.Flags().StringVar(&bucket, "bucket", "", "MinIO bucket (default: <profile>-archives)")
	c.Flags().StringVar(&indexPrefix, "index-prefix", "", "OS index prefix override (default: <profile>_)")
	c.Flags().StringVar(&token, "token", "", "bearer token (default: generated; ignored if the binding already has one)")
	c.Flags().BoolVar(&noReload, "no-reload", false, "do not SIGHUP the daemon; just print the reload hint")
	return c
}

// annotatePGConnectErr appends the host/container DSN hint when the
// failure looks like a Postgres connection problem — the #1 gotcha when
// running the CLI on the host while server.toml's DSN points at the
// in-container Postgres address.
func annotatePGConnectErr(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "connect") || strings.Contains(msg, "dial") ||
		strings.Contains(msg, "connection refused") || strings.Contains(msg, "no route") {
		return fmt.Errorf("%w\n"+
			"      hint: running the CLI on the host? server.toml's DSN is the daemon's in-container\n"+
			"      address (e.g. 172.20.0.2:5432 / postgres:5432), unreachable from the host.\n"+
			"      Override with the host-mapped address, e.g.\n"+
			"        PB_POSTGRES_DSN=postgres://phantom_brain:PASS@localhost:5433/phantom_brain", err)
	}
	return err
}

// reloadDaemonStep sends SIGHUP so the running daemon re-reads the
// registry. Best-effort: a daemon that isn't running (no PID sidecar) is
// a "skipped", not a failure — the binding is on disk and loads on next
// start.
func reloadDaemonStep(cmd *cobra.Command) provision.StepResult {
	s := provision.StepResult{Name: "daemon reload"}
	pid, err := readDaemonPID(resolveDataDir(cmd))
	if err != nil {
		s.Result, s.Detail = "skipped", "daemon not running (no pid sidecar); binding loads on next start"
		return s
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		s.Result, s.Err = "failed", fmt.Errorf("find pid %d: %w", pid, err)
		return s
	}
	if err := proc.Signal(syscall.SIGHUP); err != nil {
		s.Result, s.Err = "failed", fmt.Errorf("signal pid %d: %w", pid, err)
		return s
	}
	s.Result, s.Detail = "ok", fmt.Sprintf("SIGHUP sent to pid %d", pid)
	return s
}

// reportSteps prints the per-step summary and returns a non-nil error if
// any step failed, so the command exits non-zero on partial success.
func reportSteps(out writer, steps []provision.StepResult) error {
	fmt.Fprintln(out, "\nsummary:")
	var failed int
	for _, s := range steps {
		mark := map[string]string{"created": "+", "ok": "+", "exists": "=", "skipped": "-", "failed": "x"}[s.Result]
		if mark == "" {
			mark = "?"
		}
		line := fmt.Sprintf("  [%s] %-28s %s", mark, s.Name, s.Result)
		if s.Detail != "" {
			line += " — " + s.Detail
		}
		fmt.Fprintln(out, line)
		if s.Err != nil {
			fmt.Fprintf(out, "        %v\n", s.Err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d step(s) failed — fix the causes above and re-run (the command is idempotent)", failed)
	}
	fmt.Fprintln(out, "\nbinding live.")
	return nil
}
