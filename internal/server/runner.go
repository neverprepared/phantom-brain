package server

import (
	"context"
	"log/slog"
	"sync"
)

// vaultRunner is the per-(profile, vault) coordination unit. Holds:
//
//   - The shared mutex that orders reaper merges against synthesizer
//     claims. The reaper takes it while landing files into Raw/ +
//     queue/; the synthesizer takes it briefly when claiming the next
//     queue item. Without this lock a synth claim could fire between
//     the reaper's Raw/ write and queue/ append, which would silently
//     drop the new work until the next claim cycle.
//
//   - A cancellable context so SIGHUP can drop a vault cleanly.
//
//   - WaitGroup for graceful drain — Stop() blocks until both goroutines
//     return, so the daemon can call Stop on every runner before
//     releasing the global flock at shutdown.
//
// Phase 2 day 1 stubs out the reaper + synthesizer goroutines; days
// 3-4 fill them in. The skeleton is here so the multi-vault registry
// + lifespan code can wire things end-to-end without later refactor.
type vaultRunner struct {
	Key      VaultKey
	Binding  VaultBinding
	DataDir  DataDir

	mu       sync.Mutex
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopped  bool

	logger *slog.Logger
}

// newVaultRunner spawns the reaper + synthesizer goroutines for one
// vault. Returns a runner whose Stop method drains both before the
// daemon shuts down or before SIGHUP removes the vault.
func newVaultRunner(parentCtx context.Context, binding VaultBinding, dataDir DataDir, logger *slog.Logger) *vaultRunner {
	ctx, cancel := context.WithCancel(parentCtx)
	r := &vaultRunner{
		Key:     binding.Key,
		Binding: binding,
		DataDir: dataDir,
		cancel:  cancel,
		logger:  logger.With(slog.String("vault", binding.Key.String())),
	}
	r.wg.Add(2)
	go r.runReaperLoop(ctx)
	go r.synthesizerLoop(ctx)
	return r
}

// synthesizerLoop body lives in synthesizer.go.

// Stop cancels the runner's context and waits for both goroutines to
// return. Idempotent — a second call is a no-op. Called by the
// registry-reload diff for removed vaults and by the daemon's
// shutdown path.
func (r *vaultRunner) Stop() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	r.mu.Unlock()
	r.cancel()
	r.wg.Wait()
}

// runnerSet holds the live vaultRunners, keyed by VaultKey. Wraps a
// map under an RWMutex so SIGHUP can add/remove entries without
// disrupting concurrent lookups. Used by the daemon's lifespan and
// SIGHUP handler.
type runnerSet struct {
	mu      sync.RWMutex
	runners map[VaultKey]*vaultRunner
}

func newRunnerSet() *runnerSet {
	return &runnerSet{runners: map[VaultKey]*vaultRunner{}}
}

// Keys returns every active runner key, sorted by VaultKey. Used by
// Registry.Diff to compute add/remove sets on SIGHUP.
func (s *runnerSet) Keys() []VaultKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]VaultKey, 0, len(s.runners))
	for k := range s.runners {
		keys = append(keys, k)
	}
	return keys
}

// Add starts a runner for a vault that wasn't running. No-op if a
// runner already exists for the key — SIGHUP shouldn't double-start.
func (s *runnerSet) Add(r *vaultRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runners[r.Key]; ok {
		return
	}
	s.runners[r.Key] = r
}

// Remove stops the runner for a key (if any) and drains its
// goroutines before returning. The remove + stop happen under the
// write lock so a concurrent SIGHUP can't double-stop.
func (s *runnerSet) Remove(k VaultKey) {
	s.mu.Lock()
	r, ok := s.runners[k]
	if ok {
		delete(s.runners, k)
	}
	s.mu.Unlock()
	if ok {
		r.Stop()
	}
}

// StopAll drains every runner. Called once during daemon shutdown
// after the HTTP server has stopped accepting new requests.
func (s *runnerSet) StopAll() {
	s.mu.Lock()
	runners := make([]*vaultRunner, 0, len(s.runners))
	for _, r := range s.runners {
		runners = append(runners, r)
	}
	s.runners = map[VaultKey]*vaultRunner{}
	s.mu.Unlock()
	for _, r := range runners {
		r.Stop()
	}
}
