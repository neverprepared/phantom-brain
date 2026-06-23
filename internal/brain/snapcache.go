package brain

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

	"github.com/neverprepared/phantom-brain/internal/config"
	"github.com/neverprepared/phantom-brain/internal/vault"
)

// CachedSnapshot describes a snapshot that was previously downloaded
// from the daemon and stored locally. Used during degraded birth
// (snapshot fetch fails -> fall back to the most recent cache) and to
// surface staleness in brain_status output. The tarball itself sits
// alongside this metadata as snapshot-<gen>.tar.zst.
type CachedSnapshot struct {
	Gen         uint64 `json:"gen"`
	SHA256      string `json:"sha256"`
	SizeBytes   int64  `json:"size_bytes"`
	FetchedAt   string `json:"fetched_at"` // RFC3339
	BuiltAt     string `json:"built_at"`   // RFC3339, daemon-side timestamp
	TarballPath string `json:"tarball_path"`
}

// ListCachedSnapshots returns every cached snapshot under
// SnapshotCacheDir(), sorted newest-first by generation number. Used
// by birth's degraded path to choose a seed and by operator tools to
// report cache state.
func ListCachedSnapshots(cfg *config.Agent) ([]CachedSnapshot, error) {
	if cfg == nil {
		return nil, errors.New("brain: ListCachedSnapshots requires a non-nil Agent")
	}
	root := cfg.SnapshotCacheDir()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("brain: read snapshot cache: %w", err)
	}
	var out []CachedSnapshot
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".manifest.json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		var cs CachedSnapshot
		if err := json.Unmarshal(raw, &cs); err != nil {
			continue
		}
		out = append(out, cs)
	}
	// Newest gen first — degraded birth wants the freshest available.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Gen > out[i].Gen {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// FetchSnapshotFromDaemon fetches /api/brain/snapshot/current, then
// downloads /api/brain/snapshot/{gen}/tarball (SHA-verified) into the
// snapshot cache. Returns the cached metadata so the caller can
// either reflink/extract into a brain dir or treat gen=0 as
// "greenfield seed".
//
// Phase 2.5 wiring: in Phase 1 this returned ErrDaemonUnavailable.
// Now we make the real call and only fall back to the cache (or the
// greenfield path) when the daemon is genuinely unreachable.
func FetchSnapshotFromDaemon(ctx context.Context, cfg *config.Agent, logger *slog.Logger) (*CachedSnapshot, error) {
	if cfg == nil {
		return nil, errors.New("brain: FetchSnapshotFromDaemon requires a non-nil Agent")
	}
	if logger == nil {
		logger = slog.Default()
	}
	client, err := NewClient(ClientOpts{BaseURL: cfg.API, Token: cfg.Token})
	if err != nil {
		return nil, err
	}
	manifest, err := client.GetCurrentSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("brain: GET snapshot/current: %w", err)
	}
	// gen=0 sentinel: daemon's vault has never been snapshotted, so
	// there is nothing to download. Caller treats this the same as
	// "no cache, no daemon" — greenfield seed.
	if manifest.Gen == 0 {
		logger.Info(
			"phantom-brain: daemon vault is empty (gen=0); birth will be greenfield",
			slog.String("vault", manifest.Profile+"/"+manifest.Vault),
		)
		return nil, nil
	}

	cacheDir := cfg.SnapshotCacheDir()
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("brain: mkdir snapshot cache: %w", err)
	}
	tarPath := filepath.Join(cacheDir, fmt.Sprintf("snapshot-%d.tar.zst", manifest.Gen))
	tmpPath := tarPath + ".tmp"
	tmp, err := os.Create(tmpPath)
	if err != nil {
		return nil, err
	}
	size, err := client.DownloadSnapshotTarball(ctx, manifest.Gen, manifest.SHA256, tmp)
	_ = tmp.Close()
	if err != nil {
		_ = os.Remove(tmpPath)
		return nil, err
	}
	if err := os.Rename(tmpPath, tarPath); err != nil {
		_ = os.Remove(tmpPath)
		return nil, err
	}

	cs := &CachedSnapshot{
		Gen:         manifest.Gen,
		SHA256:      manifest.SHA256,
		SizeBytes:   size,
		FetchedAt:   nowRFC3339(),
		BuiltAt:     manifest.BuiltAt,
		TarballPath: tarPath,
	}
	metaBytes, _ := json.MarshalIndent(cs, "", "  ")
	metaPath := filepath.Join(cacheDir, fmt.Sprintf("snapshot-%d.manifest.json", manifest.Gen))
	if err := vault.WriteAtomicFile(metaPath, append(metaBytes, '\n'), 0o644); err != nil {
		// Cache metadata is best-effort; the tarball is what matters.
		logger.Warn("phantom-brain: snapshot cache metadata write failed (continuing)",
			slog.String("err", err.Error()))
	}
	logger.Info("phantom-brain: snapshot cached",
		slog.Uint64("gen", manifest.Gen),
		slog.Int64("size_bytes", size),
	)
	return cs, nil
}

// nowRFC3339 returns time.Now().UTC().Format(time.RFC3339). Tiny
// helper to keep callers tidy.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
