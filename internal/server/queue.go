package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// QueueItem is the JSON file the reaper drops into queue/pending/
// for each newly-arrived raw document. Mirrors the TS QueueItem
// shape (src/vault/queue.ts) so a same-vault TS synthesizer running
// in parallel could still claim items. Only fields actively used
// in Phase 2 are populated; the others stay omitted (omitempty)
// rather than zero so the JSON is small + diff-friendly.
type QueueItem struct {
	RawPath     string `json:"raw_path"`               // Raw/{curated,gathered}/<file>
	Source      string `json:"source"`                 // "curated" | "gathered"
	CapturedAt  string `json:"captured_at"`            // RFC3339
	Title       string `json:"title,omitempty"`
	Format      string `json:"format,omitempty"`       // "markdown" | "html" | ...
	ContentHash string `json:"content_hash,omitempty"` // SHA256 hex
	SourceURL   string `json:"source_url,omitempty"`
	SourceAttachment string `json:"source_attachment,omitempty"` // Raw/attachments/<sha>.ext
}

// EnqueueItem writes a QueueItem to queue/pending/<timestamp>-<rand>.json
// using vault.WriteAtomicFile semantics so a synthesizer claiming
// items concurrently cannot see a half-written file.
//
// The filename pattern guarantees per-process uniqueness via the
// 12-hex suffix (collision odds are negligible for the volumes we
// expect). Same-name collision across two daemons is impossible
// since the daemon owns the global flock.
func EnqueueItem(dataDir DataDir, profile, vaultName string, item QueueItem, suffix string) (string, error) {
	if item.RawPath == "" {
		return "", errors.New("server: EnqueueItem requires raw_path")
	}
	if item.Source == "" {
		return "", errors.New("server: EnqueueItem requires source")
	}
	if item.CapturedAt == "" {
		item.CapturedAt = time.Now().UTC().Format(time.RFC3339)
	}
	pendingDir := filepath.Join(dataDir.VaultDir(profile, vaultName), "queue", "pending")
	if err := os.MkdirAll(pendingDir, 0o755); err != nil {
		return "", fmt.Errorf("server: mkdir queue/pending: %w", err)
	}
	stamp := time.Now().UTC().Format("20060102T150405.000Z")
	name := fmt.Sprintf("%s-%s.json", stamp, suffix)
	path := filepath.Join(pendingDir, name)
	body, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ListPending returns every pending queue item path, sorted by name
// (which sorts chronologically since the prefix is a timestamp).
// Returns nil + nil for an empty/missing dir.
func ListPending(dataDir DataDir, profile, vaultName string) ([]string, error) {
	pendingDir := filepath.Join(dataDir.VaultDir(profile, vaultName), "queue", "pending")
	entries, err := os.ReadDir(pendingDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, filepath.Join(pendingDir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// ClaimNextItem moves the oldest pending item to queue/claimed/ via
// atomic rename — the rename IS the lock, so two concurrent claimers
// cannot grab the same file. Returns (claimedPath, item, nil) on
// success, (\"\", nil, nil) when the queue is empty.
//
// Phase 2 synthesizer claims items one at a time; the per-vault
// runner's mutex keeps the claim sequenced with reaper ingests, but
// the rename atomicity is what protects against external claimers
// (e.g. a same-vault TS server still running).
func ClaimNextItem(dataDir DataDir, profile, vaultName string) (string, *QueueItem, error) {
	pending, err := ListPending(dataDir, profile, vaultName)
	if err != nil {
		return "", nil, err
	}
	if len(pending) == 0 {
		return "", nil, nil
	}
	claimedDir := filepath.Join(dataDir.VaultDir(profile, vaultName), "queue", "claimed")
	if err := os.MkdirAll(claimedDir, 0o755); err != nil {
		return "", nil, err
	}
	for _, src := range pending {
		dst := filepath.Join(claimedDir, filepath.Base(src))
		if err := os.Rename(src, dst); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Another claimer beat us; try the next one.
				continue
			}
			return "", nil, fmt.Errorf("server: claim rename: %w", err)
		}
		raw, rerr := os.ReadFile(dst)
		if rerr != nil {
			return "", nil, fmt.Errorf("server: read claimed: %w", rerr)
		}
		var item QueueItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return "", nil, fmt.Errorf("server: parse %s: %w", dst, err)
		}
		return dst, &item, nil
	}
	return "", nil, nil
}

// MarkDone moves a claimed item to queue/done/. Called once the
// synthesizer has successfully written summary + entities + log.
// Idempotent — a double-mark just renames a non-existent file and
// returns os.ErrNotExist, which the caller can ignore.
func MarkDone(dataDir DataDir, profile, vaultName, claimedPath string) error {
	doneDir := filepath.Join(dataDir.VaultDir(profile, vaultName), "queue", "done")
	if err := os.MkdirAll(doneDir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(doneDir, filepath.Base(claimedPath))
	return os.Rename(claimedPath, dst)
}

// MarkDead moves a claimed item to queue/dead/ when synthesis failed
// in a way the synthesizer doesn't want to retry. New in Phase 2 —
// the TS implementation silently re-queued, which masked persistent
// gate failures behind apparent retries.
func MarkDead(dataDir DataDir, profile, vaultName, claimedPath string) error {
	deadDir := filepath.Join(dataDir.VaultDir(profile, vaultName), "queue", "dead")
	if err := os.MkdirAll(deadDir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(deadDir, filepath.Base(claimedPath))
	return os.Rename(claimedPath, dst)
}
