package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Reliability is the gate's verdict on how much trust the synthesiser
// should place in a source. String constants matching the TS port so
// summary frontmatter renders identically.
type Reliability string

const (
	ReliabilityHigh      Reliability = "high"
	ReliabilityMedium    Reliability = "medium"
	ReliabilityLow       Reliability = "low"
	ReliabilityContested Reliability = "contested"
)

// Category is required when reliability is low/contested. Tells the
// operator what kind of failure mode pulled the source down.
type Category string

const (
	CategorySource        Category = "source"
	CategoryFormal        Category = "formal"
	CategoryInformal      Category = "informal"
	CategoryPhilosophical Category = "philosophical"
)

// Topic is the subject-matter bucket used by brain_recall's topic
// pre-filter. The closed set matches src/gate/evaluate.ts.
type Topic string

const (
	TopicAgents         Topic = "agents"
	TopicMemory         Topic = "memory"
	TopicGovernance     Topic = "governance"
	TopicTools          Topic = "tools"
	TopicTraining       Topic = "training"
	TopicInfrastructure Topic = "infrastructure"
	TopicKnowledge      Topic = "knowledge"
	TopicMultiAgent     Topic = "multiagent"
	TopicGeneral        Topic = "general"
)

// GateVerdict is the gate's structured output. Reliability is always
// set; the other fields land per the TS rules (Category required when
// reliability is low/contested; Topic always present in v2 verdicts).
type GateVerdict struct {
	Reliability Reliability `json:"reliability"`
	Category    Category    `json:"category,omitempty"`
	Topic       Topic       `json:"topic,omitempty"`
	Reason      string      `json:"reason"`
}

// GateOpts collects the gate's inputs. SourceType is "curated" or
// "gathered" — curated bypasses the LLM call entirely per Phase 1
// rules. Format describes the markup so the LLM knows what to expect.
type GateOpts struct {
	Title      string
	SourceURL  string
	Content    string
	Format     string // "markdown" | "html" | ...
	SourceType string // "curated" | "gathered"

	// Model overrides the gate model. Empty string falls back to the
	// CLI's default (which itself defaults to the model baked into
	// the operator's `claude` config). The TS port defaults to
	// claude-haiku-4-5-20251001 — we let the CLI decide.
	Model string

	// Timeout overrides the gate timeout. Zero means use the default
	// (30s, matching TS).
	Timeout time.Duration
}

const (
	defaultGateTimeout      = 30 * time.Second
	defaultSummarizeTimeout = 45 * time.Second
	contentPreviewLimit     = 8000 // chars sent to the gate / summarize
)

// RunGate returns a GateVerdict. NEVER throws — every failure path
// degrades to {Reliability: medium, Reason: "<why>"} so a misconfigured
// daemon doesn't stall the synthesizer. Curated sources short-circuit
// with the fixed "curated source — human curation is the quality signal"
// verdict, same as the TS port.
func RunGate(ctx context.Context, llm LLMBackend, opts GateOpts) GateVerdict {
	if opts.SourceType == "curated" {
		return GateVerdict{
			Reliability: ReliabilityMedium,
			Reason:      "Curated source — human curation is a quality signal. Phase 2 gate skipped for curated.",
		}
	}
	tier := ScoreDomain(opts.SourceURL)
	preview := opts.Content
	if len(preview) > contentPreviewLimit {
		preview = preview[:contentPreviewLimit]
	}
	prompt := buildGatePrompt(opts.Title, opts.SourceURL, opts.Format, string(tier), preview)
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = defaultGateTimeout
	}
	stdout, err := llm.Complete(ctx, LLMRequest{
		Prompt:  prompt,
		Model:   opts.Model,
		Timeout: timeout,
		JSON:    true,
	})
	if err != nil {
		return GateVerdict{
			Reliability: ReliabilityMedium,
			Reason:      "Gate LLM call failed: " + err.Error(),
		}
	}
	v, parseErr := parseGateVerdict(stdout)
	if parseErr != nil {
		return GateVerdict{
			Reliability: ReliabilityMedium,
			Reason:      "Gate CLI returned unparseable response: " + parseErr.Error(),
		}
	}
	if v.Reliability == "" {
		return GateVerdict{
			Reliability: ReliabilityMedium,
			Reason:      "Gate CLI returned no reliability field",
		}
	}
	return v
}

