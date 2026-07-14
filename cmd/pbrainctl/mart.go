package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

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
	c.AddCommand(martAddCmd(), martListCmd(), martRemoveCmd(), martBuildCmd(), martSyncCmd(), martDaemonCmd(), martCredCmd())
	return c
}

// martCredCmd manages the workstation credential store — per-(profile, vault)
// daemon URL + token, so marts across profiles resolve their own creds instead
// of relying on ambient env.
func martCredCmd() *cobra.Command {
	c := &cobra.Command{Use: "cred", Short: "Manage per-profile daemon credentials for marts"}
	c.AddCommand(martCredAddCmd(), martCredListCmd(), martCredRemoveCmd())
	return c
}

func martCredAddCmd() *cobra.Command {
	var profile, vault, api, token string
	c := &cobra.Command{
		Use:   "add",
		Short: "Store the daemon URL + token for a (profile, vault)",
		Long: `Adds (or replaces) a binding's credentials in the store. Each flag
defaults from the matching agent env var, so the usual flow is: export a
profile's CL_BRAIN_* and run 'mart cred add'; repeat per profile.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			get := func(v, env string) string {
				if v != "" {
					return v
				}
				return strings.TrimSpace(os.Getenv(env))
			}
			cred := mart.Credential{
				Profile: get(profile, "CL_WORKSPACE_PROFILE"),
				Vault:   get(vault, "CL_BRAIN_VAULT"),
				API:     get(api, "CL_BRAIN_API"),
				Token:   get(token, "CL_BRAIN_API_TOKEN"),
			}
			for k, v := range map[string]string{"profile": cred.Profile, "vault": cred.Vault, "api": cred.API, "token": cred.Token} {
				if v == "" {
					return fmt.Errorf("%s is required (pass --%s or export the matching CL_BRAIN_* var)", k, k)
				}
			}
			configDir := resolveConfigDir(cmd)
			store, err := mart.LoadCredentials(configDir)
			if err != nil {
				return err
			}
			store.Set(cred)
			if err := mart.SaveCredentials(configDir, store); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stored credentials for %s/%s → %s\n", cred.Profile, cred.Vault, mart.CredentialsPath(configDir))
			return nil
		},
	}
	c.Flags().StringVar(&profile, "profile", "", "profile (default $CL_WORKSPACE_PROFILE)")
	c.Flags().StringVar(&vault, "vault", "", "vault (default $CL_BRAIN_VAULT)")
	c.Flags().StringVar(&api, "api", "", "daemon URL (default $CL_BRAIN_API)")
	c.Flags().StringVar(&token, "token", "", "bearer token (default $CL_BRAIN_API_TOKEN)")
	return c
}

func martCredListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List stored credentials (tokens redacted)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := mart.LoadCredentials(resolveConfigDir(cmd))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(store.Bindings) == 0 {
				fmt.Fprintln(out, "no stored credentials — add one with: pbrainctl client mart cred add")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "PROFILE/VAULT\tAPI\tTOKEN")
			for _, b := range store.Bindings {
				fmt.Fprintf(tw, "%s/%s\t%s\t%s\n", b.Profile, b.Vault, b.API, redactToken(b.Token))
			}
			_ = tw.Flush()
			return nil
		},
	}
}

func martCredRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <profile>/<vault>",
		Short: "Delete a binding's stored credentials",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, vault, ok := strings.Cut(args[0], "/")
			if !ok || profile == "" || vault == "" {
				return fmt.Errorf("argument must be <profile>/<vault>, got %q", args[0])
			}
			configDir := resolveConfigDir(cmd)
			store, err := mart.LoadCredentials(configDir)
			if err != nil {
				return err
			}
			if !store.Remove(profile, vault) {
				return fmt.Errorf("no stored credentials for %s/%s", profile, vault)
			}
			if err := mart.SaveCredentials(configDir, store); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed credentials for %s/%s\n", profile, vault)
			return nil
		},
	}
}

// redactToken shows a short fingerprint of a bearer token without exposing it.
func redactToken(t string) string {
	if len(t) <= 8 {
		return "********"
	}
	return t[:4] + "…" + t[len(t)-4:]
}

// resolveMartCreds resolves the daemon API URL + bearer token for a mart from
// its OWN (profile, vault), so marts across multiple profiles work without
// env-juggling. Order: (1) the workstation credentials store; (2) ambient env,
// but ONLY when it is bound to the same (profile, vault) — a mismatched env
// would project the wrong tenant, so it is ignored rather than trusted;
// (3) a clear error. The daemon still enforces the tenant via the token; this
// just picks the right token.
func resolveMartCreds(cmd *cobra.Command, spec mart.Spec) (api, token string, err error) {
	var env mart.AgentEnv
	if agent, aerr := config.LoadAgent(); aerr == nil {
		env = mart.AgentEnv{API: agent.API, Token: agent.Token, Profile: agent.Profile, Vault: agent.Vault}
	}
	return mart.ResolveCredential(resolveConfigDir(cmd), spec, env)
}

// resolveMartForRun loads a mart spec and builds a daemon client with the
// spec's own credentials (store → matching env). Shared by `mart build`/`sync`.
func resolveMartForRun(cmd *cobra.Command, name string) (mart.Spec, *mart.Registry, *brain.Client, error) {
	reg := mart.OpenRegistry(resolveConfigDir(cmd))
	spec, err := reg.Load(name)
	if err != nil {
		return mart.Spec{}, nil, nil, err
	}
	api, token, err := resolveMartCreds(cmd, spec)
	if err != nil {
		return mart.Spec{}, nil, nil, err
	}
	client, err := brain.NewClient(brain.ClientOpts{BaseURL: api, Token: token, Timeout: 30 * time.Second})
	if err != nil {
		return mart.Spec{}, nil, nil, err
	}
	return spec, reg, client, nil
}

func martSyncCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "sync [<name>]",
		Short: "Incrementally refresh a mart (or every mart with --all) from the change feed",
		Long: `Applies records changed since the mart's saved cursor into its dest —
upserting notes and re-downloading changed attachments, without a full wipe.
The one-shot command the launchd daemon runs on a schedule; also runnable by
hand. A first run (no cursor) reads from the beginning. --all syncs every
configured mart across profiles, each with its own credentials.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkAllArg(all, args); err != nil {
				return err
			}
			if all {
				return forEachMart(cmd, func(spec mart.Spec, reg *mart.Registry, client *brain.Client) error {
					return syncOne(spec, reg, client, cmd)
				})
			}
			spec, reg, client, err := resolveMartForRun(cmd, args[0])
			if err != nil {
				return err
			}
			return syncOne(spec, reg, client, cmd)
		},
	}
	c.Flags().BoolVar(&all, "all", false, "sync every configured mart (across profiles)")
	return c
}

