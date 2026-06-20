// pbrainctl is the single binary for phantom-brain v5: MCP server, daemon, and
// operator CLI in one. The subcommand picks the mode.
//
// Phase 0 wires the cobra skeleton, a real `version` subcommand, and the
// `mcp` subcommand backed by internal/mcp. Tool surface grows across
// Days 10-12; `serve` and the operator subcommands remain stubs until
// Phase 2-3.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"github.com/mindmorass/mcp-phantom-brain/internal/index"
	pbmcp "github.com/mindmorass/mcp-phantom-brain/internal/mcp"
	"github.com/mindmorass/mcp-phantom-brain/internal/ollama"
	"github.com/mindmorass/mcp-phantom-brain/internal/vault"
	"github.com/mindmorass/mcp-phantom-brain/internal/version"
	"github.com/mindmorass/mcp-phantom-brain/internal/working"
)

func main() {
	root := &cobra.Command{
		Use:   "pbrainctl",
		Short: "phantom-brain — MCP server, daemon, and operator CLI",
		Long: `pbrainctl is a single binary serving three modes:

  pbrainctl mcp          stdio JSON-RPC MCP server (per agent process)
  pbrainctl serve        HTTP daemon (per-(profile, vault) reaper + synthesizer)
  pbrainctl <op>         operator commands (list, snapshot, vault, ...)

See https://github.com/mindmorass/mcp-phantom-brain for the v5 spec.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(versionCmd())
	root.AddCommand(mcpCmd())
	root.AddCommand(serveCmd())

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

// mcpCmd runs the stdio JSON-RPC MCP server.
//
// Path resolution: reads the same env vars the v4.x TypeScript MCP
// server used (BRAIN_VAULT_PATH, WORKSPACE_PROFILE) so the Go binary
// operates against the existing vault on disk. The v5.0 deploy
// contract (CL_BRAIN_*) takes over once Phase 1 brain birth lands;
// that's a small change to this function that doesn't ripple through
// the rest of internal/mcp.
func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run as a stdio JSON-RPC MCP server",
		Long: `Starts an MCP server on stdio. Intended to be spawned by Claude Code
via .claude.json mcpServers entries.

Required env vars (Phase 0):
  BRAIN_VAULT_PATH    Absolute path to the vault directory.

Optional:
  WORKSPACE_PROFILE   Profile name (default "default"). Used to derive the
                      per-profile index directory under XDG_CONFIG_HOME.
  OLLAMA_BASE_URL     Ollama endpoint (default http://localhost:11434).
  EMBEDDING_MODEL     Model name (default nomic-embed-text).
  EMBEDDING_DIMS      Embedding dimensionality (default 768).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			vaultDir := strings.TrimSpace(os.Getenv("BRAIN_VAULT_PATH"))
			if vaultDir == "" {
				return fmt.Errorf("BRAIN_VAULT_PATH is required")
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

			// Reap any orphan working-memory shards from crashed prior
			// processes before opening our own. Best-effort: a failure
			// here doesn't block startup.
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

			srv := server.NewMCPServer(
				"phantom-brain",
				version.Version,
				server.WithToolCapabilities(false),
			)
			pbmcp.NewServer(pbmcp.ServerDeps{
				Index:    idx,
				Working:  wm,
				Embedder: oll,
				VaultDir: vaultDir,
			}).Register(srv)

			return server.ServeStdio(srv)
		},
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

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run as the HTTP daemon (not implemented in Phase 0)",
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("serve: not implemented in Phase 0 (see v5 spec, Phase 2)")
		},
	}
}