// buildGatePrompt is the verbatim Phase 2 gate prompt from
// src/gate/evaluate.ts:71-115. Kept inline rather than loaded from
// disk so the binary self-contains the prompt — operator can read
// the source to know exactly what verdict criteria the LLM saw.
// Disk-based prompt loading is a Phase 5 hardening item.
func buildGatePrompt(title, sourceURL, format, tier, preview string) string {
	var b strings.Builder
	b.WriteString("You evaluate sources for a knowledge base. Return ONLY a single JSON object — no markdown fences, no preamble.\n\n")
	b.WriteString("Source under review:\n")
	fmt.Fprintf(&b, "  Title: %s\n", title)
	if sourceURL != "" {
		fmt.Fprintf(&b, "  URL: %s\n", sourceURL)
	}
	if format != "" {
		fmt.Fprintf(&b, "  Format: %s\n", format)
	}
	fmt.Fprintf(&b, "  Domain tier: %s\n", tier)
	b.WriteString("\nContent preview:\n")
	b.WriteString("---\n")
	b.WriteString(preview)
	b.WriteString("\n---\n\n")
	b.WriteString(`Evaluate the SOURCE (not the individual claims). Apply these rules:

Reliability tiers:
  high       — primary source, authoritative documentation, peer-reviewed,
               or the canonical upstream for the topic it describes.
  medium     — secondary source from a credible publisher; encyclopedic;
               well-known community sites with moderation.
  low        — informal personal opinion, marketing copy, low-evidence claims,
               anonymous or contested authorship.
  contested  — content makes claims that are actively disputed by
               authoritative sources, or mixes verifiable and unverifiable
               material in a way that's hard to disentangle.

When reliability is low or contested, set category to one of:
  source         — provenance is the issue (anonymous / unknown).
  formal         — claims contradict formal/well-established results.
  informal       — anecdote, opinion, lacking evidence.
  philosophical  — interpretive / value-judgement / unfalsifiable framing.

Topic (always set, even for high/medium):
  agents | memory | governance | tools | training | infrastructure |
  knowledge | multiagent | general

Respond with exactly:
{"reliability": "...", "category": "...", "topic": "...", "reason": "..."}
Omit "category" when reliability is high or medium. "reason" must be one short sentence.`)
	return b.String()
}

// parseGateVerdict extracts a GateVerdict from the CLI's stdout.
// Tolerates a leading code-fence (some prompts come back with one
// even when we ask not to) and trailing whitespace.
func parseGateVerdict(raw string) (GateVerdict, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return GateVerdict{}, errors.New("empty response")
	}
	// Some CLI outputs interleave a chatty preamble before the JSON.
	// Try to find the first { ... matching } block.
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return GateVerdict{}, errors.New("no JSON object in response")
	}
	raw = raw[start : end+1]
	var v GateVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return GateVerdict{}, err
	}
	return v, nil
}

// SummarizeContent calls the CLI with a distillation prompt and
// returns the 3-5 paragraph prose summary. Returns "" + nil error
// when summarization should be skipped (e.g. CLI missing); callers
// fall back to the raw content in that case.
func SummarizeContent(ctx context.Context, llm LLMBackend, title, content, model string, timeout time.Duration) (string, error) {
	if timeout == 0 {
		timeout = defaultSummarizeTimeout
	}
	preview := content
	if len(preview) > contentPreviewLimit {
		preview = preview[:contentPreviewLimit]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Summarise the following document into 3–5 paragraphs of flowing third-person prose.\n")
	b.WriteString("Capture: key claims, named entities, why this source matters.\n")
	b.WriteString("Do NOT add commentary. Do NOT use markdown headings or lists.\n\n")
	fmt.Fprintf(&b, "Title: %s\n\n", title)
	b.WriteString("Content:\n")
	b.WriteString(preview)
	out, err := llm.Complete(ctx, LLMRequest{
		Prompt:  b.String(),
		Model:   model,
		Timeout: timeout,
		// Free-form prose — do NOT constrain to JSON.
		JSON: false,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// CallClaudeCLI shells out to `claude --print --output-format text`
// with the prompt on stdin. Returns the trimmed stdout. ctx
// cancellation is honored; the timeout wins over a long-running
// CLI invocation. Errors are wrapped with the stderr tail for
// operator diagnosis.
func CallClaudeCLI(ctx context.Context, prompt, model string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"--print", "--output-format", "text"}
	if model != "" {
		args = append(args, "--model", model)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("claude CLI timed out after %s", timeout)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			tail := stderrTail(&stderr)
			return "", fmt.Errorf("claude CLI exited with code %d: %s", exitErr.ExitCode(), tail)
		}
		// Common case: binary not on PATH. Surface a clear message
		// so operators don't think the daemon is broken — they just
		// need to install Claude Code.
		if _, lerr := exec.LookPath("claude"); lerr != nil {
			return "", fmt.Errorf("claude CLI not on PATH (install Claude Code to enable the gate)")
		}
		return "", fmt.Errorf("claude CLI exec: %w", err)
	}
	return stdout.String(), nil
}

// stderrTail returns the last 512 chars of stderr — enough to show
// the operator the actual error message without dumping a multi-KB
// log line.
func stderrTail(buf *bytes.Buffer) string {
	s := strings.TrimSpace(buf.String())
	const limit = 512
	if len(s) > limit {
		s = "..." + s[len(s)-limit:]
	}
	if s == "" {
		return "(no stderr)"
	}
	return s
}

// ClaudeCLIAvailable reports whether the claude CLI is on PATH. Used
// by the synthesizer to decide whether to call the gate at all in
// dev environments where the CLI isn't installed.
func ClaudeCLIAvailable() bool {
	_, err := exec.LookPath("claude")
	if err != nil {
		return false
	}
	if _, err := os.Stat("/dev/null"); err != nil {
		// Truly weird environment; assume not available.
		return false
	}
	return true
}
