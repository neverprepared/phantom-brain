package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	pbbrain "github.com/neverprepared/mcp-phantom-brain/internal/brain"
)

// sha256New is wrapped so call sites read cleanly; trivial indirection.
func sha256New() hash.Hash { return sha256.New() }

// ReapResult summarises one pass of the reaper over the brains/_pending
// directory. Returned by ReapOnce so tests can assert on what happened
// without scraping logs.
type ReapResult struct {
	Inspected  []string // payload tar paths examined this pass
	Merged     []string // brain_ids successfully merged
	Quarantine []string // tar paths moved to _staging/<brain_id>/quarantine
	Errors     []string // brain_id + reason for soft failures
}

// runReaperLoop is the per-vault reaper goroutine. Polls every
// reaper_poll_interval_secs; one pending tar per iteration to keep
// per-call latency bounded. Exits on ctx.Done.
func (r *vaultRunner) runReaperLoop(ctx context.Context) {
	defer r.wg.Done()
	interval := time.Duration(r.Binding.Defaults.ReaperPollIntervalSecs) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	r.logger.Info("phantom-brain: reaper loop started", slog.String("interval", interval.String()))
	for {
		select {
		case <-ctx.Done():
			r.logger.Info("phantom-brain: reaper loop exiting")
			return
		case <-t.C:
			if _, err := ReapOnce(r.DataDir, r.Binding, r.logger, &r.mu); err != nil {
				r.logger.Warn("phantom-brain: reap pass error", slog.String("err", err.Error()))
			}
		}
	}
}

// ReapOnce runs a single reaper pass: scans brains/_pending for
// payload tarballs, extracts each via SafeExtract, merges its Raw/
// content into the collective vault with SHA256 dedup, enqueues
// synthesis items, inserts the merge into the ledger, and finally
// moves the tarball to _merged/ (the merge record JSON, not the
// tarball — that gets deleted once the JSON is durable).
//
// Per-merge mu lock keeps the queue+raw+ledger writes ordered against
// the synthesizer's ClaimNextItem; without it the synth could claim a
// queue file whose corresponding Raw payload hadn't landed yet.
func ReapOnce(dataDir DataDir, binding VaultBinding, logger *slog.Logger, mu interface{ Lock(); Unlock() }) (*ReapResult, error) {
	pendingDir := filepath.Join(dataDir.BrainsDir(binding.Key.Profile, binding.Key.Vault), "_pending")
	entries, err := os.ReadDir(pendingDir)
	if errors.Is(err, os.ErrNotExist) {
		return &ReapResult{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("server: read pending: %w", err)
	}
	res := &ReapResult{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".tar") {
			continue
		}
		tarPath := filepath.Join(pendingDir, e.Name())
		res.Inspected = append(res.Inspected, tarPath)

		brainID, mergeErr := reapOnePayload(dataDir, binding, tarPath, logger, mu)
		if mergeErr != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", filepath.Base(tarPath), mergeErr))
			// Quarantine on unrecoverable extract failures so the
			// reaper doesn't churn on the same broken tar forever.
			if IsSafeTarErrorKind(mergeErr, SafeTarErrTraversal) ||
				IsSafeTarErrorKind(mergeErr, SafeTarErrSymlinkEscape) ||
				IsSafeTarErrorKind(mergeErr, SafeTarErrSizeCap) ||
				IsSafeTarErrorKind(mergeErr, SafeTarErrFileCap) ||
				IsSafeTarErrorKind(mergeErr, SafeTarErrUnsupportedEntry) {
				qpath := quarantine(dataDir, binding.Key, tarPath, logger)
				res.Quarantine = append(res.Quarantine, qpath)
			}
			continue
		}
		res.Merged = append(res.Merged, brainID)
	}
	return res, nil
}

