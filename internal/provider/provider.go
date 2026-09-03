package provider

import (
	"fmt"
	"sort"
	"sync"

	"github.com/monet88/douyinie/internal/domain"
)

type ProviderType string

const (
	TypeASR         ProviderType = "asr"
	TypeAligner     ProviderType = "aligner"
	TypeDiarizer    ProviderType = "diarizer"
	TypeTTS         ProviderType = "tts"
	TypeSeparator   ProviderType = "separator"
	TypeOCR         ProviderType = "ocr"
	TypeTranslation ProviderType = "translation"
)

// Alias for domain.PolicyState
type PolicyState = domain.PolicyState

const (
	PolicyAllowed                 = domain.PolicyAllowed
	PolicyRequiresExplicitConsent = domain.PolicyRequiresExplicitConsent
	PolicyRequiresAuthorization   = domain.PolicyRequiresAuthorization
	PolicyBlocked                 = domain.PolicyBlocked
)

// Provider represents a model or external service adapter.
type Provider interface {
	ID() string
	Type() ProviderType
	PolicyState() PolicyState
	IsHealthy() bool
	Capability() domain.ProviderCapability
	ModelInfo() (name string, version string)
}

// ModelDependency declares an additional model checkpoint dependency used by a provider (e.g. VAD for Diarizer).
type ModelDependency struct {
	Name    string
	Version string
	Role    string
}

// DependentModelProvider is an optional interface for providers that depend on additional checkpoints.
type DependentModelProvider interface {
	ModelDependencies() []ModelDependency
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

// ListAll returns all registered providers in deterministic ID order.
func (r *Registry) ListAll() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []Provider
	for _, p := range r.providers {
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID() < result[j].ID()
	})
	return result
}

// ListByType returns all providers registered for a given type in deterministic ID order.
func (r *Registry) ListByType(t ProviderType) []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []Provider
	for _, p := range r.providers {
		if p.Type() == t {
			result = append(result, p)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID() < result[j].ID()
	})
	return result
}

// GetDefault returns the first healthy and allowed provider for a type in deterministic ID order.
func (r *Registry) GetDefault(t ProviderType) (Provider, bool) {
	all := r.ListByType(t)
	for _, p := range all {
		if p.PolicyState() == domain.PolicyAllowed && p.IsHealthy() {
			return p, true
		}
	}
	return nil, false
}

// SetRequireSnapshots sets snapshot requirement on all registered providers that implement SetRequiresSnapshot.
func (r *Registry) SetRequireSnapshots(require bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.providers {
		if sc, ok := p.(interface{ SetRequiresSnapshot(bool) }); ok {
			sc.SetRequiresSnapshot(require)
		}
	}
}
