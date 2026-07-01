package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/pgstore"
	pbserver "github.com/neverprepared/phantom-brain/internal/server"
)

// profileCmd groups the all-in-one binding lifecycle. Where `binding
// create`, `db provision`, and `bucket create` each do one thing,
// `profile create` orchestrates all of them from just a profile + vault
// name — deriving the bucket and index prefix, and trying every step
// idempotently so a re-run heals a half-provisioned binding instead of
// erroring. The individual commands still exist for surgical use.
func profileCmd() *cobra.Command {
	c := &cobra.Command{Use: "profile", Short: "All-in-one binding lifecycle (provision everything from a name)"}
	c.AddCommand(profileCreateCmd())
	return c
}

// stepStatus is the per-step outcome collected across the run so the
// command can print one honest summary at the end and exit non-zero if
// anything failed — the operator sees the whole picture in a single run.
type stepStatus struct {
	name   string
	result string // "created" | "exists" | "skipped" | "failed"
	detail string
	err    error
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
			if err := validateBindingSegment("profile", profile); err != nil {
				return err
			}
			if err := validateBindingSegment("vault", vault); err != nil {
				return err
			}
			key := pbserver.VaultKey{Profile: profile, Vault: vault}

			// Derive the storage names the operator didn't pin.
			if bucket == "" {
				bucket = profile + "-archives"
			}
			if indexPrefix == "" {
				indexPrefix = profile + "_"
			}
			if err := pbserver.ValidateStorageOverridePrefix(indexPrefix); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "provisioning binding %s\n", key)
			fmt.Fprintf(out, "  bucket       : %s\n", bucket)
			fmt.Fprintf(out, "  index_prefix : %s\n\n", indexPrefix)

			ctx, cancel := signalCancel(cmd.Context())
			defer cancel()

			var steps []stepStatus

			// --- 1. Postgres SoR ------------------------------------
			steps = append(steps, provisionPostgresStep(ctx, cmd, dsn, profile))

			// --- 2. MinIO bucket ------------------------------------
			steps = append(steps, provisionBucketStep(ctx, cmd, bucket))

			// --- 3. Binding config ----------------------------------
			steps = append(steps, writeBindingConfigStep(cmd, key, indexPrefix, bucket, token))

			// --- 4. Validate ----------------------------------------
			steps = append(steps, validateConfigStep(cmd, key))

			// --- 5. Reload (best-effort) ----------------------------
			if noReload {
				steps = append(steps, stepStatus{name: "daemon reload", result: "skipped", detail: "--no-reload set"})
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

// provisionPostgresStep creates pb_<profile> and migrates it. Idempotent
// via pgstore.Provision. On connect failure it adds the host-vs-container
// DSN hint — the #1 gotcha when running the CLI on the host while the
// daemon's server.toml DSN points at the in-container Postgres address.
func provisionPostgresStep(ctx context.Context, cmd *cobra.Command, dsnFlag, profile string) stepStatus {
	s := stepStatus{name: "postgres db (pb_" + profile + ")"}
	base, err := resolveDBDSN(cmd, dsnFlag)
	if err != nil {
		s.result, s.err = "failed", err
		return s
	}
	if err := pgstore.Provision(ctx, base, profile); err != nil {
		s.result = "failed"
		s.err = annotatePGConnectErr(err)
		return s
	}
	// Provision is idempotent and doesn't distinguish create-vs-existing;
	// report "ok" rather than guess.
	s.result, s.detail = "ok", "created or already present + migrated"
	return s
}

// annotatePGConnectErr appends the host/container DSN hint when the
// failure looks like a Postgres connection problem.
func annotatePGConnectErr(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "connect") || strings.Contains(msg, "dial") ||
		strings.Contains(msg, "connection refused") || strings.Contains(msg, "no route") {
		return fmt.Errorf("%w\n"+
			"      hint: running the CLI on the host? server.toml's DSN is the daemon's in-container\n"+
			"      address (e.g. 172.20.0.2:5432 / postgres:5432), unreachable from the host.\n"+
			"      Override with the host-mapped address, e.g.\n"+
			"        PB_POSTGRES_DSN=postgres://phantom_brain:PASS@localhost:5433/phantom_brain?sslmode=disable", err)
	}
	return err
}

// provisionBucketStep creates the MinIO bucket. Idempotent via
// CreateBucket (swallows BucketAlreadyOwnedByYou). Skips cleanly when the
// daemon isn't configured for MinIO (backend = "local").
func provisionBucketStep(ctx context.Context, cmd *cobra.Command, bucket string) stepStatus {
	s := stepStatus{name: "minio bucket (" + bucket + ")"}
	mb, err := openMinIOForOps(cmd)
	if err != nil {
		// backend != minio is a legitimate skip, not a failure — the
		// binding just uses local blob storage.
		if strings.Contains(err.Error(), "backend != ") {
			s.result, s.detail = "skipped", "storage backend is not minio"
			return s
		}
		s.result, s.err = "failed", err
		return s
	}
	if err := mb.CreateBucket(ctx, bucket); err != nil {
		s.result, s.err = "failed", err
		return s
	}
	s.result = "ok"
	return s
}

