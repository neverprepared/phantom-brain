// pbrainctl is the single binary for phantom-brain v5: MCP server, daemon, and
// operator CLI in one. The subcommand picks the mode.
//
// Phase 0 wires the cobra skeleton, a real `version` subcommand, and the
// `mcp` subcommand backed by internal/mcp. Tool surface grows across
// Days 10-12; `serve` and the operator subcommands remain stubs until
// Phase 2-3.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"github.com/neverprepared/mcp-phantom-brain/internal/brain"
	"github.com/neverprepared/mcp-phantom-brain/internal/config"
	"github.com/neverprepared/mcp-phantom-brain/internal/index"
	pbmcp "github.com/neverprepared/mcp-phantom-brain/internal/mcp"
	"github.com/neverprepared/mcp-phantom-brain/internal/ollama"
	pbserver "github.com/neverprepared/mcp-phantom-brain/internal/server"
	"github.com/neverprepared/mcp-phantom-brain/internal/vault"
	"github.com/neverprepared/mcp-phantom-brain/internal/version"
	"github.com/neverprepared/mcp-phantom-brain/internal/working"
)

func main() {
	root := &cobra.Command{
		Use:   "pbrainctl",
		Short: "phantom-brain — MCP server, daemon, and operator CLI",
		Long: `pbrainctl is a single binary serving three modes:

  pbrainctl mcp          stdio JSON-RPC MCP server (per agent process)
  pbrainctl serve        HTTP daemon (per-(profile, vault) reaper + synthesizer)
  pbrainctl <op>         operator commands (list, snapshot, vault, ...)

See https://github.com/neverprepared/mcp-phantom-brain for the v5 spec.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(versionCmd())
	root.AddCommand(mcpCmd())
	root.AddCommand(serveCmd())
	root.AddCommand(migrateLegacyCmd())
	root.AddCommand(ingestBulkCmd())

	// Operator subcommands (Phase 3). Grouped by domain so the help
	// text stays scannable.
	root.AddCommand(vaultCmd())
	root.AddCommand(snapshotCmd())
	root.AddCommand(queueCmd())
	root.AddCommand(maintenanceCmd())
	root.AddCommand(brainListCmd())
	root.AddCommand(brainShowCmd())
	root.AddCommand(brainOrphansCmd())
	// force-merge retired in Phase 6 — no reaper pass to fire.
	// force-checkpoint retired in Phase 6 — daemon's SynthWorker drains async.

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "pbrainctl: %v\n", err)
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build metadata",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(),
				"pbrainctl %s\n  commit: %s\n  built:  %s\n",
				version.Version, version.Commit, version.BuildDate,
			)
			return nil
		},
	}
}

// mcpCmd runs the stdio JSON-RPC MCP server. Two startup modes:
//
//   - v5.0 agent contract (CL_BRAIN_API set): full Phase 1 lifecycle —
//     LoadAgent, recovery sweep, Birth, heartbeat, register
//     brain_{status,checkpoint,death} alongside the Phase 0 tools.
//     Graceful Shutdown on SIGINT/SIGTERM packs the death payload.
//   - legacy BRAIN_VAULT_PATH: Phase 0 behavior, kept so existing
//     deployments keep working until the operator is ready to cut
//     over. No Lifecycle, no brain_* lifecycle tools.
//
// The choice is made by CL_BRAIN_API alone — operators don't need to
// set a flag; presence of the agent contract is the signal.
func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run as a stdio JSON-RPC MCP server",
		Long: `Starts an MCP server on stdio. Intended to be spawned by Claude Code
via .claude.json mcpServers entries.

Modes (chosen automatically):

  v5.0 agent contract (set CL_BRAIN_API to enable):
    CL_BRAIN_API           daemon URL
    CL_BRAIN_API_TOKEN     bearer token
    CL_WORKSPACE_PROFILE   profile name
    CL_BRAIN_VAULT         vault name
    CL_BRAIN_ID            optional — rebind to an existing brain dir

  legacy (Phase 0 compatibility):
    BRAIN_VAULT_PATH       absolute path to the vault directory
    WORKSPACE_PROFILE      profile name (default "default")

Ollama (both modes):
  OLLAMA_BASE_URL          embeddings endpoint (default http://localhost:11434)
  EMBEDDING_MODEL          model name (default nomic-embed-text)
  EMBEDDING_DIMS           embedding dimensionality (default 768).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(os.Getenv("CL_BRAIN_API")) != "" {
				return runMCPAgentMode()
			}
			return runMCPLegacyMode()
		},
	}
}

