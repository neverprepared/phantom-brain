package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/config"
)

// gcBrainsCmd is the operator lever for reclaiming local brain dirs left
// behind by crashed or cleanly-shut-down agents. It is the explicit
// companion to the opportunistic GC pass inside the recovery sweep
// (Stream A of issue #31): same predicate (brain.IsGCEligible), same
// flock re-probe before RemoveAll, but with a dry-run mode, a table
// preview, and the ability to point at a non-default brains root for
// stranded vault bindings.
//
// Effective retention resolves in this order:
//
//  1. --older-than flag if > 0
//  2. CL_BRAIN_LOCAL_RETENTION_HOURS (Agent.LocalRetentionHours)
//
// A resolved retention of 0 disables deletion outright; the subcommand
// refuses to run without --dry-run in that case rather than no-op
// silently.
func gcBrainsCmd() *cobra.Command {
	var (
		olderThan    time.Duration
		dryRun       bool
		includeAlive bool
		brainsRoot   string
		profileFlag  string
		vaultFlag    string
	)
	c := &cobra.Command{
		Use:   "gc-brains",
		Short: "Garbage-collect dead local brain dirs past their retention window",
		Long: `Walks the agent's brains root and deletes brain dirs whose manifests
are status=dead and whose age exceeds the retention threshold. Dry-run
prints the table without touching disk; pass --include-alive to also
consider status=alive brains whose flock is takeable and whose PID is
gone (the same liveness signal the recovery sweep uses).

Retention defaults to CL_BRAIN_LOCAL_RETENTION_HOURS (24h). Setting it
to 0 disables deletion — the subcommand then refuses to run unless
--dry-run is passed.

Binding resolution order:

  1. --brains-root <path>            explicit escape hatch, wins outright
  2. --profile X --vault Y           scopes to that one binding, no env required
  3. CL_WORKSPACE_PROFILE/CL_BRAIN_VAULT (+ CL_BRAIN_API/_TOKEN) set in env
  4. neither: walks every <XDG_DATA_HOME>/phantom-brain/*/*/brains discovered
     on disk and runs GC per binding. Useful from a plain shell where no
     agent env contract is in scope.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			retention := olderThan
			// Path 1: explicit root override. Honor it with no binding resolution.
			if brainsRoot != "" {
				root := expandHome(brainsRoot)
				if retention <= 0 {
					retention = 24 * time.Hour
				}
				if retention <= 0 && !dryRun {
					return errRetentionDisabled()
				}
				return runGCBrains(out, gcBrainsOpts{
					Root:         root,
					Retention:    retention,
					DryRun:       dryRun,
					IncludeAlive: includeAlive,
					Now:          time.Now,
					Platform:     brain.NewPlatform(),
				})
			}

			// Path 2: --profile/--vault both set. Resolve directly off
			// XDG_DATA_HOME without requiring CL_BRAIN_API/_TOKEN.
			profileSet := strings.TrimSpace(profileFlag) != ""
			vaultSet := strings.TrimSpace(vaultFlag) != ""
			if profileSet != vaultSet {
				return errors.New("--profile and --vault must be set together")
			}
			if profileSet && vaultSet {
				if err := validateBindingSegment("profile", profileFlag); err != nil {
					return err
				}
				if err := validateBindingSegment("vault", vaultFlag); err != nil {
					return err
				}
				dataHome, err := resolveDataHomeFromEnv()
				if err != nil {
					return err
				}
				root := bindingBrainsRoot(dataHome, profileFlag, vaultFlag)
				if retention <= 0 {
					retention = 24 * time.Hour
				}
				if retention <= 0 && !dryRun {
					return errRetentionDisabled()
				}
				return runGCBrains(out, gcBrainsOpts{
					Root:         root,
					Retention:    retention,
					DryRun:       dryRun,
					IncludeAlive: includeAlive,
					Now:          time.Now,
					Platform:     brain.NewPlatform(),
				})
			}

			// Path 3: full agent env contract present (CL_BRAIN_API etc.).
			// LoadAgent fails fast if any of the four required vars is missing,
			// in which case we fall through to Path 4 (bindless walk).
			if agent, err := config.LoadAgent(); err == nil {
				eff := retention
				if eff <= 0 {
					eff = time.Duration(agent.LocalRetentionHours) * time.Hour
				}
				if eff <= 0 && !dryRun {
					return errRetentionDisabled()
				}
				return runGCBrains(out, gcBrainsOpts{
					Root:           agent.BrainsRoot(),
					CurrentBrainID: agent.BrainID,
					Retention:      eff,
					DryRun:         dryRun,
					IncludeAlive:   includeAlive,
					Now:            time.Now,
					Platform:       brain.NewPlatform(),
				})
			}

			// Path 4: bindless walk. Discover every binding on disk.
			dataHome, err := resolveDataHomeFromEnv()
			if err != nil {
				return err
			}
			if retention <= 0 {
				retention = 24 * time.Hour
			}
			if retention <= 0 && !dryRun {
				return errRetentionDisabled()
			}
			return walkAllBindings(out, gcBrainsOpts{
				Retention:    retention,
				DryRun:       dryRun,
				IncludeAlive: includeAlive,
				Now:          time.Now,
				Platform:     brain.NewPlatform(),
			}, dataHome)
		},
	}
	c.Flags().DurationVar(&olderThan, "older-than", 0, "minimum age before a dead brain is eligible (overrides CL_BRAIN_LOCAL_RETENTION_HOURS)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the table without deleting anything")
	c.Flags().BoolVar(&includeAlive, "include-alive", false, "also consider status=alive brains whose flock is takeable and PID is gone")
	c.Flags().StringVar(&brainsRoot, "brains-root", "", "override binding resolution and scan this directory directly")
	c.Flags().StringVar(&profileFlag, "profile", "", "scope to one binding without requiring CL_WORKSPACE_PROFILE in env (requires --vault)")
	c.Flags().StringVar(&vaultFlag, "vault", "", "scope to one binding without requiring CL_BRAIN_VAULT in env (requires --profile)")
	return c
}

// errRetentionDisabled is the shared message for the three RunE branches
// that need to bail when retention resolves to 0 outside --dry-run.
func errRetentionDisabled() error {
	return errors.New("retention disabled (CL_BRAIN_LOCAL_RETENTION_HOURS=0 and no --older-than); pass --older-than to force, or run with --dry-run")
}

// validateBindingSegment refuses path traversal in --profile/--vault flag
// values. The bindless walk uses these as filesystem path segments via
// filepath.Join; a "../etc" would let an operator escape the data root.
func validateBindingSegment(name, v string) error {
	if v == "" {
		return fmt.Errorf("--%s must not be empty", name)
	}
	if strings.ContainsAny(v, "/\\") || v == "." || v == ".." {
		return fmt.Errorf("--%s %q contains path separators", name, v)
	}
	return nil
}

// resolveDataHomeFromEnv mirrors internal/config.resolveDataHome but
// reads the process environment directly. Duplicated rather than
// exporting the unexported helper — four lines, tighter blast radius.
func resolveDataHomeFromEnv() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return xdg, nil
	}
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		return "", errors.New("gc-brains: neither XDG_DATA_HOME nor HOME is set; cannot resolve data dir")
	}
	return filepath.Join(home, ".local", "share"), nil
}

// bindingBrainsRoot mirrors Agent.BrainsRoot()'s layout:
// <dataHome>/phantom-brain/<profile>/<vault>/brains.
func bindingBrainsRoot(dataHome, profile, vault string) string {
	return filepath.Join(dataHome, "phantom-brain", profile, vault, "brains")
}

// walkAllBindings discovers every brains/ dir beneath dataHome's
// phantom-brain tree and runs GC against each. Empty discovery prints
// a single notice and returns nil — that's the right answer for a
// freshly provisioned host with no brains yet.
func walkAllBindings(out io.Writer, opts gcBrainsOpts, dataHome string) error {
	pattern := filepath.Join(dataHome, "phantom-brain", "*", "*", "brains")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("scan %s: %w", pattern, err)
	}
	sort.Strings(matches)
	fmt.Fprintf(out, "Walking all bindings under %s (no profile/vault scope set).\n",
		filepath.Join(dataHome, "phantom-brain"))
	if len(matches) == 0 {
		fmt.Fprintf(out, "no bindings found under %s\n", filepath.Join(dataHome, "phantom-brain"))
		return nil
	}
	for _, root := range matches {
		// Pretty header is "<profile>/<vault>"; recover it from the path.
		vault := filepath.Base(filepath.Dir(root))
		profile := filepath.Base(filepath.Dir(filepath.Dir(root)))
		fmt.Fprintf(out, "# %s/%s\n", profile, vault)
		// No CurrentBrainID — cross-binding sweep has no notion of "self".
		bindingOpts := opts
		bindingOpts.Root = root
		if err := runGCBrains(out, bindingOpts); err != nil {
			return fmt.Errorf("%s/%s: %w", profile, vault, err)
		}
	}
	return nil
}

// gcBrainsOpts is the interior dependency surface for runGCBrains so
// tests can inject a fake clock and platform without touching env vars.
type gcBrainsOpts struct {
	Root           string
	CurrentBrainID string
	Retention      time.Duration
	DryRun         bool
	IncludeAlive   bool
	Now            func() time.Time
	Platform       brain.Platform
}

// gcRow is one line in the operator-facing table.
type gcRow struct {
	BrainID string
	Status  string
	Age     time.Duration
	Bytes   int64
	Action  string
}

func runGCBrains(out io.Writer, opts gcBrainsOpts) error {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	now := opts.Now()

	entries, err := os.ReadDir(opts.Root)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(out, "no brains root at %s\n", opts.Root)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read brains root: %w", err)
	}

	var rows []gcRow
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		brainID := e.Name()
		dir := filepath.Join(opts.Root, brainID)

		// Belt-and-braces: refuse to descend into anything whose parent
		// isn't the root we were asked to scan. Defends against
		// snapcache-style symlinks landing under brains/ in a future
		// layout change.
		if filepath.Dir(dir) != opts.Root {
			rows = append(rows, gcRow{BrainID: brainID, Status: "?", Action: "keep:path assertion failed"})
			continue
		}

		// Never touch the current brain even if its manifest claims dead.
		if opts.CurrentBrainID != "" && brainID == opts.CurrentBrainID {
			rows = append(rows, gcRow{BrainID: brainID, Status: "self", Action: "keep:current brain"})
			continue
		}

		m, err := brain.ReadManifest(dir)
		if err != nil {
			rows = append(rows, gcRow{BrainID: brainID, Status: "?", Action: fmt.Sprintf("keep:manifest:%v", err)})
			continue
		}

		age := manifestAgeForRow(m, dir, now)
		size, _ := dirSizeBytes(dir)
		row := gcRow{
			BrainID: m.BrainID,
			Status:  string(m.Status),
			Age:     age,
			Bytes:   size,
		}

		switch m.Status {
		case brain.StatusDead:
			eligible, reason := brain.IsGCEligible(m, dir, now, opts.Retention)
			if eligible {
				row.Action = "delete"
			} else {
				row.Action = "keep:" + reason
			}
		case brain.StatusAlive:
			if !opts.IncludeAlive {
				row.Action = "keep:status=alive"
				break
			}
			// Same liveness gate the recovery sweep uses: flock must be
			// takeable AND PID must be gone AND heartbeat must be stale
			// past retention.
			ok, reason := aliveIsEligible(m, dir, age, opts.Retention, opts.Platform)
			if ok {
				row.Action = "delete"
			} else {
				row.Action = "keep:" + reason
			}
		default:
			row.Action = "keep:status=" + string(m.Status)
		}
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].BrainID < rows[j].BrainID })
	printGCTable(out, rows)

	if opts.DryRun {
		return nil
	}

	for i, r := range rows {
		if r.Action != "delete" {
			continue
		}
		dir := filepath.Join(opts.Root, r.BrainID)
		// Take the flock and hold it across RemoveAll. Releasing
		// before deletion (the old flockHeld behavior) left a race
		// where a sibling could birth into the marker between
		// Unlock and RemoveAll and have its fresh dir wiped.
		marker := brain.AliveMarkerPath(dir)
		var heldLock *flock.Flock
		if _, err := os.Stat(marker); err == nil {
			lk := flock.New(marker)
			took, lockErr := lk.TryLock()
			if lockErr != nil {
				rows[i].Action = "error:flock probe:" + lockErr.Error()
				fmt.Fprintf(out, "skip %s: %s\n", r.BrainID, rows[i].Action)
				continue
			}
			if !took {
				rows[i].Action = "error:flock acquired between sweep and delete"
				fmt.Fprintf(out, "skip %s: %s\n", r.BrainID, rows[i].Action)
				continue
			}
			heldLock = lk
		}
		if err := os.RemoveAll(dir); err != nil {
			rows[i].Action = "error:" + err.Error()
			if heldLock != nil {
				_ = heldLock.Unlock()
			}
			fmt.Fprintf(out, "skip %s: %s\n", r.BrainID, rows[i].Action)
			continue
		}
		if heldLock != nil {
			_ = heldLock.Unlock()
		}
		fmt.Fprintf(out, "deleted %s (%d bytes, age=%s)\n", r.BrainID, r.Bytes, r.Age.Truncate(time.Second))
	}
	return nil
}

// aliveIsEligible reports whether an alive-status brain is safe to GC
// under --include-alive. Mirrors recovery.go's freshness check: the
// flock must be takeable (no live holder), the PID must be gone, and
// the manifest age must exceed retention.
func aliveIsEligible(m *brain.Manifest, dir string, age, retention time.Duration, plat brain.Platform) (bool, string) {
	if age < retention {
		return false, fmt.Sprintf("too young (age=%s)", age.Truncate(time.Second))
	}
	marker := brain.AliveMarkerPath(dir)
	if _, err := os.Stat(marker); err == nil {
		lk := flock.New(marker)
		took, err := lk.TryLock()
		if err != nil {
			return false, fmt.Sprintf("flock probe: %v", err)
		}
		if !took {
			return false, "flock held"
		}
		_ = lk.Unlock()
	}
	if plat != nil && plat.ProcessAlive(m.PID) {
		return false, fmt.Sprintf("pid %d alive", m.PID)
	}
	return true, "stale alive"
}

// manifestAgeForRow returns the age the table should display. Same
// fallback ladder as brain.IsGCEligible — heartbeat first, manifest
// mtime second — so the displayed age matches the eligibility decision.
func manifestAgeForRow(m *brain.Manifest, dir string, now time.Time) time.Duration {
	if m.LastHeartbeat != "" {
		if t, err := time.Parse(time.RFC3339, m.LastHeartbeat); err == nil {
			return now.Sub(t)
		}
	}
	if st, err := os.Stat(brain.ManifestPath(dir)); err == nil {
		return now.Sub(st.ModTime())
	}
	return 0
}

// dirSizeBytes sums the size of every regular file under dir. Errors
// during the walk are ignored — the size column is advisory.
func dirSizeBytes(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func printGCTable(out io.Writer, rows []gcRow) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BRAIN_ID\tSTATUS\tAGE\tSIZE_BYTES\tACTION")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
			r.BrainID, r.Status, r.Age.Truncate(time.Second), r.Bytes, r.Action)
	}
	_ = tw.Flush()
}
