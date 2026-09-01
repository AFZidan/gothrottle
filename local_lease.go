// FILENAME: local_lease.go
package gothrottle

import (
	"context"
	"time"
)

// localLease is one reservation held in memory.
type localLease struct {
	weight    int
	expiresAt time.Time
}

// Acquire reserves capacity and returns a renewable lease. It implements
// LeaseDatastore with the same semantics as RedisStore, so switching between
// local and distributed mode does not change observable behavior.
func (ls *LocalStore) Acquire(_ context.Context, limiterID string, weight int, opts Options) (*Lease, time.Duration, error) {
	if err := validateLimiterID(limiterID); err != nil {
		return nil, 0, err
	}
	if weight <= 0 {
		return nil, 0, ErrInvalidWeight
	}
	if opts.MaxConcurrent > 0 && weight > opts.MaxConcurrent {
		return nil, 0, ErrWeightExceedsMax
	}

	token, err := newLeaseToken()
	if err != nil {
		return nil, 0, err
	}

	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.closed {
		return nil, 0, ErrStoreClosed
	}

	now := time.Now()
	state := ls.leaseStateLocked(limiterID)
	state.purgeExpired(now)

	ttl := opts.leaseTTL()
	config := opts.leaseConfig()
	if stored, ok := state.registeredConfig(now); ok && stored != config {
		return nil, 0, configMismatchError(limiterID, config, stored, limiterID)
	}

	if opts.MaxConcurrent > 0 && state.runningWeight()+weight > opts.MaxConcurrent {
		return nil, 0, nil
	}

	if opts.MinTime > 0 && !state.lastStart.IsZero() {
		if elapsed := now.Sub(state.lastStart); elapsed < opts.MinTime {
			return nil, opts.MinTime - elapsed, nil
		}
	}

	expiresAt := now.Add(ttl)
	state.leases[token] = &localLease{weight: weight, expiresAt: expiresAt}
	// Spacing is measured from a job's start, so this is written on admission
	// and never cleared by release, renewal or lease expiry.
	state.lastStart = now
	state.registerConfig(config, now.Add(configLifetime(ttl, opts.MinTime)))

	return &Lease{
		Token:     token,
		LimiterID: limiterID,
		Weight:    weight,
		TTL:       ttl,
		ExpiresAt: expiresAt,
	}, 0, nil
}

// Renew extends a lease. It implements LeaseDatastore.
func (ls *LocalStore) Renew(_ context.Context, lease *Lease) error {
	if lease == nil {
		return ErrNilLease
	}

	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.closed {
		return ErrStoreClosed
	}

	state, exists := ls.leases[lease.LimiterID]
	if !exists {
		return ErrLeaseLost
	}

	now := time.Now()
	state.purgeExpired(now)

	held, exists := state.leases[lease.Token]
	if !exists {
		return ErrLeaseLost
	}

	// Reuse the TTL the lease was created with: renewal extends the window, it
	// does not redefine it.
	held.expiresAt = now.Add(lease.ttlOrDefault())
	lease.ExpiresAt = held.expiresAt
	state.extendConfig(now, lease.ttlOrDefault())

	return nil
}

// Release returns a lease's capacity. It implements LeaseDatastore. Releasing an
// already-reclaimed lease succeeds: only this token is removed, so a stale
// release cannot disturb a newer holder.
func (ls *LocalStore) Release(_ context.Context, lease *Lease) error {
	if lease == nil {
		return ErrNilLease
	}

	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.closed {
		return ErrStoreClosed
	}

	state, exists := ls.leases[lease.LimiterID]
	if !exists {
		return nil
	}

	// Only the reservation goes; lastStart stays, because spacing is measured
	// from when the job started.
	delete(state.leases, lease.Token)
	state.extendConfig(time.Now(), lease.ttlOrDefault())
	return nil
}

// leaseStateLocked returns (creating if needed) the lease state for a limiter.
// Callers must hold ls.mu.
func (ls *LocalStore) leaseStateLocked(limiterID string) *localLeaseState {
	if ls.leases == nil {
		ls.leases = make(map[string]*localLeaseState)
	}
	state, exists := ls.leases[limiterID]
	if !exists {
		state = &localLeaseState{leases: make(map[string]*localLease)}
		ls.leases[limiterID] = state
	}
	return state
}

// localLeaseState holds every live lease for one limiter.
type localLeaseState struct {
	leases map[string]*localLease
	// lastStart is kept separately from the leases so MinTime spacing survives
	// the completion — or the expiry — of the job that set it. Release, renewal
	// and reclamation never touch it.
	lastStart time.Time

	// config is the admission configuration this limiter ID was registered
	// with, and configExpiresAt is when a genuinely idle limiter may be
	// registered with a different one. It mirrors the Redis config key so both
	// stores reject the same conflicting setups.
	config          leaseConfig
	configSet       bool
	configExpiresAt time.Time
}

// registeredConfig returns the configuration in force, if one still is.
func (s *localLeaseState) registeredConfig(now time.Time) (leaseConfig, bool) {
	if !s.configSet || now.After(s.configExpiresAt) {
		return leaseConfig{}, false
	}
	return s.config, true
}

// registerConfig records the configuration and extends its lifetime. The
// lifetime only ever grows, so a short-lived acquisition cannot cut short the
// record protecting a longer-lived one.
func (s *localLeaseState) registerConfig(config leaseConfig, expiresAt time.Time) {
	s.config = config
	s.configSet = true
	if expiresAt.After(s.configExpiresAt) {
		s.configExpiresAt = expiresAt
	}
}

// extendConfig keeps the configuration record alive while a lease is still being
// renewed or released. Like its Redis counterpart it derives the spacing
// component from the stored configuration rather than the caller's, so a client
// with a short LeaseTTL cannot shorten a record protecting a long MinTime.
func (s *localLeaseState) extendConfig(now time.Time, leaseTTL time.Duration) {
	if !s.configSet {
		return
	}
	minTime := time.Duration(s.config.minTimeUS) * time.Microsecond
	if until := now.Add(configLifetime(leaseTTL, minTime)); until.After(s.configExpiresAt) {
		s.configExpiresAt = until
	}
}

// purgeExpired drops leases whose holders are presumed gone. Only reservations
// are purged: spacing history is separate state precisely so a crashed holder's
// expiry cannot hand the next job a free start.
func (s *localLeaseState) purgeExpired(now time.Time) {
	for token, lease := range s.leases {
		if !lease.expiresAt.After(now) {
			delete(s.leases, token)
		}
	}
}

// runningWeight is the total weight of live leases. Summing beats a counter
// here for the same reason as in Redis: it cannot drift away from the
// reservations it represents.
func (s *localLeaseState) runningWeight() int {
	total := 0
	for _, lease := range s.leases {
		total += lease.weight
	}
	return total
}
