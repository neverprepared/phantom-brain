package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/neverprepared/phantom-brain/internal/brain"
	"github.com/neverprepared/phantom-brain/internal/mart"
)

// The mart_* tools let an agent trigger a mart — a read-only projection of
// long-term memory into an Obsidian-shaped folder (#141). NAMED-SPEC-ONLY: the
// agent picks a mart an operator pre-configured (`pbrainctl client mart add`);
// it never supplies a destination path or filters, so it cannot make the server
// write files anywhere or project an arbitrary tenant. Results are summaries,
// never the materialized content.

func martListTool() mcp.Tool {
	return mcp.NewTool("mart_list",
		mcp.WithDescription(
			`List the configured marts — read-only projections of long-term memory into Obsidian `+
				`folders. Shows each mart's name, tenant (profile/vault), destination, filters, and `+
				`whether its credentials resolve (so you know which can be built or synced). Call this `+
				`first to pick a name for mart_build / mart_sync.`,
		),
	)
}

func (s *Server) handleMartList(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	specs, err := mart.OpenRegistry(s.deps.ConfigDir).List()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("mart_list: %v", err)), nil
	}
	if len(specs) == 0 {
		return mcp.NewToolResultText("No marts configured. An operator defines one with `pbrainctl client mart add`."), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d configured mart(s):\n\n", len(specs))
	for _, spec := range specs {
		status := "runnable"
		if _, _, cerr := mart.ResolveCredential(s.deps.ConfigDir, spec, s.deps.AgentEnv); cerr != nil {
			status = "NO CREDS (operator must run `mart cred add`)"
		}
		fmt.Fprintf(&b, "- %s (%s/%s) [%s]\n  dest: %s\n  filters: %s\n",
			spec.Name, spec.Profile, spec.Vault, status, spec.Dest, spec.Filters.Summary())
	}
	return mcp.NewToolResultText(strings.TrimSpace(b.String())), nil
}

func martBuildTool() mcp.Tool {
	return mcp.NewTool("mart_build",
		mcp.WithDescription(
			`Full-rebuild a configured mart into its destination folder: re-render every matching `+
				`record and re-download its attachments. Pass "name" for one mart, or "all":true for `+
				`every mart. Returns a summary (record/attachment counts + path), not the content. `+
				`Marts are pre-configured by an operator — you cannot set the destination or filters. `+
				`Prefer mart_sync for a routine refresh; build is the full/first materialization.`,
		),
		mcp.WithString("name", mcp.Description("Name of the mart to build (see mart_list). Omit and set all:true to build every mart.")),
		mcp.WithBoolean("all", mcp.Description("Build every configured mart across profiles.")),
	)
}

func (s *Server) handleMartBuild(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return s.runMartOp(ctx, req, "build", func(ctx context.Context, spec mart.Spec, _ *mart.Registry, client *brain.Client) (string, error) {
		res, err := mart.Build(ctx, spec, mart.ClientSource{Client: client, Filters: spec.Filters})
		if err != nil {
			return "", err
		}
		return res.Summary("built", spec.Name), nil
	})
}

func martSyncTool() mcp.Tool {
	return mcp.NewTool("mart_sync",
		mcp.WithDescription(
			`Incrementally refresh a configured mart — apply only the records that changed since its `+
				`last run (upsert notes, re-download changed attachments; no full wipe). Pass "name" `+
				`for one mart, or "all":true for every mart. Returns a summary, not the content. This `+
				`is the cheap, routine refresh; use mart_build for a full re-materialization.`,
		),
		mcp.WithString("name", mcp.Description("Name of the mart to sync (see mart_list). Omit and set all:true to sync every mart.")),
		mcp.WithBoolean("all", mcp.Description("Sync every configured mart across profiles.")),
	)
}

func (s *Server) handleMartSync(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return s.runMartOp(ctx, req, "sync", func(ctx context.Context, spec mart.Spec, reg *mart.Registry, client *brain.Client) (string, error) {
		cur, err := reg.LoadCursor(spec.Name)
		if err != nil {
			return "", err
		}
		res, next, err := mart.Sync(ctx, spec, client, cur)
		if err != nil {
			return "", err
		}
		if err := reg.SaveCursor(spec.Name, next); err != nil {
			return "", fmt.Errorf("synced but cursor save failed: %w", err)
		}
		return res.Summary("synced", spec.Name), nil
	})
}

// runMartOp resolves each target spec's own credentials, runs op, and
// aggregates a text summary — continue-on-error so one profile's problem
// doesn't block the rest. A partial failure is still a text result (the agent
// sees which succeeded), not a tool error.
func (s *Server) runMartOp(ctx context.Context, req mcp.CallToolRequest, verb string,
	op func(context.Context, mart.Spec, *mart.Registry, *brain.Client) (string, error)) (*mcp.CallToolResult, error) {

	reg := mart.OpenRegistry(s.deps.ConfigDir)
	name := strings.TrimSpace(req.GetString("name", ""))
	all := req.GetBool("all", false)
	if all && name != "" {
		return mcp.NewToolResultError(fmt.Sprintf("mart_%s: pass either name or all, not both", verb)), nil
	}
	if !all && name == "" {
		return mcp.NewToolResultError(fmt.Sprintf("mart_%s: pass a mart name (see mart_list) or all:true", verb)), nil
	}

	var specs []mart.Spec
	if all {
		list, err := reg.List()
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("mart_%s: %v", verb, err)), nil
		}
		specs = list
	} else {
		spec, err := reg.Load(name)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("mart_%s: %v", verb, err)), nil
		}
		specs = []mart.Spec{spec}
	}
	if len(specs) == 0 {
		return mcp.NewToolResultText("No marts configured. An operator defines one with `pbrainctl client mart add`."), nil
	}

	var b strings.Builder
	failed := 0
	for _, spec := range specs {
		api, token, cerr := mart.ResolveCredential(s.deps.ConfigDir, spec, s.deps.AgentEnv)
		if cerr != nil {
			failed++
			fmt.Fprintf(&b, "- %s: SKIP (%v)\n", spec.Name, cerr)
			continue
		}
		client, cerr := brain.NewClient(brain.ClientOpts{BaseURL: api, Token: token, Timeout: 30 * time.Second})
		if cerr != nil {
			failed++
			fmt.Fprintf(&b, "- %s: ERROR %v\n", spec.Name, cerr)
			continue
		}
		line, oerr := op(ctx, spec, reg, client)
		if oerr != nil {
			failed++
			fmt.Fprintf(&b, "- %s: ERROR %v\n", spec.Name, oerr)
			continue
		}
		fmt.Fprintf(&b, "- %s\n", line)
	}
	if failed > 0 {
		fmt.Fprintf(&b, "\n%d of %d mart(s) failed.", failed, len(specs))
	}
	return mcp.NewToolResultText(strings.TrimSpace(b.String())), nil
}
