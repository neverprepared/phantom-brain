package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/config"
	pbserver "github.com/neverprepared/phantom-brain/internal/server"
)

// All operator subcommands honour PHANTOM_BRAIN_CONFIG_DIR /
// PHANTOM_BRAIN_DATA_DIR (with the daemon defaults) so they can be
// invoked on the daemon host without flags. Most are pure-disk reads;
// `vault reload` needs the daemon PID (read from the global flock
// sidecar). `force-checkpoint` / `force-merge` touch the live
// collective and assume the daemon mutex is not contested — operator
// uses them when something is stuck, not as a routine.

func opsCommonFlags(cmd *cobra.Command) {
	cmd.Flags().String("data-dir", "", "override PHANTOM_BRAIN_DATA_DIR")
	cmd.Flags().String("config-dir", "", "override PHANTOM_BRAIN_CONFIG_DIR")
}

func resolveDataDir(cmd *cobra.Command) pbserver.DataDir {
	if v, _ := cmd.Flags().GetString("data-dir"); v != "" {
		return pbserver.DataDir(expandHome(v))
	}
	return pbserver.DefaultDataDir()
}

func resolveConfigDir(cmd *cobra.Command) string {
	if v, _ := cmd.Flags().GetString("config-dir"); v != "" {
		return expandHome(v)
	}
	return pbserver.DefaultConfigDir()
}

// loadRegistryForOps is the same registry the daemon builds at
// startup. Returns an error rather than panicking so operator
// commands can degrade to "no vaults configured" gracefully.
func loadRegistryForOps(configDir string) (*pbserver.Registry, error) {
	cfg, err := pbserver.LoadServerConfig(configDir)
	if err != nil {
		return nil, err
	}
	r := pbserver.NewRegistry()
	if _, err := r.Load(pbserver.LoadOpts{
		ConfigDir:          configDir,
		Defaults:           cfg.Defaults,
		DefaultIndexPrefix: cfg.OpenSearch.IndexPrefix,
		DefaultBucket:      cfg.Storage.MinIOBucket,
	}); err != nil {
		return nil, err
	}
	return r, nil
}

// --- vault list / status / reload ------------------------------------

func vaultCmd() *cobra.Command {
	c := &cobra.Command{Use: "vault", Short: "Inspect or signal configured vaults"}
	c.AddCommand(vaultListCmd(), vaultStatusCmd(), vaultReloadCmd())
	return c
}

func vaultListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List vaults the daemon would serve",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := loadRegistryForOps(resolveConfigDir(cmd))
			if err != nil {
				return err
			}
			for _, b := range r.Vaults() {
				// v3.2: surface the resolved (index_prefix, bucket) so
				// operators debugging "where did my data land" can see
				// the binding's physical targets at a glance.
				prefix := b.Storage.IndexPrefix
				if prefix == "" {
					prefix = "(default)"
				}
				bucket := b.Storage.Bucket
				if bucket == "" {
					bucket = "(default)"
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"%s\tindex_prefix=%s\tbucket=%s\n",
					b.Key, prefix, bucket)
			}
			return nil
		},
	}
	opsCommonFlags(c)
	return c
}

func vaultStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "Per-vault counts: queue depth, ledger rows, maintenance",
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := resolveDataDir(cmd)
			r, err := loadRegistryForOps(resolveConfigDir(cmd))
			if err != nil {
				return err
			}
			for _, b := range r.Vaults() {
				// queue_pending is always 0 in Phase 6 — the file queue is
				// gone; the daemon's SynthWorker drains an in-memory chan.
				pending := []string(nil)
				ledgerRows := -1
				if l, err := pbserver.OpenLedger(d, b.Key.Profile, b.Key.Vault); err == nil {
					if list, err := l.List(100_000); err == nil {
						ledgerRows = len(list)
					}
					_ = l.Close()
				}
				maint := pbserver.InMaintenance(d, b.Key)
				fmt.Fprintf(cmd.OutOrStdout(),
					"%s\tqueue_pending=%d\tledger_rows=%d\tmaintenance=%v\n",
					b.Key, len(pending), ledgerRows, maint,
				)
			}
			return nil
		},
	}
	opsCommonFlags(c)
	return c
}

func vaultReloadCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "reload",
		Short: "Send SIGHUP to the running daemon (reloads vault registry)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			pid, err := readDaemonPID(resolveDataDir(cmd))
			if err != nil {
				return err
			}
			proc, err := os.FindProcess(pid)
			if err != nil {
				return fmt.Errorf("find pid %d: %w", pid, err)
			}
			if err := proc.Signal(syscall.SIGHUP); err != nil {
				return fmt.Errorf("signal pid %d: %w", pid, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "sent SIGHUP to daemon pid %d\n", pid)
			return nil
		},
	}
	opsCommonFlags(c)
	return c
}

// readDaemonPID reads the pid sidecar the daemon writes next to the
// global flock at startup. Returns an error if the file is missing
// (no daemon running) or unparseable.
func readDaemonPID(d pbserver.DataDir) (int, error) {
	raw, err := os.ReadFile(d.GlobalFlockPath())
	if err != nil {
		return 0, fmt.Errorf("read daemon pid sidecar: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("parse pid %q: %w", raw, err)
	}
	return pid, nil
}

// vaultArgFromArgs pulls the (profile, vault) the operator named. Most
// per-vault ops expose it as a positional arg rather than a flag
// (`pbrainctl server maintenance enter personal/memory`).
func vaultArgFromArgs(args []string) (pbserver.VaultKey, error) {
	if len(args) != 1 {
		return pbserver.VaultKey{}, errors.New("requires exactly one argument: profile/vault")
	}
	parts := strings.SplitN(args[0], "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return pbserver.VaultKey{}, fmt.Errorf("argument must look like profile/vault, got %q", args[0])
	}
	return pbserver.VaultKey{Profile: parts[0], Vault: parts[1]}, nil
}

// --- queue depth / contributors --------------------------------------

func queueCmd() *cobra.Command {
	c := &cobra.Command{Use: "queue", Short: "Synthesis queue inspection"}
	c.AddCommand(queueDepthCmd(), queueContributorsCmd())
	return c
}

func queueDepthCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "depth",
		Short: "Count pending + claimed + dead queue items per vault",
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := resolveDataDir(cmd)
			r, err := loadRegistryForOps(resolveConfigDir(cmd))
			if err != nil {
				return err
			}
			for _, b := range r.Vaults() {
				// queue_pending is always 0 in Phase 6 — the file queue is
				// gone; the daemon's SynthWorker drains an in-memory chan.
				pending := []string(nil)
				claimed := countQueueDir(d, b.Key, "claimed")
				dead := countQueueDir(d, b.Key, "dead")
				done := countQueueDir(d, b.Key, "done")
				fmt.Fprintf(cmd.OutOrStdout(), "%s\tpending=%d\tclaimed=%d\tdead=%d\tdone=%d\n",
					b.Key, len(pending), claimed, dead, done)
			}
			return nil
		},
	}
	opsCommonFlags(c)
	return c
}

// countQueueDir returns the number of .json entries in a queue
// subdir. Returns 0 for missing dirs.
func countQueueDir(d pbserver.DataDir, key pbserver.VaultKey, sub string) int {
	dir := filepath.Join(d.VaultDir(key.Profile, key.Vault), "queue", sub)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

func queueContributorsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "contributors [profile/vault]",
		Short: "List unique contributor_ids in the ledger (most recent first)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := vaultArgFromArgs(args)
			if err != nil {
				return err
			}
			l, err := pbserver.OpenLedger(resolveDataDir(cmd), key.Profile, key.Vault)
			if err != nil {
				return err
			}
			defer l.Close()
			rows, err := l.List(100_000)
			if err != nil {
				return err
			}
			seen := map[string]time.Time{}
			for _, r := range rows {
				if prev, ok := seen[r.ContributorID]; !ok || r.MergedAt.After(prev) {
					seen[r.ContributorID] = r.MergedAt
				}
			}
			type kv struct {
				id   string
				last time.Time
			}
			var sorted []kv
			for id, t := range seen {
				sorted = append(sorted, kv{id, t})
			}
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].last.After(sorted[j].last) })
			for _, kv := range sorted {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\tlast_merged=%s\n",
					kv.id, kv.last.UTC().Format(time.RFC3339))
			}
			return nil
		},
	}
	opsCommonFlags(c)
	return c
}

