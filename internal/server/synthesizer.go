package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/neverprepared/mcp-phantom-brain/internal/vault"
)

// synthesizerLoop is the per-vault queue consumer. Polls every
// reaper_poll_interval_secs (same cadence as the reaper) and processes
// at most one item per tick — keeps per-iteration latency bounded
// and gives the reaper room to land new payloads between claims.
// Exits on ctx.Done.
func (r *vaultRunner) synthesizerLoop(ctx context.Context) {
	defer r.wg.Done()
	interval := time.Duration(r.Binding.Defaults.ReaperPollIntervalSecs) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	r.logger.Info("phantom-brain: synthesizer loop started", slog.String("interval", interval.String()))
	for {
		select {
		case <-ctx.Done():
			r.logger.Info("phantom-brain: synthesizer loop exiting")
			return
		case <-t.C:
			if InMaintenance(r.DataDir, r.Binding.Key) {
				continue
			}
			if _, err := SynthesizeOne(ctx, r.DataDir, r.Binding, r.logger, &r.mu); err != nil && !errors.Is(err, errQueueEmpty) {
				r.logger.Warn("phantom-brain: synth pass error", slog.String("err", err.Error()))
			}
		}
	}
}

// errQueueEmpty signals "queue had nothing to claim — not an error,
// just nothing to do this tick". Distinct from other errors so the
// loop knows not to log.
var errQueueEmpty = errors.New("synth: queue empty")

// SynthesisResult is the return value from SynthesizeOne. Exposed so
// future ops tooling (force-synthesize subcommand) can show what
// happened.
type SynthesisResult struct {
	QueueItem    string // path of the claimed file (in queue/claimed/)
	RawPath      string
	SummaryPath  string
	EntityPaths  []string
	Reliability  Reliability
	Topic        Topic
}

// SynthesizeOne runs one queue item through coherence → gate →
// distill → write summary + entities + log + provenance → markDone.
// Returns errQueueEmpty when the queue had nothing to claim. Errors
// during processing move the item to queue/dead/ rather than
// retrying — silent retry was the TS behavior and it masked
// persistent failures.
//
// Caller holds the runner's mutex (passed as mu) so the claim is
// ordered against concurrent reaper merges.
func SynthesizeOne(ctx context.Context, dataDir DataDir, binding VaultBinding, logger *slog.Logger, mu interface{ Lock(); Unlock() }) (*SynthesisResult, error) {
	mu.Lock()
	claimedPath, item, err := ClaimNextItem(dataDir, binding.Key.Profile, binding.Key.Vault)
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	if claimedPath == "" {
		return nil, errQueueEmpty
	}

	res, err := processQueueItem(ctx, dataDir, binding, item, logger)
	if err != nil {
		logger.Warn("phantom-brain: synth failed; moving to queue/dead",
			slog.String("item", item.RawPath),
			slog.String("err", err.Error()),
		)
		if mvErr := MarkDead(dataDir, binding.Key.Profile, binding.Key.Vault, claimedPath); mvErr != nil {
			logger.Warn("phantom-brain: mark-dead also failed", slog.String("err", mvErr.Error()))
		}
		return nil, err
	}
	res.QueueItem = claimedPath
	if mvErr := MarkDone(dataDir, binding.Key.Profile, binding.Key.Vault, claimedPath); mvErr != nil {
		logger.Warn("phantom-brain: mark-done failed (synth succeeded)", slog.String("err", mvErr.Error()))
	}
	return res, nil
}

