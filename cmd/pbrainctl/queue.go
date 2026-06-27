package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/brain/wqueue"
	"github.com/neverprepared/phantom-brain/internal/config"
)

// clientQueueCmd is the operator-facing inspector for the agent-side
// write-ahead queue (issue #61). It opens the queue directly — no
// Lifecycle, no MCP server — so an operator can poke the queue while
// a Claude Code session is or isn't running. Concurrent access is
// safe (SQLite WAL + busy_timeout); the drainer is permitted to make
// progress underneath these commands.
//
// Resolution: env-bound by default via config.LoadAgent(), with
// --profile/--vault flag overrides that let the operator inspect a
// different binding without rewriting the env. --queue-dir is the
// final escape hatch when neither path works (e.g. inspecting a
// stranded queue after a vault rename).
func clientQueueCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "queue",
		Short: "Inspect and drain the agent write-ahead queue",
		Long: `The agent write-ahead queue persists writes that couldn't reach the
daemon synchronously. Items remain in the queue until the drainer
posts them successfully; queued items are deliberately invisible to
brain_recall until the drainer syncs them and the daemon synthesises
them into its online store.

  queue list                 print queued items, newest first
  queue drain-now            attempt one synchronous drain pass
  queue clear --confirm      delete every row + every staging file`,
	}
	c.AddCommand(clientQueueListCmd())
	c.AddCommand(clientQueueDrainNowCmd())
	c.AddCommand(clientQueueClearCmd())
	return c
}

// queueResolveOpts captures the resolution flags shared by every
// queue subcommand.
type queueResolveOpts struct {
	profile  string
	vault    string
	queueDir string
}

func attachQueueResolveFlags(cmd *cobra.Command, opts *queueResolveOpts) {
	cmd.Flags().StringVar(&opts.profile, "profile", "", "Profile name (overrides CL_WORKSPACE_PROFILE)")
	cmd.Flags().StringVar(&opts.vault, "vault", "", "Vault name (overrides CL_BRAIN_VAULT)")
	cmd.Flags().StringVar(&opts.queueDir, "queue-dir", "", "Open the queue at this directory directly (escape hatch)")
}

// resolveQueueDir returns the absolute directory that holds the
// queue's sqlite file. Order: --queue-dir > (--profile + --vault) >
// LoadAgent(). Returns an error with actionable guidance when nothing
// resolves.
func resolveQueueDir(opts queueResolveOpts) (string, error) {
	if strings.TrimSpace(opts.queueDir) != "" {
		return expandHome(opts.queueDir), nil
	}
	if (opts.profile != "") != (opts.vault != "") {
		return "", errors.New("--profile and --vault must be set together")
	}
	if opts.profile != "" && opts.vault != "" {
		if err := validateBindingSegment("profile", opts.profile); err != nil {
			return "", err
		}
		if err := validateBindingSegment("vault", opts.vault); err != nil {
			return "", err
		}
		dh, err := resolveXDGDataHome()
		if err != nil {
			return "", err
		}
		return filepath.Join(dh, "phantom-brain", opts.profile, opts.vault), nil
	}
	agent, err := config.LoadAgent()
	if err != nil {
		return "", fmt.Errorf("%w (or pass --profile/--vault, or --queue-dir)", err)
	}
	return agent.VaultBaseDir(), nil
}

func resolveXDGDataHome() (string, error) {
	if v := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve $HOME: %w", err)
	}
	return filepath.Join(home, ".local", "share"), nil
}

func openResolvedQueue(opts queueResolveOpts) (*wqueue.Queue, string, error) {
	dir, err := resolveQueueDir(opts)
	if err != nil {
		return nil, "", err
	}
	q, err := wqueue.Open(dir)
	if err != nil {
		return nil, dir, err
	}
	return q, dir, nil
}

// openResolvedQueueReadOnly opens an existing queue without creating
// dir/wqueue.sqlite or dir/wqueue-attach/ when neither exists yet.
// Returns wqueue.ErrNotExist when the binding has had no offline
// activity. Used by `queue list` so a passive look never materialises
// a queue on disk.
func openResolvedQueueReadOnly(opts queueResolveOpts) (*wqueue.Queue, string, error) {
	dir, err := resolveQueueDir(opts)
	if err != nil {
		return nil, "", err
	}
	q, err := wqueue.OpenReadOnly(dir)
	if err != nil {
		return nil, dir, err
	}
	return q, dir, nil
}

