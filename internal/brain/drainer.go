package brain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/neverprepared/phantom-brain/internal/brain/wqueue"
)

// DefaultDrainInterval is the drainer's polling cadence.
const DefaultDrainInterval = 30 * time.Second

// DrainOnce performs a single drain pass against the queue using
// client. Returns counts of items sent (deleted) and failed
// (re-queued). Stops on the first context cancellation. Exposed so
// `pbrainctl client queue drain-now` can reuse the same dispatcher.
//
// On success a row is deleted and connectivity.NoteSuccess fires.
// On failure the row is marked (attempt incremented) and
// connectivity.NoteFailure fires. The pass keeps going past failures
// so one bad item doesn't block the rest.
func DrainOnce(ctx context.Context, q *wqueue.Queue, client *Client, conn *Connectivity, logger *slog.Logger) (sent int, failed int, err error) {
	if q == nil || client == nil {
		return 0, 0, nil
	}
	const batch = 16
	now := time.Now()
	items, err := q.NextEligible(ctx, now, batch)
	if err != nil {
		return 0, 0, fmt.Errorf("drainer: NextEligible: %w", err)
	}
	for _, it := range items {
		if ctx.Err() != nil {
			return sent, failed, ctx.Err()
		}
		dispatchErr := dispatch(ctx, client, it)
		now = time.Now()
		if dispatchErr == nil {
			if delErr := q.Delete(ctx, it.ID); delErr != nil && logger != nil {
				logger.Warn("phantom-brain: wqueue delete after success failed",
					slog.Int64("id", it.ID), slog.String("err", delErr.Error()))
			}
			if conn != nil {
				conn.NoteSuccess(now)
			}
			sent++
			continue
		}
		if markErr := q.MarkAttempt(ctx, it.ID, now, dispatchErr); markErr != nil && logger != nil {
			logger.Warn("phantom-brain: wqueue mark attempt failed",
				slog.Int64("id", it.ID), slog.String("err", markErr.Error()))
		}
		if conn != nil {
			conn.NoteFailure(now, dispatchErr)
		}
		failed++
	}
	return sent, failed, nil
}

// dispatch routes one queued item to the appropriate Client method.
// For KindAttach the staged file is re-read and base64-encoded; the
// daemon dedups by SHA so re-sending after a partial success is safe.
func dispatch(ctx context.Context, client *Client, it *wqueue.Item) error {
	switch it.Kind {
	case wqueue.KindPerceive:
		var req PerceiveRequest
		if err := json.Unmarshal(it.PayloadJSON, &req); err != nil {
			return fmt.Errorf("drainer: unmarshal perceive: %w", err)
		}
		_, err := client.Perceive(ctx, req)
		return err
	case wqueue.KindLearn, wqueue.KindTaskPromote:
		var req LearnRequest
		if err := json.Unmarshal(it.PayloadJSON, &req); err != nil {
			return fmt.Errorf("drainer: unmarshal learn: %w", err)
		}
		_, err := client.Learn(ctx, req)
		return err
	case wqueue.KindAttach:
		var req AttachRequest
		if err := json.Unmarshal(it.PayloadJSON, &req); err != nil {
			return fmt.Errorf("drainer: unmarshal attach: %w", err)
		}
		bytes, err := os.ReadFile(it.StagedPath)
		if err != nil {
			return fmt.Errorf("drainer: read staged %s: %w", it.StagedPath, err)
		}
		req.BytesB64 = base64.StdEncoding.EncodeToString(bytes)
		_, err = client.Attach(ctx, req)
		return err
	case wqueue.KindTrace:
		var req TraceRequest
		if err := json.Unmarshal(it.PayloadJSON, &req); err != nil {
			return fmt.Errorf("drainer: unmarshal trace: %w", err)
		}
		return client.Trace(ctx, req)
	default:
		return fmt.Errorf("drainer: unknown kind %q", it.Kind)
	}
}

// runDrainer is the per-Lifecycle background goroutine. Polls every
// DefaultDrainInterval; calls DrainOnce; sweeps orphan staging files
// once per cycle. Exits on ctx cancellation.
func (l *Lifecycle) runDrainer(ctx context.Context) {
	defer close(l.drainDone)
	if l.queue == nil || l.client == nil {
		return
	}
	t := time.NewTicker(DefaultDrainInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, _, err := DrainOnce(ctx, l.queue, l.client, l.conn, l.logger); err != nil && l.logger != nil {
				l.logger.Warn("phantom-brain: wqueue drain pass failed",
					slog.String("err", err.Error()))
			}
			if _, _, err := l.queue.Cleanup(ctx); err != nil && l.logger != nil {
				l.logger.Warn("phantom-brain: wqueue cleanup failed",
					slog.String("err", err.Error()))
			}
		}
	}
}
