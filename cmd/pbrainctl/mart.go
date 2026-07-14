package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/config"
	"github.com/neverprepared/phantom-brain/internal/mart"
)

// martCmd groups the client-side mart workflow: define read-only markdown
// projections of the brain (marts) and materialize them into Obsidian dirs.
// A mart is an integration living in the pbrainctl binary but its own package
// (internal/mart), reading the brain ONLY over the public HTTP API.
//
//	mart add <name>     define/overwrite a mart (writes <configDir>/marts/<name>.toml)
//	mart list           list configured marts
//	mart remove <name>  delete a mart's definition (leaves rendered output)
//	mart build <name>   materialize a mart into its dest directory
func martCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "mart",
		Short: "Manage read-only markdown projections (marts) of the brain",
	}
	c.PersistentFlags().String("config-dir", "", "override PHANTOM_BRAIN_CONFIG_DIR (mart specs live under <config-dir>/marts)")
	c.AddCommand(martAddCmd(), martListCmd(), martRemoveCmd(), martBuildCmd(), martSyncCmd(), martDaemonCmd())
	return c
}

// resolveMartForRun loads a mart spec, asserts it matches the agent-env binding
// (the daemon resolves the tenant from the bearer token, so a spec/env mismatch
// would silently project the WRONG tenant), and returns the spec + registry +
// a daemon client. Shared by `mart build` and `mart sync`.
func resolveMartForRun(cmd *cobra.Command, name string) (mart.Spec, *mart.Registry, *brain.Client, error) {
	reg := mart.OpenRegistry(resolveConfigDir(cmd))
	spec, err := reg.Load(name)
	if err != nil {
		return mart.Spec{}, nil, nil, err
	}
	agent, err := config.LoadAgent()
	if err != nil {
		return mart.Spec{}, nil, nil, fmt.Errorf("requires the agent contract env vars (CL_BRAIN_API, CL_BRAIN_API_TOKEN, CL_WORKSPACE_PROFILE, CL_BRAIN_VAULT): %w", err)
	}
	if agent.Profile != spec.Profile || agent.Vault != spec.Vault {
		return mart.Spec{}, nil, nil, fmt.Errorf("mart %q targets %s/%s but the agent env is bound to %s/%s — set CL_WORKSPACE_PROFILE/CL_BRAIN_VAULT to match, or rebuild the mart spec",
			spec.Name, spec.Profile, spec.Vault, agent.Profile, agent.Vault)
	}
	client, err := newBrainClientFromEnv()
	if err != nil {
		return mart.Spec{}, nil, nil, err
	}
	return spec, reg, client, nil
}

func martSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync <name>",
		Short: "Incrementally refresh a mart from the change feed (only what changed)",
		Long: `Applies records changed since the mart's saved cursor into its dest —
upserting notes and re-downloading changed attachments, without a full wipe.
The one-shot command the launchd daemon runs on a schedule; also runnable by
hand. A first run (no cursor) reads from the beginning.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, reg, client, err := resolveMartForRun(cmd, args[0])
			if err != nil {
				return err
			}
			cur, err := reg.LoadCursor(spec.Name)
			if err != nil {
				return err
			}
			res, next, err := mart.Sync(cmd.Context(), spec, client, cur)
			if err != nil {
				return err
			}
			if err := reg.SaveCursor(spec.Name, next); err != nil {
				return fmt.Errorf("synced but failed to persist cursor: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "synced mart %q: %d changed record(s), %d attachment(s) → %s\n",
				spec.Name, res.RecordsWritten, res.AttachmentsWritten, res.DestPath)
			if res.AttachmentsSkipped > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "warning: %d attachment(s) could not be materialized (see [!warning] callouts)\n", res.AttachmentsSkipped)
			}
			return nil
		},
	}
}

func martAddCmd() *cobra.Command {
	var (
		profile         string
		vault           string
		dest            string
		tags            []string
		kinds           []string
		sources         []string
		topic           string
		reliability     []string
		ephemeral       bool
		skipAttachments bool
	)
	c := &cobra.Command{
		Use:   "add <name>",
		Short: "Define (or overwrite) a mart",
		Long: `Writes a mart spec to <config-dir>/marts/<name>.toml. profile/vault
