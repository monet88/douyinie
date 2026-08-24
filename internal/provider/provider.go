package provider

import (
	"fmt"
	"sync"
)

type ProviderType string

const (
	TypeASR         ProviderType = "asr"
	TypeAligner     ProviderType = "aligner"
	TypeTTS         ProviderType = "tts"
	TypeSeparator   ProviderType = "separator"
	TypeOCR         ProviderType = "ocr"
	TypeTranslation ProviderType = "translation"
)

type PolicyState string

const (
	PolicyAllowed                 PolicyState = "ALLOWED"
	PolicyRequiresExplicitConsent PolicyState = "REQUIRES_EXPLICIT_CONSENT"
	PolicyRequiresAuthorization   PolicyState = "REQUIRES_AUTHORIZATION"
	PolicyBlocked                 PolicyState = "BLOCKED"
)

// Provider represents a model or external service adapter.
type Provider interface {
	ID() string
	Type() ProviderType
	PolicyState() PolicyState
	IsHealthy() bool
}

// Registry maintains available providers indexed by ID and Type.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry creates a new empty provider registry.
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]Provider),
	}
}

// Register registers a provider instance.
func (r *Registry) Register(p Provider) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if p == nil {
		return fmt.Errorf("cannot register nil provider")
	}
	r.providers[p.ID()] = p
	return nil
}

// Get retrieves a provider by ID.
func (r *Registry) Get(id string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	p, ok := r.providers[id]
	return p, ok
}

// ListByType returns all providers registered for a given type.
func (r *Registry) ListByType(t ProviderType) []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []Provider
	for _, p := range r.providers {
		if p.Type() == t {
			result = append(result, p)
		}
	}
	return result
}

// GetDefault returns the first healthy and allowed provider for a type.
func (r *Registry) GetDefault(t ProviderType) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, p := range r.providers {
		if p.Type() == t && p.PolicyState() == PolicyAllowed && p.IsHealthy() {
			return p, true
		}
	}
	return nil, false
}
