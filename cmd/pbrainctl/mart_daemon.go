package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/mart"
)

// martDaemonCmd manages a macOS LaunchAgent that runs `mart sync` on a schedule
// — the "daemon" is launchd + a one-shot sync (no long-running process). The
// plist carries NO secret: the scheduled job resolves its token from the
// credentials store (`mart cred add`), so token rotation needs no reinstall.
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

// martAllLabel is the single job that syncs every configured mart (--all).
const martAllLabel = "com.phantom-brain.marts"

// daemonLabels returns the (primary, reconcile) launchd labels for a
// name-scoped or --all daemon.
func daemonLabels(all bool, name string) (primary, reconcile string) {
	if all {
		return martAllLabel, martAllLabel + ".reconcile"
	}
	return martLabel(name), martReconcileLabel(name)
}

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
		all         bool
	)
	c := &cobra.Command{
		Use:   "install [<name>]",
		Short: "Install/replace a LaunchAgent that syncs a mart (or all marts) on an interval",
		Long: `Writes a LaunchAgent under ~/Library/LaunchAgents that runs 'mart sync' on
an interval. The token is NOT baked into the plist — the job reads it from the
credentials store ('mart cred add'), so rotating a token needs no reinstall.
--all installs one job that syncs every configured mart across profiles.
--reconcile-at HH:MM adds a daily full 'mart build' to prune upstream deletes.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkAllArg(all, args); err != nil {
				return err
			}
			if interval < 30 {
				return fmt.Errorf("--interval must be >= 30 seconds")
			}
			configDir := resolveConfigDir(cmd)
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("resolve pbrainctl path: %w", err)
			}
			logDir := filepath.Join(configDir, "marts", "logs")
			if err := os.MkdirAll(logDir, 0o700); err != nil {
				return fmt.Errorf("create log dir: %w", err)
			}
			// The scheduled job resolves creds from the store — NO secret in the
			// plist. launchd doesn't inherit the shell env, so set PATH/HOME.
			env := map[string]string{"PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"}
			if home, herr := os.UserHomeDir(); herr == nil {
				env["HOME"] = home
			}

			var label, logBase, target string
			if all {
				store, _ := mart.LoadCredentials(configDir)
				if len(store.Bindings) == 0 {
					return fmt.Errorf("no stored credentials — run `pbrainctl client mart cred add` for each profile before `daemon install --all`")
				}
				label, logBase, target = martAllLabel, "_all", "--all"
			} else {
				name := args[0]
				spec, err := mart.OpenRegistry(configDir).Load(name)
				if err != nil {
					return err
				}
				// Persist the spec's creds to the store (resolve from store or a
				// matching env) so the storeless job can read them.
				api, token, err := resolveMartCreds(cmd, spec)
				if err != nil {
					return err
				}
				store, err := mart.LoadCredentials(configDir)
				if err != nil {
					return err
				}
				store.Set(mart.Credential{Profile: spec.Profile, Vault: spec.Vault, API: api, Token: token})
				if err := mart.SaveCredentials(configDir, store); err != nil {
					return err
				}
				label, logBase, target = martLabel(name), name, name
			}

			syncArgs := []string{exe, "client", "mart", "--config-dir", configDir, "sync", target}
			plist := renderPlist(label, syncArgs, env,
				filepath.Join(logDir, logBase+".out"), filepath.Join(logDir, logBase+".err"), &interval, "")
			if err := installPlist(cmd, label, plist); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "installed %s (sync every %ds) → logs in %s\n", label, interval, logDir)

			if reconcileAt != "" {
				hh, mm, perr := parseHHMM(reconcileAt)
				if perr != nil {
					return perr
				}
				rlabel := label + ".reconcile"
				buildArgs := []string{exe, "client", "mart", "--config-dir", configDir, "build", target}
				rplist := renderPlist(rlabel, buildArgs, env,
					filepath.Join(logDir, logBase+".reconcile.out"), filepath.Join(logDir, logBase+".reconcile.err"),
					nil, fmt.Sprintf("%02d:%02d", hh, mm))
				if err := installPlist(cmd, rlabel, rplist); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "installed %s (full rebuild daily at %02d:%02d)\n", rlabel, hh, mm)
			}
			return nil
		},
	}
	c.Flags().IntVar(&interval, "interval", 900, "seconds between incremental syncs")
	c.Flags().StringVar(&reconcileAt, "reconcile-at", "", "also run a daily full rebuild at HH:MM (prunes upstream-deleted records)")
	c.Flags().BoolVar(&all, "all", false, "one job that syncs every configured mart across profiles")
	return c
}

func martDaemonUninstallCmd() *cobra.Command {
	var (
		purge bool
		all   bool
	)
	c := &cobra.Command{
		Use:   "uninstall [<name>]",
		Short: "Remove a mart's LaunchAgent(s) (or the --all job)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkAllArg(all, args); err != nil {
				return err
			}
			name := ""
			if !all {
				name = args[0]
			}
			primary, reconcile := daemonLabels(all, name)
			for _, label := range []string{primary, reconcile} {
				_ = bootout(label) // ignore "not loaded"
				if p, err := launchAgentPath(label); err == nil {
					if rmErr := os.Remove(p); rmErr != nil && !os.IsNotExist(rmErr) {
						return fmt.Errorf("remove %s: %w", p, rmErr)
					}
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "uninstalled LaunchAgent(s): %s\n", primary)
			if purge && !all {
				if err := mart.OpenRegistry(resolveConfigDir(cmd)).RemoveCursor(name); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "removed the sync cursor (next sync re-reads from the beginning)")
			} else if purge && all {
				fmt.Fprintln(cmd.OutOrStdout(), "note: --purge is per-mart; use `mart daemon uninstall <name> --purge` to drop a specific cursor")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&purge, "purge", false, "also delete the sync cursor (single mart only)")
	c.Flags().BoolVar(&all, "all", false, "remove the --all job instead of a named mart")
	return c
}

func martDaemonStatusCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "status [<name>]",
		Short: "Show a mart LaunchAgent's load state + last exit + cursor age",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkAllArg(all, args); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			name := ""
			if !all {
				name = args[0]
			}
			label, _ := daemonLabels(all, name)
			res, _ := exec.Command("launchctl", "list", label).CombinedOutput()
			if s := strings.TrimSpace(string(res)); s != "" {
				fmt.Fprintf(out, "%s:\n%s\n", label, s)
			} else {
				fmt.Fprintf(out, "%s: not loaded\n", label)
			}
			if all {
				fmt.Fprintln(out, "cursor: per-mart (see `pbrainctl client mart list`)")
				return nil
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
	c.Flags().BoolVar(&all, "all", false, "status of the --all job")
	return c
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
