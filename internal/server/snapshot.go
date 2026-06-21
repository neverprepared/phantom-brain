package server

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/neverprepared/mcp-phantom-brain/internal/brain"
	"github.com/neverprepared/mcp-phantom-brain/internal/vault"
)

// genCounterFilename holds the monotonic snapshot generation number,
// written atomically every time the builder bumps it.
const genCounterFilename = ".gen-counter"

// SnapshotManifest is the JSON sidecar published alongside every
// snapshot tarball. Brains read it during birth to verify the
// payload matches what they claimed and to know which (profile,
// vault) they're attaching to.
type SnapshotManifest struct {
	Profile           string `json:"profile"`
	Vault             string `json:"vault"`
	Gen               uint64 `json:"gen"`
	SHA256            string `json:"sha256"`
	SizeBytes         int64  `json:"size_bytes"`
	BuiltAt           string `json:"built_at"` // RFC3339
	ParentSynthesisID string `json:"parent_synthesis_id,omitempty"`
}

// SnapshotInfo summarises a published snapshot for brain consumption
// and ops tooling. Includes the tarball path so callers can serve it.
type SnapshotInfo struct {
	Manifest    SnapshotManifest
	TarballPath string
}

// ReadGenCounter returns the current .gen-counter for a vault. Zero
// (with nil error) when the file doesn't exist — first call sees a
// fresh collective with no snapshots yet.
func ReadGenCounter(dataDir DataDir, profile, vaultName string) (uint64, error) {
	path := filepath.Join(dataDir.IndexDir(profile, vaultName), genCounterFilename)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("server: read gen counter: %w", err)
	}
	s := strings.TrimSpace(string(raw))
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("server: parse gen counter %q: %w", s, err)
	}
	return n, nil
}

// writeGenCounter atomically persists n. Uses vault.WriteAtomicFile
// so a crash mid-update can't leave the counter half-written.
func writeGenCounter(dataDir DataDir, profile, vaultName string, n uint64) error {
	path := filepath.Join(dataDir.IndexDir(profile, vaultName), genCounterFilename)
	return vault.WriteAtomicFile(path, []byte(strconv.FormatUint(n, 10)+"\n"), 0o644)
}

// BuildSnapshot produces the next snapshot for (profile, vaultName) per
// v4.4 §3 staged-first protocol:
//
//  1. Allocate next gen = current + 1.
//  2. Create staged/snapshot-<gen>/ and reflink the vault/Wiki tree
//     into it (CoW where supported; copy fallback).
//  3. Reflink/copy _index/vectors.db into staged for snapshot-time
//     immutability. (Phase 5 will switch to SQLite Backup API for
//     point-in-time consistency under active writes.)
//  4. Stream-tar staged/snapshot-<gen>/ into _published/snapshot-
//     <gen>.tar.zst (temp + rename).
//  5. Compute SHA256, write .sha256 + .manifest.json sidecars.
//  6. Atomically bump .gen-counter to <gen>.
//  7. Prune snapshots older than retention_gens, EXCEPT any whose
//     staged/snapshot-<gen>/.claims/ dir is non-empty (a brain
//     claimed it and might still be mid-birth — pruning would race).
//
// Caller holds the vaultRunner mutex so concurrent reaper landings
// don't interleave with the reflink.
func BuildSnapshot(dataDir DataDir, profile, vaultName string, retentionGens int) (*SnapshotInfo, error) {
	if retentionGens <= 0 {
		retentionGens = 30
	}
	current, err := ReadGenCounter(dataDir, profile, vaultName)
	if err != nil {
		return nil, err
	}
	next := current + 1

	stagedRoot := dataDir.StagedDir(profile, vaultName)
	stagedDir := filepath.Join(stagedRoot, fmt.Sprintf("snapshot-%d", next))
	if err := os.MkdirAll(stagedRoot, 0o755); err != nil {
		return nil, fmt.Errorf("server: mkdir staged root: %w", err)
	}
	if _, err := os.Stat(stagedDir); err == nil {
		// Leftover from a prior crashed build — remove and retry. The
		// .claims/ subdir is also wiped, but at gen=next there cannot
		// be any claims yet (no brain has seen this gen).
		if err := os.RemoveAll(stagedDir); err != nil {
			return nil, fmt.Errorf("server: clean stale staged: %w", err)
		}
	}

	// Reflink Wiki + _index into the staged tree. Reflink + copy
	// fallback is provided by internal/brain (cross-platform).
	src := dataDir.VaultDir(profile, vaultName)
	if _, err := os.Stat(filepath.Join(src, "Wiki")); errors.Is(err, os.ErrNotExist) {
		// Brand-new vault with no Wiki yet — staged gets an empty
		// Wiki/ so brain extracts don't have to special-case the
		// absent dir. _index also gets a placeholder.
		if err := os.MkdirAll(filepath.Join(stagedDir, "vault", "Wiki"), 0o755); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Join(stagedDir, "_index"), 0o755); err != nil {
			return nil, err
		}
	} else {
		if err := brain.ReflinkOrCopyTree(filepath.Join(src, "Wiki"), filepath.Join(stagedDir, "vault", "Wiki")); err != nil {
			return nil, fmt.Errorf("server: reflink Wiki: %w", err)
		}
		// _index is optional — a fresh vault may not have one yet.
		indexSrc := dataDir.IndexDir(profile, vaultName)
		if _, err := os.Stat(indexSrc); err == nil {
			if err := brain.ReflinkOrCopyTree(indexSrc, filepath.Join(stagedDir, "_index")); err != nil {
				return nil, fmt.Errorf("server: reflink _index: %w", err)
			}
		}
	}

	// Empty .claims/ dir is created at staging time so brain claims
	// (Phase 2.5 birth flow) have a destination.
	if err := os.MkdirAll(filepath.Join(stagedDir, ".claims"), 0o755); err != nil {
		return nil, err
	}

	publishedDir := dataDir.PublishedDir(profile, vaultName)
	if err := os.MkdirAll(publishedDir, 0o755); err != nil {
		return nil, err
	}
	tarballPath := filepath.Join(publishedDir, fmt.Sprintf("snapshot-%d.tar.zst", next))
	sha, size, err := writeStagedTarZst(stagedDir, tarballPath)
	if err != nil {
		return nil, err
	}

	manifest := SnapshotManifest{
		Profile:   profile,
		Vault:     vaultName,
		Gen:       next,
		SHA256:    sha,
		SizeBytes: size,
		BuiltAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeSnapshotSidecars(publishedDir, next, manifest); err != nil {
		return nil, err
	}

	// Bump counter LAST so a partial build doesn't advertise itself.
	if err := writeGenCounter(dataDir, profile, vaultName, next); err != nil {
		return nil, err
	}

	if err := pruneSnapshots(dataDir, profile, vaultName, retentionGens); err != nil {
		// Pruning failure does NOT invalidate the new snapshot.
		// Caller logs; we still return the SnapshotInfo.
		return &SnapshotInfo{Manifest: manifest, TarballPath: tarballPath}, fmt.Errorf("server: snapshot prune: %w", err)
	}
	return &SnapshotInfo{Manifest: manifest, TarballPath: tarballPath}, nil
}