// runMCPLegacyMode is the Phase 0 startup path, unchanged. Kept so
// the live deployment can roll forward to this binary without setting
// CL_BRAIN_* until the operator is ready.
func runMCPLegacyMode() error {
	vaultDir := strings.TrimSpace(os.Getenv("BRAIN_VAULT_PATH"))
	if vaultDir == "" {
		return fmt.Errorf("BRAIN_VAULT_PATH is required (or set CL_BRAIN_API for agent-contract mode)")
	}
	vaultDir = expandHome(vaultDir)

	indexDir, err := resolveLegacyIndexDir(vaultDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return fmt.Errorf("mkdir index dir: %w", err)
	}
	if err := vault.EnsureSkeleton(vaultDir); err != nil {
		return fmt.Errorf("ensure vault skeleton: %w", err)
	}
	_, _ = working.ReapOrphanedShards(indexDir)

	oll := ollama.New(ollama.OptionsFromEnv())
	idx, err := index.Open(indexDir, oll.Dims())
	if err != nil {
		return fmt.Errorf("open index: %w", err)
	}
	defer idx.Close()

	wm, err := working.Open(indexDir)
	if err != nil {
		return fmt.Errorf("open working memory: %w", err)
	}
	defer wm.Close()

	srv := server.NewMCPServer("phantom-brain", version.Version, server.WithToolCapabilities(false))
	pbmcp.NewServer(pbmcp.ServerDeps{
		Index:    idx,
		Working:  wm,
		Embedder: oll,
		VaultDir: vaultDir,
	}).Register(srv)
	return server.ServeStdio(srv)
}

// runMCPAgentMode is the v5.0 startup path: LoadAgent → recovery
// sweep → Lifecycle.Start (births brain, starts heartbeat) → register
// MCP tools rooted at brain_dir/vault → ServeStdio. SIGINT/SIGTERM
// triggers a graceful Shutdown that packs the death payload before
// the process exits.
func runMCPAgentMode() error {
	agent, err := config.LoadAgent()
	if err != nil {
		return err
	}

	// MCP uses stdout for JSON-RPC; logger MUST go to stderr.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Recovery sweep before we touch anything else — corpses from
	// crashed siblings get marked dead, freeing their resources.
	if _, err := brain.Recover(brain.RecoverOpts{
		Agent:    agent,
		Platform: brain.NewPlatform(),
		Logger:   logger,
	}); err != nil {
		logger.Warn("phantom-brain: recovery sweep failed (continuing)", slog.String("err", err.Error()))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	lc, err := brain.Start(brain.StartOpts{
		Agent:        agent,
		Platform:     brain.NewPlatform(),
		Logger:       logger,
		HeartbeatCtx: ctx,
	})
	if err != nil {
		return fmt.Errorf("brain start: %w", err)
	}
	// Best-effort death payload on exit. Idempotent via
	// brain.IsAlreadyShutDown so a brain_death MCP call followed by
	// SIGTERM doesn't try to die twice.
	defer func() {
		shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
		defer shutdownCancel()
		if _, err := lc.Shutdown(shutdownCtx); err != nil && !brain.IsAlreadyShutDown(err) {
			logger.Warn("phantom-brain: shutdown error", slog.String("err", err.Error()))
		}
	}()

	indexDir := filepath.Join(lc.BrainDir(), "_index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return fmt.Errorf("mkdir index dir: %w", err)
	}
	_, _ = working.ReapOrphanedShards(indexDir)

	oll := ollama.New(ollama.OptionsFromEnv())
	idx, err := index.Open(indexDir, oll.Dims())
	if err != nil {
		return fmt.Errorf("open index: %w", err)
	}
	defer idx.Close()

	wm, err := working.Open(indexDir)
	if err != nil {
		return fmt.Errorf("open working memory: %w", err)
	}
	defer wm.Close()

	srv := server.NewMCPServer("phantom-brain", version.Version, server.WithToolCapabilities(false))
	pbmcp.NewServer(pbmcp.ServerDeps{
		Index:     idx,
		Working:   wm,
		Embedder:  oll,
		VaultDir:  lc.VaultDir(),
		Lifecycle: lc,
		Client:    lc.Client(), // Phase 6: POST writes through here
	}).Register(srv)

	// Serve in a goroutine so signals can interrupt cleanly.
	srvErr := make(chan error, 1)
	go func() { srvErr <- server.ServeStdio(srv) }()
	select {
	case <-ctx.Done():
		logger.Info("phantom-brain: shutdown signal received")
		return nil
	case err := <-srvErr:
		return err
	}
}

// resolveLegacyIndexDir mirrors src/config.ts:resolveIndexPath from the
// TS MCP server: the per-profile per-vault index lives at
//
//	$XDG_CONFIG_HOME/phantom-brain/profiles/<profile>/<vault>/_index/
//
// with $HOME/.config as the XDG fallback and "default" as the profile
// fallback. The vault segment is the basename of BRAIN_VAULT_PATH.
//
// This keeps the Go MCP server operating on the same _index/ files
// the TS server uses, so a brain born under the old runtime can be
// queried by the new one without migration.
func resolveLegacyIndexDir(vaultPath string) (string, error) {
	if override := strings.TrimSpace(os.Getenv("BRAIN_INDEX_PATH")); override != "" {
		return expandHome(override), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve $HOME: %w", err)
	}
	xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	profile := strings.TrimSpace(os.Getenv("WORKSPACE_PROFILE"))
	if profile == "" {
		profile = "default"
	}
	return filepath.Join(xdg, "phantom-brain", "profiles", profile, filepath.Base(vaultPath), "_index"), nil
}

// expandHome turns a leading ~/ into $HOME. The TS layer does the same
// thing; needed because Claude Code's MCP env block doesn't expand the
// shell tilde.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~/"))
}

