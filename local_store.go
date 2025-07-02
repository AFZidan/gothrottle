// FILENAME: local_store.go
package gothrottle

import (
	"sync"
	"time"
)

// LocalStore is an in-memory implementation of Datastore.
type LocalStore struct {
	mu     sync.RWMutex
	state  map[string]*LocalState
	closed bool
}

// LocalState holds the state for a single limiter.
type LocalState struct {
	running   int
	lastStart time.Time
}

// NewLocalStore creates a new LocalStore instance.
func NewLocalStore() *LocalStore {
	return &LocalStore{
		state: make(map[string]*LocalState),
	}
}

// Request checks if a job can run according to the limiter's rules.
func (ls *LocalStore) Request(limiterID string, weight int, opts Options) (canRun bool, waitTime time.Duration, err error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.closed {
		return false, 0, ErrStoreClosed
	}

	state, exists := ls.state[limiterID]
	if !exists {
		state = &LocalState{
			running:   0,
			lastStart: time.Time{},
		}
		ls.state[limiterID] = state
	}

	now := time.Now()

	// Check max concurrent limit
	if opts.MaxConcurrent > 0 && state.running+weight > opts.MaxConcurrent {
		return false, 0, nil
	}

	// Check min time between jobs
	if opts.MinTime > 0 && !state.lastStart.IsZero() {
		elapsed := now.Sub(state.lastStart)
		if elapsed < opts.MinTime {
			waitTime = opts.MinTime - elapsed
			return false, waitTime, nil
		}
	}

	// Job can run - update state
	state.running += weight
	state.lastStart = now

	return true, 0, nil
}

// RegisterDone informs the store that a job has finished.
func (ls *LocalStore) RegisterDone(limiterID string, weight int) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.closed {
		return ErrStoreClosed
	}

	state, exists := ls.state[limiterID]
	if !exists {
		return nil // Nothing to do
	}

	state.running -= weight
	if state.running < 0 {
		state.running = 0
	}

	return nil
}

// Disconnect cleans up any connections.
func (ls *LocalStore) Disconnect() error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	ls.closed = true
	ls.state = nil

	return nil
}
