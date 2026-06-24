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
		Long: `Deletes the summary doc at <sha> and triggers a snapshot rebuild on
the daemon. The doc stays visible to brain_recall until a new snapshot
publishes and a fresh brain births.`,
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
				"forgot %s (forgotten=%t)\nstays visible to brain_recall until a new snapshot publishes\n",
				resp.SHA, resp.Forgotten)
			return nil
		},
	}
}
