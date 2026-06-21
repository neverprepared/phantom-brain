package brain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/neverprepared/mcp-phantom-brain/internal/config"
)

// ShipQueueItem describes one death payload waiting for the daemon to
// pick it up. Exposed to operator tooling (brain_status MCP tool +
// future pbrainctl `queue depth` subcommand) so the size of the
// pending backlog is observable.
type ShipQueueItem struct {
	BrainID     string // owning brain (the parent dir of the payload)
	PayloadPath string
	SizeBytes   int64
}

// ListShipQueue enumerates every death-*.tar under ShipPendingDir()
// across all brain subdirectories. Sorted by path for stable output.
// Returns an empty slice (not error) when the dir doesn't exist —
// that's the steady state for a freshly initialised host.
func ListShipQueue(cfg *config.Agent) ([]ShipQueueItem, error) {
	if cfg == nil {
		return nil, errors.New("brain: ListShipQueue requires a non-nil Agent")
	}
	root := cfg.ShipPendingDir()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("brain: read ship-pending: %w", err)
	}
	var out []ShipQueueItem
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		brainID := e.Name()
		sub := filepath.Join(root, brainID)
		files, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			full := filepath.Join(sub, f.Name())
			st, err := os.Stat(full)
			if err != nil {
				continue
			}
			out = append(out, ShipQueueItem{
				BrainID:     brainID,
				PayloadPath: full,
				SizeBytes:   st.Size(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PayloadPath < out[j].PayloadPath })
	return out, nil
}

// ShipQueueDepthBytes sums the sizes of every pending payload.
// Compared against CL_BRAIN_MAX_PENDING_MB to decide whether the brain
// should refuse new ingests (back-pressure). Phase 1 doesn't enforce
// the cap yet; this helper makes the value available to operators
// inspecting brain_status.
func ShipQueueDepthBytes(cfg *config.Agent) (int64, error) {
	items, err := ListShipQueue(cfg)
	if err != nil {
		return 0, err
	}
	var sum int64
	for _, it := range items {
		sum += it.SizeBytes
	}
	return sum, nil
}

// ShipQueueResult summarises one drain pass. Returned by
// UploadShipQueue so the caller (typically a Lifecycle entrypoint or
// the startup drain in pbrainctl mcp) can log + decide whether to
// retry.
type ShipQueueResult struct {
	// Shipped is the set of payload paths successfully uploaded +
	// completed. These were deleted from disk after the daemon
	// confirmed.
	Shipped []string
	// Skipped is the set of payloads the daemon refused with a known-
	// permanent error (e.g. MERGE_IN_PROGRESS) — the payload stays on
	// disk so an operator can inspect; UploadShipQueue does not
	// re-try them.
	Skipped []string
	// Failed is the set of payloads that hit a transient error
	// (network, timeout, 5xx). They stay on disk for the next call.
	Failed []string
}

// UploadShipQueue iterates every payload under ShipPendingDir() and
// drives the daemon's three-step upload protocol: /merge/init →
// PUT the tarball → /merge/complete. On success the local payload
// is deleted (the daemon now owns it in its own _pending/ tree).
// Errors are categorised into Skipped (permanent — daemon refused
// definitively) vs Failed (transient — caller should retry later).
//
// ctx bounds the entire drain. Honour it from the caller's shutdown
// signal so a slow daemon doesn't block the agent's exit.
func UploadShipQueue(ctx context.Context, cfg *config.Agent, logger *slog.Logger) (*ShipQueueResult, error) {
	if cfg == nil {
		return nil, errors.New("brain: UploadShipQueue requires a non-nil Agent")
	}
	if logger == nil {
		logger = slog.Default()
	}
	items, err := ListShipQueue(cfg)
	if err != nil {
		return nil, err
	}
	res := &ShipQueueResult{}
	if len(items) == 0 {
		return res, nil
	}

	client, cerr := NewClient(ClientOpts{BaseURL: cfg.API, Token: cfg.Token})
	if cerr != nil {
		return res, cerr
	}

	for _, item := range items {
		if err := ctx.Err(); err != nil {
			// Caller bailed (timeout / shutdown). The remaining items
			// stay on disk for the next drain.
			res.Failed = append(res.Failed, item.PayloadPath)
			continue
		}
		shipOne(ctx, client, item, logger, res)
	}
	return res, nil
}

// shipOne runs the full init+upload+complete dance for a single
// payload. Updates the shared ShipQueueResult in place. Splits out
// of UploadShipQueue so the per-item flow is easier to read.
func shipOne(ctx context.Context, client *Client, item ShipQueueItem, logger *slog.Logger, res *ShipQueueResult) {
	f, err := os.Open(item.PayloadPath)
	if err != nil {
		logger.Warn("phantom-brain: ship-queue payload open failed",
			slog.String("path", item.PayloadPath), slog.String("err", err.Error()))
		res.Failed = append(res.Failed, item.PayloadPath)
		return
	}
	defer f.Close()

	init, err := client.InitMerge(ctx, item.BrainID, item.SizeBytes, 600)
	if err != nil {
		// Permanent: daemon refuses with a known terminal code.
		if IsAPIErrorCode(err, "MERGE_IN_PROGRESS") {
			logger.Warn("phantom-brain: ship-queue daemon already has this brain; leaving payload for ops review",
				slog.String("brain_id", item.BrainID), slog.String("path", item.PayloadPath))
			res.Skipped = append(res.Skipped, item.PayloadPath)
			return
		}
		logger.Warn("phantom-brain: ship-queue /merge/init failed (will retry)",
			slog.String("brain_id", item.BrainID), slog.String("err", err.Error()))
		res.Failed = append(res.Failed, item.PayloadPath)
		return
	}

	if _, err := client.UploadTarball(ctx, init.URL, f, item.SizeBytes); err != nil {
		logger.Warn("phantom-brain: ship-queue PUT failed (will retry)",
			slog.String("brain_id", item.BrainID), slog.String("err", err.Error()))
		res.Failed = append(res.Failed, item.PayloadPath)
		return
	}

	if err := client.CompleteMerge(ctx, init.UploadID, item.BrainID); err != nil {
		if IsAPIErrorCode(err, "MERGE_IN_PROGRESS") {
			res.Skipped = append(res.Skipped, item.PayloadPath)
			return
		}
		logger.Warn("phantom-brain: ship-queue /merge/complete failed (will retry)",
			slog.String("brain_id", item.BrainID), slog.String("err", err.Error()))
		res.Failed = append(res.Failed, item.PayloadPath)
		return
	}

	// Daemon owns it now. Delete the local copy so the next drain
	// doesn't re-upload.
	if err := os.Remove(item.PayloadPath); err != nil {
		logger.Warn("phantom-brain: ship succeeded but local cleanup failed",
			slog.String("path", item.PayloadPath), slog.String("err", err.Error()))
		// Still counted as shipped — daemon has it; the leftover file
		// will be retried (which will surface as Skipped /
		// MERGE_IN_PROGRESS) but that's not a correctness issue.
	}
	res.Shipped = append(res.Shipped, item.PayloadPath)
	logger.Info("phantom-brain: shipped death payload",
		slog.String("brain_id", item.BrainID),
		slog.Int64("size_bytes", item.SizeBytes),
	)

	// Tidy up an empty per-brain dir so ListShipQueue doesn't keep
	// returning it on every tick.
	parent := filepath.Dir(item.PayloadPath)
	if entries, _ := os.ReadDir(parent); len(entries) == 0 {
		_ = os.Remove(parent)
	}
}