// writeBindingConfigStep writes auth.toml + config.toml. Unlike `binding
// create` (which refuses a pre-existing dir to protect the token), this
// heals in place: an existing binding keeps its token and only gets its
// config.toml overrides refreshed. A fresh binding gets a new token.
func writeBindingConfigStep(cmd *cobra.Command, key pbserver.VaultKey, indexPrefix, bucket, token string) stepStatus {
	s := stepStatus{name: "binding config"}
	configDir := resolveConfigDir(cmd)
	bindingDir := filepath.Join(configDir, "profiles", key.Profile, "vaults", key.Vault)
	authPath := filepath.Join(bindingDir, "auth.toml")
	cfgPath := filepath.Join(bindingDir, "config.toml")

	existed := false
	if _, err := os.Stat(authPath); err == nil {
		existed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		s.result, s.err = "failed", fmt.Errorf("stat %s: %w", authPath, err)
		return s
	}

	if err := os.MkdirAll(bindingDir, 0o700); err != nil {
		s.result, s.err = "failed", bindingWriteErr("mkdir", bindingDir, key.Profile, key.Vault, err)
		return s
	}

	// Only write auth.toml when absent — never clobber a live token.
	if !existed {
		if strings.TrimSpace(token) == "" {
			generated, err := newBearerToken()
			if err != nil {
				s.result, s.err = "failed", err
				return s
			}
			token = generated
		} else if strings.TrimSpace(token) != token {
			s.result, s.err = "failed", errors.New("--token must contain no whitespace")
			return s
		}
		authBody := fmt.Sprintf("bearer_token = %q\n", token)
		if err := os.WriteFile(authPath, []byte(authBody), 0o600); err != nil {
			s.result, s.err = "failed", bindingWriteErr("write", authPath, key.Profile, key.Vault, err)
			return s
		}
	}

	// config.toml (storage overrides) is safe to (re)write idempotently —
	// it holds no secret, and the derived overrides are deterministic.
	body := buildBindingConfigTOML(indexPrefix, bucket)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		s.result, s.err = "failed", bindingWriteErr("write", cfgPath, key.Profile, key.Vault, err)
		return s
	}

	if existed {
		s.result, s.detail = "exists", "kept token, refreshed config.toml"
	} else {
		s.result, s.detail = "created", "auth.toml (token) + config.toml: "+authPath
	}
	return s
}

// validateConfigStep dry-runs the registry load exactly as the daemon
// would at startup, catching TOML typos and duplicate bearer tokens
// before the SIGHUP. It confirms the just-written binding is visible.
func validateConfigStep(cmd *cobra.Command, key pbserver.VaultKey) stepStatus {
	s := stepStatus{name: "config validate"}
	configDir := resolveConfigDir(cmd)
	if _, err := pbserver.LoadServerConfig(configDir); err != nil {
		s.result, s.err = "failed", err
		return s
	}
	r, err := loadRegistryForOps(configDir)
	if err != nil {
		s.result, s.err = "failed", err
		return s
	}
	if _, ok := r.LookupByVault(key); !ok {
		s.result, s.err = "failed", fmt.Errorf("binding %s not visible in the loaded registry after write", key)
		return s
	}
	s.result, s.detail = "ok", "registry loads; binding visible"
	return s
}

// reloadDaemonStep sends SIGHUP so the running daemon re-reads the
// registry. Best-effort: a daemon that isn't running (no PID sidecar) is
// a "skipped", not a failure — the binding is on disk and loads on next
// start.
func reloadDaemonStep(cmd *cobra.Command) stepStatus {
	s := stepStatus{name: "daemon reload"}
	pid, err := readDaemonPID(resolveDataDir(cmd))
	if err != nil {
		s.result, s.detail = "skipped", "daemon not running (no pid sidecar); binding loads on next start"
		return s
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		s.result, s.err = "failed", fmt.Errorf("find pid %d: %w", pid, err)
		return s
	}
	if err := proc.Signal(syscall.SIGHUP); err != nil {
		s.result, s.err = "failed", fmt.Errorf("signal pid %d: %w", pid, err)
		return s
	}
	s.result, s.detail = "ok", fmt.Sprintf("SIGHUP sent to pid %d", pid)
	return s
}

// reportSteps prints the per-step summary and returns a non-nil error if
// any step failed, so the command exits non-zero on partial success.
func reportSteps(out writer, steps []stepStatus) error {
	fmt.Fprintln(out, "\nsummary:")
	var failed int
	for _, s := range steps {
		mark := map[string]string{"created": "+", "ok": "+", "exists": "=", "skipped": "-", "failed": "x"}[s.result]
		if mark == "" {
			mark = "?"
		}
		line := fmt.Sprintf("  [%s] %-28s %s", mark, s.name, s.result)
		if s.detail != "" {
			line += " — " + s.detail
		}
		fmt.Fprintln(out, line)
		if s.err != nil {
			fmt.Fprintf(out, "        %v\n", s.err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d step(s) failed — fix the causes above and re-run (the command is idempotent)", failed)
	}
	fmt.Fprintln(out, "\nbinding live.")
	return nil
}