// writeStagedTarZst tars + zstd-compresses stagedDir's contents into
// outPath using a temp file + rename. Returns the SHA256 hex and
// size of the final tarball.
func writeStagedTarZst(stagedDir, outPath string) (string, int64, error) {
	tmp := outPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", 0, err
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}

	hash := sha256.New()
	mw := io.MultiWriter(f, hash)

	zw, err := zstd.NewWriter(mw, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		cleanup()
		return "", 0, fmt.Errorf("server: zstd writer: %w", err)
	}
	tw := tar.NewWriter(zw)

	if err := addStagedTreeToTar(tw, stagedDir); err != nil {
		_ = tw.Close()
		_ = zw.Close()
		cleanup()
		return "", 0, err
	}
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		cleanup()
		return "", 0, fmt.Errorf("server: tar close: %w", err)
	}
	if err := zw.Close(); err != nil {
		cleanup()
		return "", 0, fmt.Errorf("server: zstd close: %w", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return "", 0, fmt.Errorf("server: fsync tarball: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", 0, fmt.Errorf("server: close tarball: %w", err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		_ = os.Remove(tmp)
		return "", 0, fmt.Errorf("server: rename tarball: %w", err)
	}
	st, err := os.Stat(outPath)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), st.Size(), nil
}

// addStagedTreeToTar walks stagedDir and writes every regular file +
// directory entry into the tar with paths relative to stagedDir.
// Skips the .claims/ subtree — those are local-only birth markers,
// not part of the brain-visible payload.
func addStagedTreeToTar(tw *tar.Writer, stagedDir string) error {
	return filepath.Walk(stagedDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stagedDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == ".claims" || strings.HasPrefix(rel, ".claims"+string(filepath.Separator)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = strings.ReplaceAll(rel, string(filepath.Separator), "/")
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, src)
		_ = src.Close()
		return err
	})
}

// writeSnapshotSidecars writes the .sha256 and .manifest.json files
// alongside snapshot-<gen>.tar.zst.
func writeSnapshotSidecars(publishedDir string, gen uint64, m SnapshotManifest) error {
	shaPath := filepath.Join(publishedDir, fmt.Sprintf("snapshot-%d.tar.zst.sha256", gen))
	if err := vault.WriteAtomicFile(shaPath, []byte(m.SHA256+"\n"), 0o644); err != nil {
		return err
	}
	manifestPath := filepath.Join(publishedDir, fmt.Sprintf("snapshot-%d.manifest.json", gen))
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return vault.WriteAtomicFile(manifestPath, append(body, '\n'), 0o644)
}