// reapOnePayload does the per-tarball work. Returns the brain_id of
// the merged payload on success. Caller decides what to do with the
// tarball based on whether this returns nil.
func reapOnePayload(dataDir DataDir, binding VaultBinding, tarPath string, logger *slog.Logger, mu interface{ Lock(); Unlock() }) (string, error) {
	// Stage extract into a brain-specific dir under brains/_staging/.
	// Use the tarball basename minus .tar as the dir name; the
	// manifest within can override once parsed.
	stem := strings.TrimSuffix(filepath.Base(tarPath), ".tar")
	stagingDir := filepath.Join(dataDir.BrainsDir(binding.Key.Profile, binding.Key.Vault),
		"_staging", stem+"-"+randomHex(6))
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return "", fmt.Errorf("server: mkdir staging: %w", err)
	}

	f, err := os.Open(tarPath)
	if err != nil {
		return "", fmt.Errorf("server: open pending: %w", err)
	}
	defer f.Close()

	limits := SafeTarLimits{
		MaxUncompressedBytes: binding.Defaults.MaxUncompressedBytes,
		MaxFiles:             10000, // sane upper bound, not configurable yet
	}
	if err := SafeExtract(f, stagingDir, limits); err != nil {
		_ = os.RemoveAll(stagingDir)
		return "", err
	}

	// Parse the brain's own manifest to learn its brain_id +
	// contributor_id. The manifest lives at the tarball root per
	// internal/brain/death.go packDeathPayload.
	bm, err := readBrainManifest(filepath.Join(stagingDir, pbbrain.ManifestFilename))
	if err != nil {
		_ = os.RemoveAll(stagingDir)
		return "", fmt.Errorf("server: parse brain manifest: %w", err)
	}
	if bm.Profile != binding.Key.Profile || bm.Vault != binding.Key.Vault {
		_ = os.RemoveAll(stagingDir)
		return "", fmt.Errorf("server: payload manifest (%s/%s) does not match vault (%s)",
			bm.Profile, bm.Vault, binding.Key)
	}

	mu.Lock()
	defer mu.Unlock()

	rawCount, attachCount, err := mergeIntoCollective(dataDir, binding.Key, stagingDir, logger)
	if err != nil {
		return "", fmt.Errorf("server: merge collective: %w", err)
	}

	// Ledger insert. ErrDuplicateMerge is a hard failure — the same
	// brain shouldn't ever die twice, and silently re-merging would
	// be a footgun. Reaper logs and moves on; the tar stays in
	// pending so an operator can decide.
	ledger, err := OpenLedger(dataDir, binding.Key.Profile, binding.Key.Vault)
	if err != nil {
		return "", err
	}
	defer ledger.Close()

	tarStat, _ := os.Stat(tarPath)
	var payloadBytes int64
	if tarStat != nil {
		payloadBytes = tarStat.Size()
	}
	if err := ledger.Insert(MergeRecord{
		BrainID:         bm.BrainID,
		ContributorID:   bm.ContributorID,
		Profile:         bm.Profile,
		Vault:           bm.Vault,
		MergedAt:        time.Now().UTC(),
		RawCount:        rawCount,
		AttachmentCount: attachCount,
		PayloadBytes:    payloadBytes,
	}); err != nil {
		return "", err
	}

	// Persist the merge record as JSON in _merged/ then remove the
	// staging dir and the original tar. The JSON is the audit
	// breadcrumb; the tar contents are now durable across Raw/ +
	// queue/ + ledger so the tar itself is redundant.
	mergedJSON := filepath.Join(dataDir.BrainsDir(binding.Key.Profile, binding.Key.Vault),
		"_merged", bm.BrainID+".json")
	if err := os.MkdirAll(filepath.Dir(mergedJSON), 0o755); err != nil {
		return "", err
	}
	bj, _ := json.MarshalIndent(map[string]any{
		"brain_id":         bm.BrainID,
		"contributor_id":   bm.ContributorID,
		"profile":          bm.Profile,
		"vault":            bm.Vault,
		"merged_at":        time.Now().UTC().Format(time.RFC3339),
		"raw_count":        rawCount,
		"attachment_count": attachCount,
		"payload_bytes":    payloadBytes,
	}, "", "  ")
	if err := os.WriteFile(mergedJSON, append(bj, '\n'), 0o644); err != nil {
		return "", err
	}
	_ = os.RemoveAll(stagingDir)
	_ = os.Remove(tarPath)

	logger.Info("phantom-brain: merged death payload",
		slog.String("brain_id", bm.BrainID),
		slog.String("contributor_id", bm.ContributorID),
		slog.Int("raw", rawCount),
		slog.Int("attachments", attachCount),
	)
	return bm.BrainID, nil
}

// readBrainManifest parses the brain's manifest.json at the given
// path. Used by reapOnePayload before any merge work; matches the
// schema written by internal/brain/manifest.go.
func readBrainManifest(path string) (*pbbrain.Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m pbbrain.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.BrainID == "" {
		return nil, errors.New("manifest missing brain_id")
	}
	return &m, nil
}

