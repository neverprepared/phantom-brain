package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

--brains-root overrides Agent.BrainsRoot() for cleaning up stranded
vault bindings whose env vars are no longer in scope.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			agent, err := config.LoadAgent()
			if err != nil {
				return err
			}
			root := brainsRoot
			if root == "" {
				root = agent.BrainsRoot()
			} else {
				root = expandHome(root)
			}

			retention := olderThan
			if retention <= 0 {
				retention = time.Duration(agent.LocalRetentionHours) * time.Hour
			}
			if retention <= 0 && !dryRun {
				return errors.New("retention disabled (CL_BRAIN_LOCAL_RETENTION_HOURS=0 and no --older-than); pass --older-than to force, or run with --dry-run")
			}

			return runGCBrains(cmd.OutOrStdout(), gcBrainsOpts{
				Root:           root,
				CurrentBrainID: agent.BrainID,
				Retention:      retention,
				DryRun:         dryRun,
				IncludeAlive:   includeAlive,
				Now:            time.Now,
				Platform:       brain.NewPlatform(),
			})
		},
	}
	c.Flags().DurationVar(&olderThan, "older-than", 0, "minimum age before a dead brain is eligible (overrides CL_BRAIN_LOCAL_RETENTION_HOURS)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the table without deleting anything")
	c.Flags().BoolVar(&includeAlive, "include-alive", false, "also consider status=alive brains whose flock is takeable and PID is gone")
	c.Flags().StringVar(&brainsRoot, "brains-root", "", "override the agent's BrainsRoot (for stranded vault bindings)")
	return c
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
