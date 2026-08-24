package provider

import (
	"sync"
	"time"
)

type CircuitState string

const (
	CircuitClosed   CircuitState = "CLOSED"
	CircuitOpen     CircuitState = "OPEN"
	CircuitHalfOpen CircuitState = "HALF_OPEN"
)

// CircuitBreakerConfig defines threshold and cooldown timing.
type CircuitBreakerConfig struct {
	FailureThreshold int
	CooldownDuration time.Duration
	SuccessThreshold int
}

// CircuitBreaker manages fault tolerance and cooldown for providers.
type CircuitBreaker struct {
	mu           sync.RWMutex
	cfg          CircuitBreakerConfig
	states       map[string]CircuitState
	failureCount map[string]int
	successCount map[string]int
	lastFailure  map[string]time.Time
}

// NewCircuitBreaker creates a new CircuitBreaker.
func NewCircuitBreaker(cfg CircuitBreakerConfig) *CircuitBreaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 3
	}
	if cfg.CooldownDuration <= 0 {
		cfg.CooldownDuration = 10 * time.Second
	}
	if cfg.SuccessThreshold <= 0 {
		cfg.SuccessThreshold = 2
	}

	return &CircuitBreaker{
		cfg:          cfg,
		states:       make(map[string]CircuitState),
		failureCount: make(map[string]int),
		successCount: make(map[string]int),
		lastFailure:  make(map[string]time.Time),
	}
}

// CanAttempt checks if an invocation can proceed for the provider.
func (cb *CircuitBreaker) CanAttempt(providerID string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	state, ok := cb.states[providerID]
	if !ok || state == CircuitClosed {
		return true
	}

	if state == CircuitOpen {
		last := cb.lastFailure[providerID]
		if time.Since(last) >= cb.cfg.CooldownDuration {
			// Transition to Half-Open to probe health
			cb.states[providerID] = CircuitHalfOpen
			cb.successCount[providerID] = 0
			return true
		}
		return false
	}

	// CircuitHalfOpen allows controlled probe attempts
	return true
}

// RecordSuccess records a successful invocation.
func (cb *CircuitBreaker) RecordSuccess(providerID string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	state := cb.states[providerID]
	if state == CircuitHalfOpen {
		cb.successCount[providerID]++
		if cb.successCount[providerID] >= cb.cfg.SuccessThreshold {
			cb.states[providerID] = CircuitClosed
			cb.failureCount[providerID] = 0
			cb.successCount[providerID] = 0
		}
	} else if state == CircuitClosed {
		cb.failureCount[providerID] = 0
	}
}

// RecordFailure records a failed invocation.
// Non-transient errors (e.g. policy violations) do not trip the circuit breaker.
func (cb *CircuitBreaker) RecordFailure(providerID string, isTransient bool) {
	if !isTransient {
		return
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failureCount[providerID]++
	cb.lastFailure[providerID] = time.Now()

	if cb.states[providerID] == CircuitHalfOpen || cb.failureCount[providerID] >= cb.cfg.FailureThreshold {
		cb.states[providerID] = CircuitOpen
	}
}

// GetState returns the current circuit state for a provider.
func (cb *CircuitBreaker) GetState(providerID string) CircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	state, ok := cb.states[providerID]
	if !ok {
		return CircuitClosed
	}

	if state == CircuitOpen {
		last := cb.lastFailure[providerID]
		if time.Since(last) >= cb.cfg.CooldownDuration {
			return CircuitHalfOpen
		}
	}

	return state
}