// --- maintenance enter / exit ----------------------------------------

func maintenanceCmd() *cobra.Command {
	c := &cobra.Command{Use: "maintenance", Short: "Per-vault maintenance kill-switch"}
	c.AddCommand(maintenanceEnterCmd(), maintenanceExitCmd())
	return c
}

func maintenanceEnterCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "enter [profile/vault]",
		Short: "Pause the synthesizer + refuse new merges for this vault",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := vaultArgFromArgs(args)
			if err != nil {
				return err
			}
			if err := pbserver.SetMaintenance(resolveDataDir(cmd), key); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "maintenance ON for %s\n", key)
			return nil
		},
	}
	opsCommonFlags(c)
	return c
}

func maintenanceExitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "exit [profile/vault]",
		Short: "Clear the maintenance flag (synthesizer resumes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := vaultArgFromArgs(args)
			if err != nil {
				return err
			}
			if err := pbserver.ClearMaintenance(resolveDataDir(cmd), key); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "maintenance OFF for %s\n", key)
			return nil
		},
	}
	opsCommonFlags(c)
	return c
}

// --- list / show / orphans (brain-side, reads $XDG_DATA_HOME) --------

// brainListCmd lists every brain dir under the agent's data home.
// Aimed at the agent host, not the daemon — operators check what
// brains a given host has accumulated and whether any are stuck.
func brainListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List local brain dirs for the configured (profile, vault)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			agent, err := config.LoadAgent()
			if err != nil {
				return err
			}
			entries, err := os.ReadDir(agent.BrainsRoot())
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				m, err := brain.ReadManifest(agent.BrainDir(e.Name()))
				if err != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\t(corrupt: %v)\n", e.Name(), err)
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\tlast_heartbeat=%s\tseed=%s\n",
					m.BrainID, m.Status, m.LastHeartbeat, m.SeedSource)
			}
			return nil
		},
	}
	return c
}

func brainShowCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "show [brain_id]",
		Short: "Dump the full manifest for one brain",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agent, err := config.LoadAgent()
			if err != nil {
				return err
			}
			m, err := brain.ReadManifest(agent.BrainDir(args[0]))
			if err != nil {
				return err
			}
			body, _ := json.MarshalIndent(m, "", "  ")
			fmt.Fprintln(cmd.OutOrStdout(), string(body))
			return nil
		},
	}
	return c
}

// brainOrphansCmd lists brains the recovery sweep would mark dead
// without actually marking them. Read-only audit for ops review.
func brainOrphansCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "orphans",
		Short: "Dry-run recovery sweep — list candidates without mutating",
		RunE: func(cmd *cobra.Command, _ []string) error {
			agent, err := config.LoadAgent()
			if err != nil {
				return err
			}
			entries, err := os.ReadDir(agent.BrainsRoot())
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			plat := brain.NewPlatform()
			currentBoot, _ := plat.BootID()
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				dir := agent.BrainDir(e.Name())
				m, err := brain.ReadManifest(dir)
				if err != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\tCORRUPT %v\n", e.Name(), err)
					continue
				}
				if m.Status != brain.StatusAlive {
					continue
				}
				if currentBoot != "" && m.BootID != "" && m.BootID != currentBoot {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\tprior-boot (manifest=%s, current=%s)\n",
						m.BrainID, m.BootID, currentBoot)
					continue
				}
				marker := brain.AliveMarkerPath(dir)
				if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\tno-marker\n", m.BrainID)
					continue
				}
				if !plat.ProcessAlive(m.PID) {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\tpid-dead (pid=%d)\n", m.BrainID, m.PID)
				}
			}
			return nil
		},
	}
	return c
}

// force-merge retired in Phase 6 — the reaper is gone; agent writes
// land in OS as they happen, so there's nothing to drain.

// force-checkpoint retired in Phase 6 — the daemon's async
// SynthWorker drains synth jobs from the in-memory queue; there's
// nothing for an operator subcommand to claim out-of-band.

// newStderrLogger gives operator commands the same log shape the
// daemon uses (stderr text handler) so daemon-style log lines don't
// pollute the human-readable command output on stdout.
func newStderrLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
