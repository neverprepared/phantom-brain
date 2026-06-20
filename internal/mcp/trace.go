package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// traceTool defines the brain_trace MCP tool schema.
//
// brain_trace queries the synthesis audit log at Wiki/_log.md. The
// synthesizer (Phase 2) appends an entry every time a Raw item is
// promoted to Wiki/; brain_trace is the agent's window into "what
// happened recently?" and "when did this entity first appear?"
//
// Phase 0 surface is the last-N-entries view. Entry-by-topic and
// since-timestamp filters arrive when the synthesizer ships and gives
// us a richer log shape to filter against.
func traceTool() mcp.Tool {
	return mcp.NewTool("brain_trace",
		mcp.WithDescription(
			`Read the brain's synthesis audit log (Wiki/_log.md). Returns the most `+
				`recent entries, optionally filtered by substring match. Use to investigate `+
				`when a fact was learned, what was promoted to the Wiki, or to debug a `+
				`recall miss.`,
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of entries to return (default 20, max 200). Entries are returned newest-first."),
		),
		mcp.WithString("contains",
			mcp.Description("Optional substring filter. Case-insensitive. Only entries containing this string are returned."),
		),
	)
}

const (
	defaultTraceLimit = 20
	maxTraceLimit     = 200
)

func (s *Server) handleTrace(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := defaultTraceLimit
	if got, err := req.RequireFloat("limit"); err == nil {
		limit = int(got)
	}
	if limit <= 0 {
		limit = defaultTraceLimit
	}
	if limit > maxTraceLimit {
		limit = maxTraceLimit
	}

	contains, _ := req.RequireString("contains")
	contains = strings.TrimSpace(contains)

	logPath := filepath.Join(s.deps.VaultDir, "Wiki", "_log.md")
	raw, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return mcp.NewToolResultText("Synthesis log is empty (Wiki/_log.md does not exist yet)."), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("read log: %v", err)), nil
	}

	entries := parseLogEntries(string(raw))
	if contains != "" {
		entries = filterEntries(entries, contains)
	}

	// Most recent first. Synthesizer appends in time order, so the
	// last entry in the file is the newest. Slice the tail.
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}

	if len(entries) == 0 {
		return mcp.NewToolResultText("No log entries match the filter."), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d entr(y/ies):\n\n", len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		b.WriteString(entries[i])
		b.WriteString("\n---\n")
	}
	return mcp.NewToolResultText(b.String()), nil
}

// parseLogEntries splits a Wiki/_log.md into individual entries.
//
// Entry delimiter heuristic: an entry starts at a line beginning with
// "## " (markdown level-2 heading) and continues until the next "## "
// or end of file. This mirrors the synthesizer's append convention
// (Phase 2 work; for now we tolerate any markdown shape).
//
// If the log contains no "## " headings, the entire file is returned
// as one entry. This makes the read robust against logs from earlier
// runtime versions.
func parseLogEntries(s string) []string {
	if !strings.Contains(s, "\n## ") && !strings.HasPrefix(s, "## ") {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return nil
		}
		return []string{trimmed}
	}
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "## ") {
			if cur.Len() > 0 {
				out = append(out, strings.TrimRight(cur.String(), "\n"))
				cur.Reset()
			}
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	if cur.Len() > 0 {
		trimmed := strings.TrimRight(cur.String(), "\n")
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func filterEntries(entries []string, substr string) []string {
	lower := strings.ToLower(substr)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e), lower) {
			out = append(out, e)
		}
	}
	return out
}
