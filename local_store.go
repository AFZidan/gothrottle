// FILENAME: local_store.go
package gothrottle

import (
	"sync"
	"time"
)

// LocalStore is an in-memory implementation of Datastore and LeaseDatastore.
type LocalStore struct {
	mu     sync.RWMutex
	state  map[string]*LocalState
	leases map[string]*localLeaseState
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
		state:  make(map[string]*LocalState),
		leases: make(map[string]*localLeaseState),
	}
}

// Request checks if a job can run according to the limiter's rules.
func (ls *LocalStore) Request(limiterID string, weight int, opts Options) (canRun bool, waitTime time.Duration, err error) {
	if err := validateAdmission(limiterID, weight, opts); err != nil {
		return false, 0, err
	}

	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.closed {
		return false, 0, ErrStoreClosed
	}

	state, exists := ls.state[limiterID]
	if !exists {
		state = &LocalState{}
		ls.state[limiterID] = state
	}

	now := time.Now()

	// Written as a subtraction against the limit rather than summing first: on a
	// 32-bit build running+weight can exceed int even when both are individually
	// valid, and the wrap would turn a refusal into an admission.
	if !fitsWithin(state.running, weight, opts.MaxConcurrent) {
		return false, 0, nil
	}

	if opts.MinTime > 0 && !state.lastStart.IsZero() {
		elapsed := now.Sub(state.lastStart)
		if elapsed < opts.MinTime {
			waitTime = opts.MinTime - elapsed
			return false, waitTime, nil
		}
	}

	state.running = addClamped(state.running, weight)
	state.lastStart = now

	return true, 0, nil
}

// RegisterDone informs the store that a job has finished.
func (ls *LocalStore) RegisterDone(limiterID string, weight int) error {
	if err := validateCompletion(limiterID, weight); err != nil {
		return err
	}

	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.closed {
		return ErrStoreClosed
	}

	state, exists := ls.state[limiterID]
	if !exists {
		// Nothing was ever admitted under this ID, so there is no reservation to
		// return. Reporting an error would only invite a retry that cannot help.
		return nil
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
	ls.leases = nil

	return nil
}