func clientQueueListCmd() *cobra.Command {
	var (
		opts     queueResolveOpts
		asJSON   bool
		deadOnly bool
		limit    int
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "Print queued items, newest first (--dead for dead-lettered only)",
		Long: `Prints queued items newest-first. Dead-lettered rows (permanent
failures or attempts exhausted) are shown with a DEAD marker and the
dead reason; they are never retried but stay for inspection until
` + "`queue clear`" + `. Use --dead to list only dead-lettered rows.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q, dir, err := openResolvedQueueReadOnly(opts)
			if err != nil {
				if errors.Is(err, wqueue.ErrNotExist) {
					fmt.Fprintln(cmd.OutOrStdout(), "no queue (no offline activity yet)")
					return nil
				}
				return err
			}
			defer q.Close()
			var items []*wqueue.Item
			if deadOnly {
				items, err = q.ListDead(cmd.Context(), limit)
			} else {
				items, err = q.List(cmd.Context(), limit)
			}
			if err != nil {
				return err
			}
			if asJSON {
				return writeQueueJSON(cmd.OutOrStdout(), dir, items)
			}
			return writeQueueTable(cmd.OutOrStdout(), dir, items, time.Now())
		},
	}
	attachQueueResolveFlags(c, &opts)
	c.Flags().BoolVar(&asJSON, "json", false, "Emit JSON instead of a table")
	c.Flags().BoolVar(&deadOnly, "dead", false, "List only dead-lettered items")
	c.Flags().IntVar(&limit, "limit", 0, "Maximum rows to print (0 = no cap)")
	return c
}

func writeQueueTable(w io.Writer, dir string, items []*wqueue.Item, now time.Time) error {
	fmt.Fprintf(w, "# queue: %s\n", filepath.Join(dir, "wqueue.sqlite"))
	if len(items) == 0 {
		fmt.Fprintln(w, "(empty)")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tSHA\tATTEMPTS\tSTATE\tENQUEUED\tLAST_ATTEMPT\tLAST_ERROR")
	for _, it := range items {
		shortSHA := it.SHA
		if len(shortSHA) > 12 {
			shortSHA = shortSHA[:12]
		}
		last := "-"
		if !it.LastAttemptAt.IsZero() {
			last = humanizeAge(now.Sub(it.LastAttemptAt)) + " ago"
		}
		// Dead rows surface their dead_reason; live rows surface last_error.
		msg := it.LastError
		state := "pending"
		if it.Dead {
			state = "dead"
			if it.DeadReason != "" {
				msg = it.DeadReason
			}
		}
		if len(msg) > 60 {
			msg = msg[:57] + "..."
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s ago\t%s\t%s\n",
			it.ID, it.Kind, shortSHA, it.Attempts, state,
			humanizeAge(now.Sub(it.EnqueuedAt)), last, msg,
		)
	}
	return tw.Flush()
}

type queueListJSON struct {
	Dir   string           `json:"dir"`
	Items []queueItemJSON  `json:"items"`
}

type queueItemJSON struct {
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	SHA           string `json:"sha"`
	StagedPath    string `json:"staged_path,omitempty"`
	Attempts      int    `json:"attempts"`
	EnqueuedAt    string `json:"enqueued_at"`
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	Dead          bool   `json:"dead,omitempty"`
	DeadReason    string `json:"dead_reason,omitempty"`
}

func writeQueueJSON(w io.Writer, dir string, items []*wqueue.Item) error {
	out := queueListJSON{Dir: dir, Items: make([]queueItemJSON, 0, len(items))}
	for _, it := range items {
		row := queueItemJSON{
			ID:         it.ID,
			Kind:       string(it.Kind),
			SHA:        it.SHA,
			StagedPath: it.StagedPath,
			Attempts:   it.Attempts,
			EnqueuedAt: it.EnqueuedAt.UTC().Format(time.RFC3339),
			LastError:  it.LastError,
			Dead:       it.Dead,
			DeadReason: it.DeadReason,
		}
		if !it.LastAttemptAt.IsZero() {
			row.LastAttemptAt = it.LastAttemptAt.UTC().Format(time.RFC3339)
		}
		out.Items = append(out.Items, row)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func humanizeAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func clientQueueDrainNowCmd() *cobra.Command {
	var opts queueResolveOpts
	c := &cobra.Command{
		Use:   "drain-now",
		Short: "Attempt one synchronous drain pass against the daemon",
		Long: `Walks every eligible queued item once and POSTs it to the daemon.
Items that succeed are deleted from the queue; items that fail are
left in place with their attempt count bumped (the background
drainer in the live agent will retry on its own backoff schedule).

Exits non-zero when any item failed.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q, dir, err := openResolvedQueue(opts)
			if err != nil {
				return err
			}
			defer q.Close()

			// drain-now needs a daemon client. We require the agent
			// contract env vars for this — --profile/--vault alone
			// don't carry the bearer token.
			agent, err := config.LoadAgent()
			if err != nil {
				return fmt.Errorf("drain-now requires the agent contract env vars (CL_BRAIN_API, CL_BRAIN_API_TOKEN, CL_WORKSPACE_PROFILE, CL_BRAIN_VAULT): %w", err)
			}
			client, err := brain.NewClient(brain.ClientOpts{
				BaseURL: agent.API,
				Token:   agent.Token,
				Timeout: 5 * time.Minute,
			})
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			sent, failed, lastErr := drainOnce(ctx, q, client, 256)
			fmt.Fprintf(cmd.OutOrStdout(), "# queue: %s\nsent=%d failed=%d\n",
				filepath.Join(dir, "wqueue.sqlite"), sent, failed)
			if failed > 0 && lastErr != nil {
				// Distinguish "daemon's down, retry later" (exit 0) from
				// a genuine internal error like a corrupted DB row or
				// missing staging file (exit 1).
				if errors.Is(lastErr, brain.ErrDaemonUnreachable) {
					depth, _ := q.Depth(ctx)
					fmt.Fprintf(cmd.OutOrStdout(), "daemon unreachable, %d items still pending\n", depth)
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "last_error: %s\n", lastErr)
				return fmt.Errorf("drain-now: %d item(s) failed", failed)
			}
			return nil
		},
	}
	attachQueueResolveFlags(c, &opts)
	return c
}