// syncOne runs one incremental sync and persists the advanced cursor. The
// signature matches forEachMart's callback; the single-mart path adapts to it.
func syncOne(spec mart.Spec, reg *mart.Registry, client *brain.Client, cmd *cobra.Command) error {
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
					s.Name, s.Profile, s.Vault, s.Ephemeral, s.Dest, s.Filters.Summary())
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
	var all bool
	c := &cobra.Command{
		Use:   "build [<name>]",
		Short: "Materialize a mart (or every mart with --all) into its dest directory",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkAllArg(all, args); err != nil {
				return err
			}
			if all {
				return forEachMart(cmd, func(spec mart.Spec, _ *mart.Registry, client *brain.Client) error {
					return buildOne(cmd, spec, client, pageSize)
				})
			}
			spec, _, client, err := resolveMartForRun(cmd, args[0])
			if err != nil {
				return err
			}
			return buildOne(cmd, spec, client, pageSize)
		},
	}
	c.Flags().IntVar(&pageSize, "page-size", 100, "records fetched per daemon request")
	c.Flags().BoolVar(&all, "all", false, "build every configured mart (across profiles)")
	return c
}

// buildOne materializes a single mart and prints its result.
func buildOne(cmd *cobra.Command, spec mart.Spec, client *brain.Client, pageSize int) error {
	res, err := mart.Build(cmd.Context(), spec, mart.ClientSource{Client: client, Filters: spec.Filters, PageSize: pageSize})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "built mart %q: %d record(s), %d attachment(s) → %s\n",
		spec.Name, res.RecordsWritten, res.AttachmentsWritten, res.DestPath)
	if res.AttachmentsSkipped > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "warning: %d attachment(s) could not be materialized (see [!warning] callouts)\n", res.AttachmentsSkipped)
	}
	return nil
}

// checkAllArg enforces "exactly one of <name> or --all".
func checkAllArg(all bool, args []string) error {
	if all && len(args) > 0 {
		return fmt.Errorf("pass either a mart name or --all, not both")
	}
	if !all && len(args) == 0 {
		return fmt.Errorf("requires a mart name (or --all for every mart)")
	}
	return nil
}

// forEachMart resolves each configured mart's own credentials and runs `do`,
// continuing past failures so one profile's problem doesn't block the rest.
// Returns an error if any mart failed.
func forEachMart(cmd *cobra.Command, do func(mart.Spec, *mart.Registry, *brain.Client) error) error {
	reg := mart.OpenRegistry(resolveConfigDir(cmd))
	specs, err := reg.List()
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no marts configured")
		return nil
	}
	failed := 0
	for _, spec := range specs {
		api, token, cerr := resolveMartCreds(cmd, spec)
		if cerr != nil {
			failed++
			fmt.Fprintf(cmd.ErrOrStderr(), "SKIP %s (%s/%s): %v\n", spec.Name, spec.Profile, spec.Vault, cerr)
			continue
		}
		client, cerr := brain.NewClient(brain.ClientOpts{BaseURL: api, Token: token, Timeout: 30 * time.Second})
		if cerr != nil {
			failed++
			fmt.Fprintf(cmd.ErrOrStderr(), "ERROR %s: %v\n", spec.Name, cerr)
			continue
		}
		if derr := do(spec, reg, client); derr != nil {
			failed++
			fmt.Fprintf(cmd.ErrOrStderr(), "ERROR %s: %v\n", spec.Name, derr)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d mart(s) failed", failed, len(specs))
	}
	return nil
}
