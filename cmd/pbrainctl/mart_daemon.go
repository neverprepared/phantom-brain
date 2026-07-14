package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/config"
	"github.com/neverprepared/phantom-brain/internal/mart"
)

// martDaemonCmd manages a macOS LaunchAgent that runs `mart sync <name>` on a
// schedule — the "daemon" is launchd + a one-shot sync (no long-running
// process). The plist embeds the agent-env creds (launchd does NOT inherit the
// shell env) and is written 0600 because it carries the bearer token.
func martDaemonCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "daemon",
		Short: "Schedule incremental mart refresh via a macOS LaunchAgent",
	}
	c.AddCommand(martDaemonInstallCmd(), martDaemonUninstallCmd(), martDaemonStatusCmd())
	return c
}

func martLabel(name string) string          { return "com.phantom-brain.mart." + name }
func martReconcileLabel(name string) string { return martLabel(name) + ".reconcile" }

func launchAgentPath(label string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// plistEsc XML-escapes a value interpolated into the plist (paths, the token).
var plistEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace

func martDaemonInstallCmd() *cobra.Command {
	var (
		interval    int
		reconcileAt string
	)
	c := &cobra.Command{
		Use:   "install <name>",
		Short: "Install/replace a LaunchAgent that syncs the mart on an interval",
		Long: `Writes ~/Library/LaunchAgents/com.phantom-brain.mart.<name>.plist (0600 —
it embeds CL_BRAIN_API_TOKEN, since launchd does not inherit your shell env),
then bootstraps it. --reconcile-at HH:MM adds a second daily job that runs a
full 'mart build' to prune records deleted upstream (the change feed can't see
deletes).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if interval < 30 {
				return fmt.Errorf("--interval must be >= 30 seconds")
			}
			configDir := resolveConfigDir(cmd)
			reg := mart.OpenRegistry(configDir)
			spec, err := reg.Load(name)
			if err != nil {
				return err
			}
			// Bake the CURRENT env creds into the plist; refuse if they don't
			// match the mart's tenant (else the scheduled sync projects the
			// wrong tenant).
			agent, err := config.LoadAgent()
			if err != nil {
				return fmt.Errorf("requires the agent contract env vars (CL_BRAIN_API, CL_BRAIN_API_TOKEN, CL_WORKSPACE_PROFILE, CL_BRAIN_VAULT): %w", err)
			}
			if agent.Profile != spec.Profile || agent.Vault != spec.Vault {
				return fmt.Errorf("mart %q targets %s/%s but the agent env is bound to %s/%s", name, spec.Profile, spec.Vault, agent.Profile, agent.Vault)
			}
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("resolve pbrainctl path: %w", err)
			}
			logDir := filepath.Join(configDir, "marts", "logs")
			if err := os.MkdirAll(logDir, 0o700); err != nil {
				return fmt.Errorf("create log dir: %w", err)
			}

			env := map[string]string{
				"PATH":                 "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
				"CL_BRAIN_API":         agent.API,
				"CL_BRAIN_API_TOKEN":   agent.Token,
				"CL_WORKSPACE_PROFILE": agent.Profile,
				"CL_BRAIN_VAULT":       agent.Vault,
			}
			if home, herr := os.UserHomeDir(); herr == nil {
				env["HOME"] = home
			}

			// Primary: interval sync.
			syncArgs := []string{exe, "client", "mart", "--config-dir", configDir, "sync", name}
			plist := renderPlist(martLabel(name), syncArgs, env,
				filepath.Join(logDir, name+".out"), filepath.Join(logDir, name+".err"),
				&interval, "")
			if err := installPlist(cmd, martLabel(name), plist); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "installed %s (sync every %ds) → logs in %s\n", martLabel(name), interval, logDir)

			// Optional: daily full-rebuild reconcile (prunes deletes).
			if reconcileAt != "" {
				hh, mm, perr := parseHHMM(reconcileAt)
				if perr != nil {
					return perr
				}
				buildArgs := []string{exe, "client", "mart", "--config-dir", configDir, "build", name}
				rplist := renderPlist(martReconcileLabel(name), buildArgs, env,
					filepath.Join(logDir, name+".reconcile.out"), filepath.Join(logDir, name+".reconcile.err"),
					nil, fmt.Sprintf("%02d:%02d", hh, mm))
				if err := installPlist(cmd, martReconcileLabel(name), rplist); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "installed %s (full rebuild daily at %02d:%02d)\n", martReconcileLabel(name), hh, mm)
			}
			return nil
		},
	}
	c.Flags().IntVar(&interval, "interval", 900, "seconds between incremental syncs")
	c.Flags().StringVar(&reconcileAt, "reconcile-at", "", "also run a daily full rebuild at HH:MM (prunes upstream-deleted records)")
	return c
}

func martDaemonUninstallCmd() *cobra.Command {
	var purge bool
	c := &cobra.Command{
		Use:   "uninstall <name>",
		Short: "Remove a mart's LaunchAgent(s)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			for _, label := range []string{martLabel(name), martReconcileLabel(name)} {
				_ = bootout(label) // ignore "not loaded"
				if p, err := launchAgentPath(label); err == nil {
					if rmErr := os.Remove(p); rmErr != nil && !os.IsNotExist(rmErr) {
						return fmt.Errorf("remove %s: %w", p, rmErr)
					}
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "uninstalled LaunchAgent(s) for mart %q\n", name)
			if purge {
				if err := mart.OpenRegistry(resolveConfigDir(cmd)).RemoveCursor(name); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "removed the sync cursor (next sync re-reads from the beginning)")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&purge, "purge", false, "also delete the sync cursor")
	return c
}

func martDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <name>",
		Short: "Show a mart LaunchAgent's load state + last exit + cursor age",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			out := cmd.OutOrStdout()
			label := martLabel(name)
			res, _ := exec.Command("launchctl", "list", label).CombinedOutput()
			if s := strings.TrimSpace(string(res)); s != "" {
				fmt.Fprintf(out, "%s:\n%s\n", label, s)
			} else {
				fmt.Fprintf(out, "%s: not loaded\n", label)
			}
			cur := mart.OpenRegistry(resolveConfigDir(cmd)).CursorPath(name)
			if fi, err := os.Stat(cur); err == nil {
				fmt.Fprintf(out, "cursor: %s (updated %s)\n", cur, fi.ModTime().Format("2006-01-02 15:04:05"))
			} else {
				fmt.Fprintf(out, "cursor: none yet (%s)\n", cur)
			}
			return nil
		},
	}
}

// renderPlist builds a LaunchAgent plist. Exactly one of interval / calendar is
// used (interval when non-nil, else a daily StartCalendarInterval at HH:MM).
func renderPlist(label string, args []string, env map[string]string, outPath, errPath string, interval *int, calendarHHMM string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	fmt.Fprintf(&b, "  <key>Label</key><string>%s</string>\n", plistEsc(label))
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, a := range args {
		fmt.Fprintf(&b, "    <string>%s</string>\n", plistEsc(a))
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>RunAtLoad</key><true/>\n")
	if interval != nil {
		fmt.Fprintf(&b, "  <key>StartInterval</key><integer>%d</integer>\n", *interval)
	} else if calendarHHMM != "" {
		// calendarHHMM is always a validated "HH:MM" from parseHHMM.
		hh, mm, _ := parseHHMM(calendarHHMM)
		fmt.Fprintf(&b, "  <key>StartCalendarInterval</key>\n  <dict>\n    <key>Hour</key><integer>%d</integer>\n    <key>Minute</key><integer>%d</integer>\n  </dict>\n", hh, mm)
	}
	b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
	// Stable key order for reproducible plists.
	for _, k := range []string{"PATH", "HOME", "CL_BRAIN_API", "CL_BRAIN_API_TOKEN", "CL_WORKSPACE_PROFILE", "CL_BRAIN_VAULT"} {
		if v, ok := env[k]; ok {
			fmt.Fprintf(&b, "    <key>%s</key><string>%s</string>\n", k, plistEsc(v))
		}
	}
	b.WriteString("  </dict>\n")
	fmt.Fprintf(&b, "  <key>StandardOutPath</key><string>%s</string>\n", plistEsc(outPath))
	fmt.Fprintf(&b, "  <key>StandardErrorPath</key><string>%s</string>\n", plistEsc(errPath))
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// installPlist writes the plist 0600, validates it, and (re)bootstraps it.
func installPlist(cmd *cobra.Command, label, contents string) error {
	path, err := launchAgentPath(label)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	if out, lerr := exec.Command("plutil", "-lint", path).CombinedOutput(); lerr != nil {
		return fmt.Errorf("plist failed validation: %s", strings.TrimSpace(string(out)))
	}
	_ = bootout(label) // clear any prior instance; ignore "not loaded"
	dom := "gui/" + strconv.Itoa(os.Getuid())
	if out, berr := exec.Command("launchctl", "bootstrap", dom, path).CombinedOutput(); berr != nil {
		return fmt.Errorf("launchctl bootstrap failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func bootout(label string) error {
	dom := "gui/" + strconv.Itoa(os.Getuid())
	return exec.Command("launchctl", "bootout", dom+"/"+label).Run()
}

func parseHHMM(s string) (int, int, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("--reconcile-at must be HH:MM, got %q", s)
	}
	hh, err1 := strconv.Atoi(parts[0])
	mm, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, fmt.Errorf("--reconcile-at must be a valid HH:MM (00:00–23:59), got %q", s)
	}
	return hh, mm, nil
}
