package server

import (
	"context"
	"log/slog"
	"sync"
)

// vaultRunner is the per-(profile, vault) coordination unit. Phase 6
// shrank its role: with the file-queue reaper + synthesizer gone, it
// holds the binding + a cancellable context so SIGHUP can drop a
// vault cleanly. No goroutines run from here anymore — the
// daemon-level SynthWorker handles synthesis for every vault.
//
// The shared mutex is preserved so Day 8's deletion doesn't have to
// chase down every test fixture that takes &r.mu; once the test
// surface settles in Day 9/10 it can come out.
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

// newVaultRunner constructs a runner for one vault. No goroutines
// are spawned — the daemon's SynthWorker covers what the per-vault
// reaper + synthesizer used to do.
func newVaultRunner(parentCtx context.Context, binding VaultBinding, dataDir DataDir, logger *slog.Logger) *vaultRunner {
	_, cancel := context.WithCancel(parentCtx)
	return &vaultRunner{
		Key:     binding.Key,
		Binding: binding,
		DataDir: dataDir,
		cancel:  cancel,
		logger:  logger.With(slog.String("vault", binding.Key.String())),
	}
}

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