// serveCmd runs the v5.0 HTTP daemon (Phase 2). Reads config from
// PHANTOM_BRAIN_CONFIG_DIR (default ~/.config/phantom-brain-server),
// keeps state under PHANTOM_BRAIN_DATA_DIR (default /var/lib/phantom-
// brain). SIGHUP reloads the vault registry; SIGINT/SIGTERM drain.
// migrateLegacyCmd ports an existing v4.x TS-style vault into the
// v5.0 agent layout. One-time operation; safe to run idempotently
// (refuses if a brain already exists for the configured (profile,
// vault)).
//
//	BRAIN_LEGACY_VAULT_PATH=/path/to/old/vault \
//	  CL_BRAIN_API=... CL_WORKSPACE_PROFILE=personal CL_BRAIN_VAULT=memory \
//	  pbrainctl migrate-legacy
func migrateLegacyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate-legacy",
		Short: "Copy a v4.x TS-era vault into the v5.0 agent layout (one-time)",
		Long: `Reads BRAIN_LEGACY_VAULT_PATH and copies its contents into a fresh
brain dir under $XDG_DATA_HOME/phantom-brain/{profile}/{vault}/brains/<brain_id>/.
Stamps a manifest with seed_source = "legacy-migration". Refuses if a brain
already exists for this (profile, vault) — delete the existing brain dir
first if you want to force a fresh migration. The source vault is NOT
deleted; remove it manually after verifying the next snapshot picks up
the migrated content.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			legacyPath := strings.TrimSpace(os.Getenv("BRAIN_LEGACY_VAULT_PATH"))
			if legacyPath == "" {
				return fmt.Errorf("BRAIN_LEGACY_VAULT_PATH is required")
			}
			legacyPath = expandHome(legacyPath)
			agent, err := config.LoadAgent()
			if err != nil {
				return err
			}
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			res, err := brain.MigrateLegacyVault(legacyPath, agent, logger)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"migrated %d files to %s (brain_id=%s)\n",
				res.CopiedFiles, res.BrainDir, res.BrainID,
			)
			return nil
		},
	}
}

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run as the HTTP daemon (Phase 2)",
		Long: `Starts the phantom-brain HTTP daemon: per-(profile, vault) reaper +
synthesizer + snapshot publisher, plus the v4.4 §8 API.

Required config dir layout (default ~/.config/phantom-brain-server):

  server.toml
  profiles/<profile>/vaults/<vault>/config.toml  (optional)
  profiles/<profile>/vaults/<vault>/auth.toml    (bearer_token)

State lives under PHANTOM_BRAIN_DATA_DIR (default /var/lib/phantom-brain).
The daemon acquires an exclusive flock on {data}/_daemon/locks/brain-server.pid
to prevent a second instance from corrupting the data dir.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			d, err := pbserver.Start(pbserver.StartOpts{
				ConfigDir: pbserver.DefaultConfigDir(),
				DataDir:   pbserver.DefaultDataDir(),
				Logger:    logger,
			})
			if err != nil {
				return err
			}
			return d.Run()
		},
	}
}