// processQueueItem is the synthesis pipeline proper. Side-effects:
// writes summary + entity pages + log + provenance. Returns the
// SynthesisResult on success.
func processQueueItem(ctx context.Context, dataDir DataDir, binding VaultBinding, item *QueueItem, logger *slog.Logger) (*SynthesisResult, error) {
	rawAbs := filepath.Join(dataDir.VaultDir(binding.Key.Profile, binding.Key.Vault), item.RawPath)
	contentBytes, err := os.ReadFile(rawAbs)
	if err != nil {
		return nil, fmt.Errorf("read raw %s: %w", item.RawPath, err)
	}
	content := string(contentBytes)

	// Coherence first — it's free and rejects obviously-broken input
	// before paying for the LLM.
	verdict := GateVerdict{Topic: TopicGeneral, Reliability: ReliabilityMedium}
	if cr := CheckCoherence(content); !cr.Passed {
		verdict = GateVerdict{
			Reliability: ReliabilityLow,
			Category:    CategoryInformal,
			Topic:       TopicGeneral,
			Reason:      "coherence-fail: " + cr.Reason,
		}
	} else {
		verdict = RunGate(ctx, GateOpts{
			Title:      item.Title,
			SourceURL:  item.SourceURL,
			Content:    content,
			Format:     item.Format,
			SourceType: item.Source,
		})
	}

	// Distill. Skip when the CLI is missing — operator gets the raw
	// content rendered into the summary body, with the verdict reason
	// explaining why no LLM summary is present.
	summary := ""
	if ClaudeCLIAvailable() {
		s, err := SummarizeContent(ctx, item.Title, content, "", 0)
		if err != nil {
			logger.Warn("phantom-brain: summarize failed; using raw content as summary body",
				slog.String("err", err.Error()))
		} else {
			summary = s
		}
	}
	if summary == "" {
		// Raw content fallback so the summary page is never empty.
		summary = content
	}

	// Write summary page.
	summaryPath, slug, err := writeSummaryPage(dataDir, binding.Key, item, verdict, summary)
	if err != nil {
		return nil, fmt.Errorf("write summary: %w", err)
	}

	// Entity pages — heuristic extracted from the RAW content (not
	// the LLM summary) per the TS rationale (entity coverage is more
	// faithful on the original text).
	entities := ExtractEntities(content)
	var entityPaths []string
	for _, ent := range entities {
		path, err := upsertEntityPage(dataDir, binding.Key, ent, item, verdict, content, slug)
		if err != nil {
			logger.Warn("phantom-brain: entity upsert failed (continuing)",
				slog.String("entity", ent), slog.String("err", err.Error()))
			continue
		}
		entityPaths = append(entityPaths, path)
	}

	// Append to Wiki/_log.md.
	if err := appendSynthesisLog(dataDir, binding.Key, item, summaryPath, entityPaths, verdict); err != nil {
		logger.Warn("phantom-brain: log append failed (continuing)", slog.String("err", err.Error()))
	}

	// Upsert provenance.
	if err := upsertProvenance(dataDir, binding.Key, item, summaryPath, entityPaths, verdict); err != nil {
		logger.Warn("phantom-brain: provenance upsert failed (continuing)", slog.String("err", err.Error()))
	}

	return &SynthesisResult{
		RawPath:     item.RawPath,
		SummaryPath: summaryPath,
		EntityPaths: entityPaths,
		Reliability: verdict.Reliability,
		Topic:       verdict.Topic,
	}, nil
}

// writeSummaryPage drops Wiki/summaries/<slug>[-N].md with the
// frontmatter + body shape ported from src/tools/brain-synthesize.ts:
// buildSummaryPage. Returns the relative path used in provenance.
func writeSummaryPage(dataDir DataDir, key VaultKey, item *QueueItem, v GateVerdict, body string) (relPath, slug string, err error) {
	slug = slugify(item.Title)
	if slug == "" {
		slug = "untitled"
	}
	summariesDir := filepath.Join(dataDir.VaultDir(key.Profile, key.Vault), "Wiki", "summaries")
	if err := os.MkdirAll(summariesDir, 0o755); err != nil {
		return "", "", err
	}
	// Disambiguate filename collisions with a numeric suffix.
	chosen := slug
	for n := 1; n < 1000; n++ {
		candidate := filepath.Join(summariesDir, chosen+".md")
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			break
		}
		chosen = fmt.Sprintf("%s-%d", slug, n+1)
	}
	page := buildSummaryPage(item, v, body)
	if err := vault.WriteAtomicFile(filepath.Join(summariesDir, chosen+".md"), []byte(page), 0o644); err != nil {
		return "", "", err
	}
	return filepath.Join("Wiki", "summaries", chosen+".md"), slug, nil
}