// LoadSnapshotManifest reads the JSON sidecar for a given gen. Used
// by the API handlers and the prune logic.
func LoadSnapshotManifest(dataDir DataDir, profile, vaultName string, gen uint64) (*SnapshotManifest, error) {
	path := filepath.Join(dataDir.PublishedDir(profile, vaultName), fmt.Sprintf("snapshot-%d.manifest.json", gen))
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m SnapshotManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("server: parse %s: %w", path, err)
	}
	return &m, nil
}

// CurrentSnapshot returns the most recently built snapshot for the
// vault, or (nil, nil) if the vault has never been snapshotted.
func CurrentSnapshot(dataDir DataDir, profile, vaultName string) (*SnapshotInfo, error) {
	gen, err := ReadGenCounter(dataDir, profile, vaultName)
	if err != nil {
		return nil, err
	}
	if gen == 0 {
		return nil, nil
	}
	m, err := LoadSnapshotManifest(dataDir, profile, vaultName, gen)
	if err != nil {
		return nil, err
	}
	return &SnapshotInfo{
		Manifest:    *m,
		TarballPath: filepath.Join(dataDir.PublishedDir(profile, vaultName), fmt.Sprintf("snapshot-%d.tar.zst", gen)),
	}, nil
}

// listPublishedGens returns every snapshot gen with all three files
// present (tarball, sha256, manifest), sorted ascending.
func listPublishedGens(dataDir DataDir, profile, vaultName string) ([]uint64, error) {
	entries, err := os.ReadDir(dataDir.PublishedDir(profile, vaultName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	have := map[uint64]int{}
	for _, e := range entries {
		name := e.Name()
		var n uint64
		switch {
		case strings.HasSuffix(name, ".tar.zst"):
			fmt.Sscanf(strings.TrimSuffix(name, ".tar.zst"), "snapshot-%d", &n)
		case strings.HasSuffix(name, ".sha256"):
			fmt.Sscanf(strings.TrimSuffix(name, ".tar.zst.sha256"), "snapshot-%d", &n)
		case strings.HasSuffix(name, ".manifest.json"):
			fmt.Sscanf(strings.TrimSuffix(name, ".manifest.json"), "snapshot-%d", &n)
		default:
			continue
		}
		if n > 0 {
			have[n]++
		}
	}
	var out []uint64
	for gen, count := range have {
		if count >= 3 {
			out = append(out, gen)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// PruneSnapshots is the exported entry point operator tooling reaches
// for. Same behaviour as the internal pruneSnapshots; kept as a
// wrapper so the package-internal call sites don't have to change.
func PruneSnapshots(dataDir DataDir, profile, vaultName string, retentionGens int) error {
	return pruneSnapshots(dataDir, profile, vaultName, retentionGens)
}

// pruneSnapshots removes published snapshots older than retentionGens
// EXCEPT those whose staged/.claims/ dir is non-empty (dual-source pin:
// a brain claimed that gen and might still be mid-birth).
func pruneSnapshots(dataDir DataDir, profile, vaultName string, retentionGens int) error {
	gens, err := listPublishedGens(dataDir, profile, vaultName)
	if err != nil {
		return err
	}
	if len(gens) <= retentionGens {
		return nil
	}
	cutoff := len(gens) - retentionGens
	for _, gen := range gens[:cutoff] {
		if pinned, _ := genHasClaims(dataDir, profile, vaultName, gen); pinned {
			continue
		}
		_ = os.Remove(filepath.Join(dataDir.PublishedDir(profile, vaultName), fmt.Sprintf("snapshot-%d.tar.zst", gen)))
		_ = os.Remove(filepath.Join(dataDir.PublishedDir(profile, vaultName), fmt.Sprintf("snapshot-%d.tar.zst.sha256", gen)))
		_ = os.Remove(filepath.Join(dataDir.PublishedDir(profile, vaultName), fmt.Sprintf("snapshot-%d.manifest.json", gen)))
		_ = os.RemoveAll(filepath.Join(dataDir.StagedDir(profile, vaultName), fmt.Sprintf("snapshot-%d", gen)))
	}
	return nil
}

// genHasClaims reports whether staged/snapshot-<gen>/.claims/ has any
// entries. A non-empty dir means at least one brain has claimed this
// gen and the prune path must skip it.
func genHasClaims(dataDir DataDir, profile, vaultName string, gen uint64) (bool, error) {
	dir := filepath.Join(dataDir.StagedDir(profile, vaultName), fmt.Sprintf("snapshot-%d", gen), ".claims")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}