// mergeIntoCollective moves a staged payload's Raw/ files into the
// collective vault, deduping by SHA256 of the file's bytes and
// enqueueing a synth item for each NEW raw document. Returns
// (rawCount, attachmentCount) so the ledger row is accurate.
//
// Dedup rule: if a file with the same SHA256 already exists in the
// collective Raw/ (curated or gathered), the staged copy is dropped
// silently — that file already produced its synthesis output. The
// queue is NOT re-appended for duplicates.
func mergeIntoCollective(dataDir DataDir, key VaultKey, stagingDir string, logger *slog.Logger) (rawCount, attachCount int, err error) {
	rawRoot := filepath.Join(stagingDir, "vault", "Raw")

	// Curated + gathered: hashed move into collective, queue enqueue
	// per new file.
	for _, sub := range []string{"curated", "gathered"} {
		srcDir := filepath.Join(rawRoot, sub)
		entries, err := os.ReadDir(srcDir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return rawCount, attachCount, err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			srcPath := filepath.Join(srcDir, e.Name())
			sha, err := sha256File(srcPath)
			if err != nil {
				return rawCount, attachCount, err
			}
			dstPath := filepath.Join(dataDir.VaultDir(key.Profile, key.Vault), "Raw", sub, e.Name())
			if existing, dupErr := sha256IfExists(dstPath); dupErr == nil && existing == sha {
				continue // exact same file already merged earlier — skip silently
			}
			// Collision-with-different-content: append the sha prefix
			// to the destination name so the new file lands without
			// clobbering. The queue entry uses the actual landed name.
			if _, err := os.Stat(dstPath); err == nil {
				dstPath = collisionRename(dstPath, sha)
			}
			if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
				return rawCount, attachCount, err
			}
			if err := os.Rename(srcPath, dstPath); err != nil {
				return rawCount, attachCount, err
			}
			rel, _ := filepath.Rel(dataDir.VaultDir(key.Profile, key.Vault), dstPath)
			item := QueueItem{
				RawPath:     rel,
				Source:      sub,
				CapturedAt:  time.Now().UTC().Format(time.RFC3339),
				Title:       strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())),
				Format:      formatFromFilename(e.Name()),
				ContentHash: sha,
			}
			if _, err := EnqueueItem(dataDir, key.Profile, key.Vault, item, sha[:12]); err != nil {
				return rawCount, attachCount, err
			}
			rawCount++
		}
	}

	// Attachments: dedup by SHA prefix in the filename (matches the
	// TS shape). Filenames are <sha>.<ext>; if the same sha already
	// exists we drop.
	attachSrc := filepath.Join(rawRoot, "attachments")
	attachDst := filepath.Join(dataDir.VaultDir(key.Profile, key.Vault), "Raw", "attachments")
	if entries, err := os.ReadDir(attachSrc); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			srcPath := filepath.Join(attachSrc, e.Name())
			dstPath := filepath.Join(attachDst, e.Name())
			if _, err := os.Stat(dstPath); err == nil {
				continue // same sha — skip
			}
			if err := os.MkdirAll(attachDst, 0o755); err != nil {
				return rawCount, attachCount, err
			}
			if err := os.Rename(srcPath, dstPath); err != nil {
				return rawCount, attachCount, err
			}
			attachCount++
		}
	}
	return rawCount, attachCount, nil
}

// sha256File computes the SHA256 hex of a regular file.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sha256IfExists returns the sha for a file if it exists; nil error
// + empty string when the file is missing.
func sha256IfExists(path string) (string, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return sha256File(path)
}

// collisionRename appends -<sha-prefix> to the basename before its
// extension so the new file doesn't overwrite the existing one.
func collisionRename(path, sha string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	return base + "-" + sha[:8] + ext
}

// quarantine moves a problem tarball to _staging/quarantine/ with a
// timestamp suffix so operators can inspect it without the reaper
// re-trying. Returns the destination path (or empty + logs on error).
func quarantine(dataDir DataDir, key VaultKey, tarPath string, logger *slog.Logger) string {
	qDir := filepath.Join(dataDir.BrainsDir(key.Profile, key.Vault), "_staging", "quarantine")
	if err := os.MkdirAll(qDir, 0o755); err != nil {
		logger.Warn("phantom-brain: quarantine mkdir failed", slog.String("err", err.Error()))
		return ""
	}
	dst := filepath.Join(qDir, fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102T150405Z"), filepath.Base(tarPath)))
	if err := os.Rename(tarPath, dst); err != nil {
		logger.Warn("phantom-brain: quarantine rename failed", slog.String("err", err.Error()))
		return ""
	}
	logger.Warn("phantom-brain: quarantined unsafe payload", slog.String("path", dst))
	return dst
}

// formatFromFilename maps a file extension to a format string used by
// the synthesizer's gate (it differentiates markdown from html etc.).
func formatFromFilename(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return "markdown"
	case ".html", ".htm":
		return "html"
	case ".txt":
		return "text"
	case ".json":
		return "json"
	default:
		return "unknown"
	}
}

// randomHex generates n bytes of entropy as hex. Used for staging dir
// uniqueness — failure to read randomness is fatal at startup, not
// silently swallowed.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("server: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