// buildSummaryPage renders the YAML frontmatter + body. Frontmatter
// keys match the TS shape so existing TS Wiki indexers still parse.
func buildSummaryPage(item *QueueItem, v GateVerdict, body string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", yamlEscape(item.Title))
	b.WriteString("kind: summary\n")
	if item.Source != "" {
		fmt.Fprintf(&b, "source: %s\n", item.Source)
	}
	if item.SourceURL != "" {
		fmt.Fprintf(&b, "source_url: %s\n", item.SourceURL)
	}
	if item.SourceAttachment != "" {
		fmt.Fprintf(&b, "source_attachment: %s\n", item.SourceAttachment)
	}
	if item.CapturedAt != "" {
		fmt.Fprintf(&b, "captured_at: %s\n", item.CapturedAt)
	}
	fmt.Fprintf(&b, "synthesized_at: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "reliability: %s\n", v.Reliability)
	if v.Category != "" {
		fmt.Fprintf(&b, "category: %s\n", v.Category)
	}
	if v.Topic != "" {
		fmt.Fprintf(&b, "topic: %s\n", v.Topic)
	}
	if v.Reason != "" {
		fmt.Fprintf(&b, "reason: %s\n", yamlEscape(v.Reason))
	}
	b.WriteString("---\n\n")
	if item.SourceAttachment != "" {
		fmt.Fprintf(&b, "**Source attachment:** [[%s]]\n\n", item.SourceAttachment)
	}
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// upsertEntityPage creates or appends to Wiki/entities/<slug>.md.
// The first writer creates the page with a Source Reliability table
// header; subsequent writers append a row + snippet. Per-vault file
// lock isn't necessary because we hold the runner mutex — no other
// synthesizer is running for this vault.
func upsertEntityPage(dataDir DataDir, key VaultKey, entity string, item *QueueItem, v GateVerdict, rawBody, summarySlug string) (string, error) {
	slug := slugify(entity)
	if slug == "" {
		return "", errors.New("empty entity slug")
	}
	dir := filepath.Join(dataDir.VaultDir(key.Profile, key.Vault), "Wiki", "entities")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, slug+".md")
	relPath := filepath.Join("Wiki", "entities", slug+".md")

	snippet := EntitySnippet(rawBody, entity)
	row := buildEntityTableRow(item, v)
	mention := buildEntityMentionBlock(item, summarySlug, snippet)

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		page := buildEntityPageHeader(entity) + row + "\n## Mentions\n\n" + mention
		if err := vault.WriteAtomicFile(path, []byte(page), 0o644); err != nil {
			return "", err
		}
		return relPath, nil
	}

	// Append: read current body, append row to the table + a new
	// mention block. We don't try to be smart about table dedup —
	// every synthesis run is one row, and duplicates are operator-
	// visible signal that the same raw was re-merged.
	existing, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	appended := string(existing)
	// Insert the row just before "## Mentions" if present, else
	// append it at the end.
	if idx := strings.Index(appended, "\n## Mentions"); idx >= 0 {
		appended = appended[:idx] + row + appended[idx:]
	} else {
		appended += row + "\n## Mentions\n\n"
	}
	appended += mention
	if err := vault.WriteAtomicFile(path, []byte(appended), 0o644); err != nil {
		return "", err
	}
	return relPath, nil
}

// buildEntityPageHeader returns the boilerplate at the top of a
// fresh entity page.
func buildEntityPageHeader(entity string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", yamlEscape(entity))
	b.WriteString("kind: entity\n")
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "# %s\n\n", entity)
	b.WriteString("## Source Reliability\n\n")
	b.WriteString("| Source | Reliability | Topic | Captured | Attachment |\n")
	b.WriteString("|---|---|---|---|---|\n")
	return b.String()
}

// buildEntityTableRow returns one row of the Source Reliability
// table.
func buildEntityTableRow(item *QueueItem, v GateVerdict) string {
	src := item.SourceURL
	if src == "" {
		src = item.RawPath
	}
	attach := ""
	if item.SourceAttachment != "" {
		attach = "[[" + item.SourceAttachment + "]]"
	}
	return fmt.Sprintf("| %s | %s | %s | %s | %s |\n",
		mdTableEscape(src), v.Reliability, v.Topic, item.CapturedAt, attach)
}

// buildEntityMentionBlock returns a per-source snippet block under
// the Mentions section.
func buildEntityMentionBlock(item *QueueItem, summarySlug, snippet string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### From [[Wiki/summaries/%s.md|%s]]\n\n", summarySlug, yamlEscape(item.Title))
	b.WriteString(snippet)
	b.WriteString("\n\n")
	return b.String()
}