// drainOnce attempts every eligible row once. Returns (sent, failed,
// lastErr). lastErr is the most recent failure (informational only —
// non-fatal items are kept in the queue and retried later).
func drainOnce(ctx context.Context, q *wqueue.Queue, client *brain.Client, limit int) (int, int, error) {
	items, err := q.NextEligible(ctx, time.Now(), limit)
	if err != nil {
		return 0, 0, err
	}
	var sent, failed int
	var lastErr error
	for _, it := range items {
		if err := dispatch(ctx, client, it); err != nil {
			_ = q.MarkAttempt(ctx, it.ID, time.Now(), err)
			failed++
			lastErr = err
			continue
		}
		if err := q.Delete(ctx, it.ID); err != nil {
			lastErr = err
			failed++
			continue
		}
		sent++
	}
	return sent, failed, lastErr
}

// dispatch routes one queued item to the correct daemon endpoint.
// Mirrors what the live drainer (Stream B) will do; duplicated here
// so the operator CLI can run without depending on goroutine wiring.
func dispatch(ctx context.Context, client *brain.Client, it *wqueue.Item) error {
	switch it.Kind {
	case wqueue.KindPerceive:
		var req brain.PerceiveRequest
		if err := json.Unmarshal(it.PayloadJSON, &req); err != nil {
			return fmt.Errorf("decode perceive payload: %w", err)
		}
		_, err := client.Perceive(ctx, req)
		return err
	case wqueue.KindLearn, wqueue.KindTaskPromote:
		var req brain.LearnRequest
		if err := json.Unmarshal(it.PayloadJSON, &req); err != nil {
			return fmt.Errorf("decode learn payload: %w", err)
		}
		_, err := client.Learn(ctx, req)
		return err
	case wqueue.KindAttach:
		var req brain.AttachRequest
		if err := json.Unmarshal(it.PayloadJSON, &req); err != nil {
			return fmt.Errorf("decode attach payload: %w", err)
		}
		if it.StagedPath != "" {
			raw, err := os.ReadFile(it.StagedPath)
			if err != nil {
				return fmt.Errorf("read staged attach bytes: %w", err)
			}
			req.BytesB64 = base64.StdEncoding.EncodeToString(raw)
		}
		_, err := client.Attach(ctx, req)
		return err
	case wqueue.KindTrace:
		var req brain.TraceRequest
		if err := json.Unmarshal(it.PayloadJSON, &req); err != nil {
			return fmt.Errorf("decode trace payload: %w", err)
		}
		return client.Trace(ctx, req)
	default:
		return fmt.Errorf("unknown queue kind %q", it.Kind)
	}
}

func clientQueueClearCmd() *cobra.Command {
	var (
		opts    queueResolveOpts
		confirm bool
	)
	c := &cobra.Command{
		Use:   "clear",
		Short: "Delete every queued item and every staging file",
		Long: `Operator escape hatch. Removes every row from the queue's sqlite
file and every file in the staging directory. Requires --confirm —
without it the command prints the deletion that would happen and
exits non-zero.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q, dir, err := openResolvedQueue(opts)
			if err != nil {
				return err
			}
			defer q.Close()
			depth, err := q.Depth(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "# queue: %s\n", filepath.Join(dir, "wqueue.sqlite"))
			if !confirm {
				fmt.Fprintf(out, "would delete %d row(s); rerun with --confirm to proceed\n", depth)
				return fmt.Errorf("clear requires --confirm")
			}
			n, err := q.Clear(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "cleared %d row(s)\n", n)
			return nil
		},
	}
	attachQueueResolveFlags(c, &opts)
	c.Flags().BoolVar(&confirm, "confirm", false, "Required to actually delete; without it the command is a no-op")
	return c
}
