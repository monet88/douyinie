package governance

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/storage"
)

// PolicyService manages policy states and enforces fail-closed routing governance.
type PolicyService struct {
	mu    sync.RWMutex
	db    *storage.DB
	inMem map[string]domain.PolicyState
}

// NewPolicyService creates a new PolicyService.
func NewPolicyService(db *storage.DB) *PolicyService {
	return &PolicyService{
		db:    db,
		inMem: make(map[string]domain.PolicyState),
	}
}

// SetPolicy sets the policy state for a provider.
func (ps *PolicyService) SetPolicy(ctx context.Context, providerID string, state domain.PolicyState, reason string) error {
	if strings.TrimSpace(providerID) == "" {
		return fmt.Errorf("provider_id is required")
	}
	if !state.IsValid() {
		return fmt.Errorf("%w: %q", domain.ErrInvalidPolicyState, state)
	}

	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.db != nil {
		if err := ps.db.SetProviderPolicy(ctx, providerID, state, reason); err != nil {
			return fmt.Errorf("persist provider policy: %w", err)
		}
	}
	ps.inMem[providerID] = state
	return nil
}

// GetPolicy retrieves the effective policy state for a provider.
func (ps *PolicyService) GetPolicy(ctx context.Context, providerID string, defaultState domain.PolicyState) domain.PolicyState {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	if state, ok := ps.inMem[providerID]; ok {
		return state
	}

	if ps.db != nil {
		if state, err := ps.db.GetProviderPolicy(ctx, providerID); err == nil && state != "" {
			return state
		}
	}

	if defaultState != "" {
		return defaultState
	}
	return domain.PolicyBlocked
}

// EvaluateEligibility checks if a provider can be executed based on policy state, consent, and authorization.
func (ps *PolicyService) EvaluateEligibility(ctx context.Context, providerID string, defaultState domain.PolicyState, consentGranted bool, isAuthorized bool) (bool, domain.PolicyState, error) {
	effectiveState := ps.GetPolicy(ctx, providerID, defaultState)

	switch effectiveState {
	case domain.PolicyBlocked:
		return false, effectiveState, domain.ErrPolicyBlocked
	case domain.PolicyRequiresExplicitConsent:
		if !consentGranted {
			return false, effectiveState, domain.ErrConsentRequired
		}
		return true, effectiveState, nil
	case domain.PolicyRequiresAuthorization:
		if !isAuthorized {
			return false, effectiveState, domain.ErrAuthRequired
		}
		return true, effectiveState, nil
	case domain.PolicyAllowed:
		return true, effectiveState, nil
	default:
		return false, effectiveState, domain.ErrPolicyBlocked
	}
}
