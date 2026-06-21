package brain

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/neverprepared/mcp-phantom-brain/internal/config"
)

// DeathOpts collects everything Death() needs. Same shape as BirthOpts
// — explicit dependencies keep the function testable and the call site
// readable.
type DeathOpts struct {
	Agent    *config.Agent
	BrainDir string
	Logger   *slog.Logger
	Now      func() time.Time
}

// DeathResult records the artifacts of a death so the MCP tool can
// echo them to the operator. PayloadPath is local because Phase 1
// doesn't ship to a daemon.
type DeathResult struct {
	BrainID     string
	PayloadPath string // path of the death-<ts>.tar inside ShipPendingDir
	PayloadSize int64
}

// Death transitions a brain from alive to dead and packages a payload
// for the eventual daemon ship.
//
// Sequence (v4.4 §6 trimmed-payload death):
//
//  1. Read the manifest. If status != alive (already dying / dead),
//     reject — death is not idempotent at the API layer; callers must
//     handle the error.
//  2. Flip status to shutting_down, persist.
//  3. tar up vault/Raw/ + vault/Raw/attachments/ + manifest.json into
//     ShipPendingDir()/<brain_id>/death-<unix>.tar. Wiki/ and the
//     working-memory shard are intentionally NOT shipped — only the
//     things that survive synthesis.
//  4. Flip status to dead, persist.
//  5. Log the shipqueue-stub warning so operators see that nothing
//     will leave the host until Phase 2.
//
// Callers MUST have stopped the heartbeat goroutine before calling
// Death — the tar walk would otherwise race the alive-marker touches.
// Tested via TestDeath_LogsDaemonStubWarning.
func Death(opts DeathOpts) (*DeathResult, error) {
	if opts.Agent == nil {
		return nil, errors.New("brain: Death requires a non-nil Agent")
	}
	if opts.BrainDir == "" {
		return nil, errors.New("brain: Death requires a brain directory")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	m, err := ReadManifest(opts.BrainDir)
	if err != nil {
		return nil, fmt.Errorf("brain: read manifest before death: %w", err)
	}
	if m.Status != StatusAlive {
		return nil, fmt.Errorf("brain: cannot die from status=%q (only alive)", m.Status)
	}

	m.Status = StatusShuttingDown
	if err := WriteManifest(opts.BrainDir, m); err != nil {
		return nil, fmt.Errorf("brain: mark shutting_down: %w", err)
	}

	tnow := now().UTC()
	payloadDir := filepath.Join(opts.Agent.ShipPendingDir(), m.BrainID)
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		return nil, fmt.Errorf("brain: mkdir ship-pending: %w", err)
	}
	payloadPath := filepath.Join(payloadDir, fmt.Sprintf("death-%d.tar", tnow.Unix()))
	size, err := packDeathPayload(opts.BrainDir, payloadPath)
	if err != nil {
		return nil, fmt.Errorf("brain: pack death payload: %w", err)
	}

	m.Status = StatusDead
	if err := WriteManifest(opts.BrainDir, m); err != nil {
		// Payload is already on disk — log so the operator can recover
		// even if the manifest flip fails.
		return nil, fmt.Errorf("brain: mark dead (payload at %s): %w", payloadPath, err)
	}

	// Phase 2.5 wired the shipqueue — death payloads now actually ship
	// when UploadShipQueue runs (typically at the next agent startup's
	// recovery drain). Log at INFO; only the failure path is a WARN.
	logger.Info(
		"phantom-brain: death payload written to local ship queue",
		slog.String("brain_id", m.BrainID),
		slog.String("payload", payloadPath),
		slog.Int64("size_bytes", size),
	)
	return &DeathResult{BrainID: m.BrainID, PayloadPath: payloadPath, PayloadSize: size}, nil
}

// packDeathPayload writes a tar containing the manifest plus the
// brain's vault/Raw/ tree (which includes attachments/). Returns the
// final file size on disk so callers can report it.
//
// Wiki/ and _index/ and the wm-<PID>.sqlite shard are intentionally
// excluded — they're either regenerable from Raw or process-local
// scratch. The daemon's synthesizer will re-run the gate over Raw/
// after merge, so shipping Wiki/ would duplicate work and risk a
// stale Wiki overwriting fresher synthesis on the collective.
func packDeathPayload(brainDir, outPath string) (int64, error) {
	out, err := os.Create(outPath)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	tw := tar.NewWriter(out)
	defer tw.Close()

	if err := addFileToTar(tw, brainDir, ManifestFilename); err != nil {
		return 0, err
	}
	rawRoot := filepath.Join(brainDir, "vault", "Raw")
	if err := addTreeToTar(tw, brainDir, rawRoot); err != nil {
		return 0, err
	}
	if err := tw.Close(); err != nil {
		return 0, fmt.Errorf("tar close: %w", err)
	}
	if err := out.Sync(); err != nil {
		return 0, fmt.Errorf("tar fsync: %w", err)
	}
	st, err := out.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// addFileToTar writes a single file rooted at base+rel into the tar
// using rel as the in-archive name. Used for the top-level manifest.
func addFileToTar(tw *tar.Writer, base, rel string) error {
	full := filepath.Join(base, rel)
	st, err := os.Stat(full)
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(st, "")
	if err != nil {
		return err
	}
	hdr.Name = rel
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("copy %s into tar: %w", rel, err)
	}
	return nil
}

// addTreeToTar walks root and writes every regular file under it into
// the tar. Paths are stored relative to base so extraction reconstructs
// the brain dir layout. Returns nil if root doesn't exist (a brain that
// never ingested anything has no Raw/ tree to ship).
func addTreeToTar(tw *tar.Writer, base, root string) error {
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Normalize separators for tar (POSIX-style paths).
		rel = strings.ReplaceAll(rel, string(filepath.Separator), "/")
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}