default to $CL_WORKSPACE_PROFILE/$CL_BRAIN_VAULT. dest must be an absolute
directory the mart can own (a dedicated subdir, e.g. .../vaults/taxes/_mart) —
build refuses to wipe a non-empty directory it did not create.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if profile == "" {
				profile = strings.TrimSpace(os.Getenv("CL_WORKSPACE_PROFILE"))
			}
			if vault == "" {
				vault = strings.TrimSpace(os.Getenv("CL_BRAIN_VAULT"))
			}
			if profile == "" {
				return fmt.Errorf("--profile is required (or set $CL_WORKSPACE_PROFILE)")
			}
			if vault == "" {
				return fmt.Errorf("--vault is required (or set $CL_BRAIN_VAULT)")
			}
			spec := mart.Spec{
				Name:            args[0],
				Profile:         profile,
				Vault:           vault,
				Dest:            expandHome(dest),
				Ephemeral:       ephemeral,
				SkipAttachments: skipAttachments,
				Filters: mart.Filters{
					Kinds:       kinds,
					Tags:        tags,
					Sources:     sources,
					Topic:       topic,
					Reliability: reliability,
				},
			}
			reg := mart.OpenRegistry(resolveConfigDir(cmd))
			if err := reg.Save(spec); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "saved mart %q → %s\n", spec.Name, reg.Path(spec.Name))
			fmt.Fprintf(cmd.OutOrStdout(), "build it with: pbrainctl client mart build %s\n", spec.Name)
			return nil
		},
	}
	c.Flags().StringVar(&profile, "profile", "", "profile binding (default $CL_WORKSPACE_PROFILE)")
	c.Flags().StringVar(&vault, "vault", "", "vault binding (default $CL_BRAIN_VAULT)")
	c.Flags().StringVar(&dest, "dest", "", "absolute output directory the mart owns (required)")
	c.Flags().StringSliceVar(&tags, "tag", nil, "only records carrying ANY of these tags (repeatable)")
	c.Flags().StringSliceVar(&kinds, "kind", nil, "only records of these kinds (repeatable)")
	c.Flags().StringSliceVar(&sources, "source", nil, "only records with ANY of these source[] values (repeatable)")
	c.Flags().StringVar(&topic, "topic", "", "only records with this topic")
	c.Flags().StringSliceVar(&reliability, "reliability", nil, "only records with these reliability values (repeatable)")
	c.Flags().BoolVar(&ephemeral, "ephemeral", false, "clean-rebuild the dest each build (wipe + re-render)")
	c.Flags().BoolVar(&skipAttachments, "skip-attachments", false, "project attachment metadata/stub text only; do NOT download MinIO blobs into the mart")
	_ = c.MarkFlagRequired("dest")
	return c
}

func martListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured marts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg := mart.OpenRegistry(resolveConfigDir(cmd))
			specs, err := reg.List()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(specs) == 0 {
				fmt.Fprintln(out, "no marts configured — define one with: pbrainctl client mart add <name> --dest <dir>")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tPROFILE/VAULT\tEPHEMERAL\tDEST\tFILTERS")
			for _, s := range specs {
				fmt.Fprintf(tw, "%s\t%s/%s\t%t\t%s\t%s\n",
					s.Name, s.Profile, s.Vault, s.Ephemeral, s.Dest, describeFilters(s.Filters))
			}
			_ = tw.Flush()
			return nil
		},
	}
}

func martRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Delete a mart's definition (leaves its rendered output on disk)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg := mart.OpenRegistry(resolveConfigDir(cmd))
			spec, loadErr := reg.Load(args[0])
			if err := reg.Remove(args[0]); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "removed mart %q\n", args[0])
			if loadErr == nil {
				fmt.Fprintf(out, "note: rendered output left in place at %s (delete it by hand if unwanted)\n", spec.Dest)
			}
			return nil
		},
	}
}

func martBuildCmd() *cobra.Command {
	var pageSize int
	c := &cobra.Command{
		Use:   "build <name>",
		Short: "Materialize a mart into its dest directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, _, client, err := resolveMartForRun(cmd, args[0])
			if err != nil {
				return err
			}
			res, err := mart.Build(cmd.Context(), spec, mart.ClientSource{
				Client:   client,
				Filters:  spec.Filters,
				PageSize: pageSize,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "built mart %q: %d record(s), %d attachment(s) → %s\n",
				spec.Name, res.RecordsWritten, res.AttachmentsWritten, res.DestPath)
			if res.AttachmentsSkipped > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "warning: %d attachment(s) could not be materialized (see [!warning] callouts in the notes)\n",
					res.AttachmentsSkipped)
			}
			return nil
		},
	}
	c.Flags().IntVar(&pageSize, "page-size", 100, "records fetched per daemon request")
	return c
}

// describeFilters renders a compact one-line summary of a mart's filters for
// `mart list`.
func describeFilters(f mart.Filters) string {
	var parts []string
	if len(f.Kinds) > 0 {
		parts = append(parts, "kind="+strings.Join(f.Kinds, ","))
	}
	if len(f.Tags) > 0 {
		parts = append(parts, "tag="+strings.Join(f.Tags, ","))
	}
	if len(f.Sources) > 0 {
		parts = append(parts, "source="+strings.Join(f.Sources, ","))
	}
	if f.Topic != "" {
		parts = append(parts, "topic="+f.Topic)
	}
	if len(f.Reliability) > 0 {
		parts = append(parts, "reliability="+strings.Join(f.Reliability, ","))
	}
	if len(parts) == 0 {
		return "(all)"
	}
	return strings.Join(parts, " ")
}
