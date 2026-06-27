package main

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/config"
)

// clientReflectCmd / clientForgetCmd expose the v3.3 brain_reflect
// maintenance cycle (issue #72 Phase 1) on the operator CLI. Both
// build a brain.Client from the agent-contract env (CL_BRAIN_API,
// CL_BRAIN_API_TOKEN, CL_WORKSPACE_PROFILE, CL_BRAIN_VAULT) via
// config.LoadAgent — same resolution path `queue drain-now` uses.
//
//   reflect         print forget-candidate SHAs (read-only)
//   forget <sha>    delete one summary by SHA (+ snapshot rebuild)

func newBrainClientFromEnv() (*brain.Client, error) {
	agent, err := config.LoadAgent()
	if err != nil {
		return nil, fmt.Errorf("requires the agent contract env vars (CL_BRAIN_API, CL_BRAIN_API_TOKEN, CL_WORKSPACE_PROFILE, CL_BRAIN_VAULT): %w", err)
	}
	return brain.NewClient(brain.ClientOpts{
		BaseURL: agent.API,
		Token:   agent.Token,
		Timeout: 30 * time.Second,
	})
}

func clientReflectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reflect",
		Short: "Report long-term-memory forget-candidates (read-only)",
		Long: `Asks the daemon for forget-candidate SHAs and prints them. Phase 1
surfaces "stale-gate" docs — summaries the synthesis gate never
enriched. Review the list, then run "pbrainctl client forget <sha>"
on the SHAs you approve. Deletes nothing itself (propose-then-apply).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newBrainClientFromEnv()
			if err != nil {
				return err
			}
			resp, err := client.Reflect(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(resp.Candidates) == 0 {
				fmt.Fprintln(out, "no stale-gate candidates — long-term memory looks clean")
				return nil
			}
			fmt.Fprintf(out, "%d forget-candidate(s):\n\n", len(resp.Candidates))
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "SHA\tREASON\tTITLE")
			for _, c := range resp.Candidates {
				title := c.Title
				if strings.TrimSpace(title) == "" {
					title = "(untitled)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", c.SHA, c.Reason, title)
			}
			_ = tw.Flush()
			fmt.Fprintln(out, "\nApprove one with: pbrainctl client forget <sha>")
			return nil
		},
	}
}

func clientForgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forget <sha>",
		Short: "Delete one long-term-memory summary by SHA",
		Long: `Deletes the summary doc at <sha> on the daemon. The removal takes
effect on the next online brain_recall (Phase D2b: recall reads the
daemon's Postgres store).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sha := strings.TrimSpace(args[0])
			if sha == "" {
				return fmt.Errorf("forget requires a non-empty sha")
			}
			client, err := newBrainClientFromEnv()
			if err != nil {
				return err
			}
			resp, err := client.Forget(cmd.Context(), sha)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"forgot %s (forgotten=%t)\ntakes effect on the next online brain_recall\n",
				resp.SHA, resp.Forgotten)
			return nil
		},
	}
}

// clientResynthCmd exposes the v3.4 re-synthesis backfill (issue #82) on
// the operator CLI. Defaults to a dry run (safe); --apply starts the
// background backfill that re-processes docs stuck at Synthesised=false.
func clientResynthCmd() *cobra.Command {
	var apply bool
	var limit int
	cmd := &cobra.Command{
		Use:   "resynth",
		Short: "Re-synthesize docs stuck at Synthesised=false (dropped synth jobs)",
		Long: `Reports docs the synth worker never enriched — typically a bulk ingest
that outran the single CLI-bound worker and overflowed its queue. Defaults
to a dry run; pass --apply to start a background backfill that re-processes
them (non-lossy, serialized with the live worker). Unlike "forget" these
docs are kept and re-synthesized, not deleted.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newBrainClientFromEnv()
			if err != nil {
				return err
			}
			resp, err := client.Resynth(cmd.Context(), !apply, limit)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%d doc(s) stuck at Synthesised=false\n", resp.BacklogCount)
			if len(resp.Sample) > 0 {
				fmt.Fprintf(out, "\nsample (up to %d):\n", len(resp.Sample))
				tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
				fmt.Fprintln(tw, "SHA\tTITLE")
				for _, item := range resp.Sample {
					title := item.Title
					if strings.TrimSpace(title) == "" {
						title = "(untitled)"
					}
					fmt.Fprintf(tw, "%s\t%s\n", item.SHA, title)
				}
				_ = tw.Flush()
			}
			if !apply {
				fmt.Fprintln(out, "\ndry run — re-run with --apply to re-synthesize")
				return nil
			}
			if resp.Started {
				fmt.Fprintf(out, "\nstarted re-synthesis of %d doc(s) in the background — "+
					"re-run without --apply to watch the count fall\n", resp.Pending)
			} else {
				fmt.Fprintln(out, "\nnothing to re-synthesize")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "start the backfill (default is a dry-run report)")
	cmd.Flags().IntVar(&limit, "limit", 0, "cap how many docs the apply pass processes (0 = all)")
	return cmd
}