// appendSynthesisLog appends one entry to Wiki/_log.md matching the
// TS format (timestamp + title + source + summary + entities + gate).
func appendSynthesisLog(dataDir DataDir, key VaultKey, item *QueueItem, summaryPath string, entityPaths []string, v GateVerdict) error {
	logPath := filepath.Join(dataDir.VaultDir(key.Profile, key.Vault), "Wiki", "_log.md")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s — %s\n", time.Now().UTC().Format(time.RFC3339), item.Title)
	fmt.Fprintf(&b, "- Source: %s\n", item.RawPath)
	fmt.Fprintf(&b, "- Summary: %s\n", summaryPath)
	if len(entityPaths) > 0 {
		fmt.Fprintf(&b, "- Entities: %s\n", strings.Join(entityNames(entityPaths), ", "))
	}
	fmt.Fprintf(&b, "- Gate: %s — %s\n\n", v.Reliability, v.Reason)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		return err
	}
	return f.Sync()
}

// entityNames extracts the basename (no .md) from entity paths for
// the log line.
func entityNames(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = strings.TrimSuffix(filepath.Base(p), ".md")
	}
	return out
}

// provenanceMap is the shape of _index/provenance.json. Mirrors the
// TS structure exactly.
type provenanceMap map[string]provenanceEntry

type provenanceEntry struct {
	WikiPages     []string    `json:"wiki_pages"`
	SynthesizedAt string      `json:"synthesized_at"`
	Reliability   Reliability `json:"reliability"`
	Category      Category    `json:"category,omitempty"`
	Topic         Topic       `json:"topic,omitempty"`
	ContentHash   string      `json:"content_hash"`
}

// upsertProvenance reads + merges + writes _index/provenance.json
// keyed by the raw_path. The runner mutex serialises us with the
// reaper's writes so an atomic read-merge-write under WriteAtomicFile
// is sufficient.
func upsertProvenance(dataDir DataDir, key VaultKey, item *QueueItem, summaryPath string, entityPaths []string, v GateVerdict) error {
	provPath := filepath.Join(dataDir.IndexDir(key.Profile, key.Vault), "provenance.json")
	prov := provenanceMap{}
	if raw, err := os.ReadFile(provPath); err == nil {
		if err := json.Unmarshal(raw, &prov); err != nil {
			return fmt.Errorf("parse existing provenance: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pages := append([]string{summaryPath}, entityPaths...)
	prov[item.RawPath] = provenanceEntry{
		WikiPages:     pages,
		SynthesizedAt: time.Now().UTC().Format(time.RFC3339),
		Reliability:   v.Reliability,
		Category:      v.Category,
		Topic:         v.Topic,
		ContentHash:   item.ContentHash,
	}
	body, err := json.MarshalIndent(prov, "", "  ")
	if err != nil {
		return err
	}
	return vault.WriteAtomicFile(provPath, append(body, '\n'), 0o644)
}

// --- maintenance flag ------------------------------------------------

// InMaintenance reports whether the vault's maintenance.flag is set.
// The synthesizer pauses claims while the flag is present.
func InMaintenance(dataDir DataDir, key VaultKey) bool {
	_, err := os.Stat(filepath.Join(dataDir.VaultLocksDir(key.Profile, key.Vault), "maintenance.flag"))
	return err == nil
}

// SetMaintenance creates the maintenance.flag. Idempotent.
func SetMaintenance(dataDir DataDir, key VaultKey) error {
	if err := os.MkdirAll(dataDir.VaultLocksDir(key.Profile, key.Vault), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir.VaultLocksDir(key.Profile, key.Vault), "maintenance.flag"),
		[]byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// ClearMaintenance removes the maintenance.flag. Idempotent.
func ClearMaintenance(dataDir DataDir, key VaultKey) error {
	err := os.Remove(filepath.Join(dataDir.VaultLocksDir(key.Profile, key.Vault), "maintenance.flag"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// --- helpers ---------------------------------------------------------

// slugify produces a filesystem-safe slug from a title. Lowercased,
// non-alphanumerics replaced with hyphen, repeated hyphens collapsed.
// Mirrors src/vault/slug.ts behaviour closely enough for our use; not
// worth a 1:1 port given the simplicity.
func slugify(s string) string {
	var b strings.Builder
	prevHyphen := true // suppress leading hyphens
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 80 {
		out = out[:80]
		out = strings.TrimRight(out, "-")
	}
	return out
}

// yamlEscape returns a YAML-safe string. Wraps in double quotes if
// the value contains characters that would confuse the scalar parser.
func yamlEscape(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, ":#\"'\n\r\t[]{}|>&%@*") {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		s = strings.ReplaceAll(s, "\n", `\n`)
		return `"` + s + `"`
	}
	return s
}

// mdTableEscape escapes pipe characters that would break a Markdown
// table row. Newlines are turned into spaces for the same reason.
func mdTableEscape(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
