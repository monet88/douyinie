package governance

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/storage"
)

// CredentialResolver is the pluggable seam for resolving and verifying that a backing credential reference
// exists and is authorized in the OS secure credential store / environment.
type CredentialResolver interface {
	ResolveCredential(ctx context.Context, ref domain.CredentialRef) (bool, error)
}

// DefaultCredentialResolver validates safe credential references against OS store / environment rules.
// The default credential-resolution path fails closed unless backing credential existence is actually verifiable.
type DefaultCredentialResolver struct{}

func (r *DefaultCredentialResolver) ResolveCredential(ctx context.Context, ref domain.CredentialRef) (bool, error) {
	if IsPotentialRawSecret(ref.KeyRef) {
		return false, domain.ErrRawSecretForbidden
	}
	key := strings.TrimSpace(ref.KeyRef)
	if key == "" {
		return false, fmt.Errorf("%w: empty backing key reference", domain.ErrAuthRequired)
	}

	storageType := strings.ToLower(strings.TrimSpace(ref.StorageType))
	switch storageType {
	case "env_ref":
		if val, ok := os.LookupEnv(key); ok && strings.TrimSpace(val) != "" {
			return true, nil
		}
		return false, nil
	case "os_credential_store":
		return verifyWindowsCredential(key)
	default:
		// Non-supported platform paths or unknown storage types fail closed
		return false, fmt.Errorf("%w: unsupported storage_type", domain.ErrAuthRequired)
	}
}

// MapCredentialResolver is a deterministic in-memory credential resolver for test injection.
type MapCredentialResolver struct {
	mu    sync.RWMutex
	store map[string]bool
}

// NewMapCredentialResolver creates a new MapCredentialResolver with the given authorized keys.
func NewMapCredentialResolver(authorizedKeys map[string]bool) *MapCredentialResolver {
	store := make(map[string]bool)
	for k, v := range authorizedKeys {
		store[k] = v
	}
	return &MapCredentialResolver{store: store}
}

func (m *MapCredentialResolver) ResolveCredential(ctx context.Context, ref domain.CredentialRef) (bool, error) {
	if IsPotentialRawSecret(ref.KeyRef) {
		return false, domain.ErrRawSecretForbidden
	}
	if strings.TrimSpace(ref.KeyRef) == "" {
		return false, fmt.Errorf("%w: empty backing key reference", domain.ErrAuthRequired)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.store[ref.KeyRef], nil
}

func (m *MapCredentialResolver) Authorize(keyRef string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store[keyRef] = true
}

func (m *MapCredentialResolver) Revoke(keyRef string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.store, keyRef)
}

// CredentialService manages safe OS/environment credential references.
type CredentialService struct {
	mu       sync.RWMutex
	db       *storage.DB
	inMem    map[string]domain.CredentialRef
	resolver CredentialResolver
}

// NewCredentialService creates a new CredentialService with an optional custom resolver seam.
func NewCredentialService(db *storage.DB, resolver ...CredentialResolver) *CredentialService {
	var res CredentialResolver = &DefaultCredentialResolver{}
	if len(resolver) > 0 && resolver[0] != nil {
		res = resolver[0]
	}
	return &CredentialService{
		db:       db,
		inMem:    make(map[string]domain.CredentialRef),
		resolver: res,
	}
}

// SetResolver sets a custom credential resolver seam (e.g. for mock testing or external vault).
func (cs *CredentialService) SetResolver(resolver CredentialResolver) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if resolver != nil {
		cs.resolver = resolver
	}
}

// IsPotentialRawSecret detects if a string looks like an embedded secret rather than a reference name.
func IsPotentialRawSecret(s string) bool {
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "sk-") ||
		strings.HasPrefix(lower, "bearer ") ||
		strings.HasPrefix(lower, "eyjh") || // JWT header
		len(s) > 128 { // Reference keys are short names
		return true
	}
	return false
}

// RegisterCredentialRef stores a safe credential reference.
func (cs *CredentialService) RegisterCredentialRef(ctx context.Context, ref domain.CredentialRef) error {
	if ref.ID == "" {
		ref.ID = uuid.NewString()
	}
	if ref.CreatedAt.IsZero() {
		ref.CreatedAt = time.Now().UTC()
	}

	if IsPotentialRawSecret(ref.KeyRef) {
		return fmt.Errorf("%w: key_ref appears to be a raw secret rather than a storage reference identifier", domain.ErrRawSecretForbidden)
	}

	if strings.TrimSpace(ref.Name) == "" || strings.TrimSpace(ref.KeyRef) == "" {
		return fmt.Errorf("name and key_ref are required")
	}

	storageType := strings.ToLower(strings.TrimSpace(ref.StorageType))
	if storageType == "" {
		storageType = domain.StorageTypeOSCredentialStore
	}
	if storageType != domain.StorageTypeEnvRef && storageType != domain.StorageTypeOSCredentialStore {
		return fmt.Errorf("%w: %q", domain.ErrUnsupportedStorageType, ref.StorageType)
	}
	ref.StorageType = storageType

	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.db != nil {
		if err := cs.db.SaveCredentialRef(ctx, ref); err != nil {
			return fmt.Errorf("save credential ref: %w", err)
		}
	}
	cs.inMem[ref.ID] = ref
	return nil
}

// GetCredentialRef retrieves a credential reference by ID or name.
func (cs *CredentialService) GetCredentialRef(ctx context.Context, idOrName string) (*domain.CredentialRef, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	if ref, ok := cs.inMem[idOrName]; ok {
		return &ref, nil
	}

	// Check name in memory
	for _, ref := range cs.inMem {
		if ref.Name == idOrName {
			return &ref, nil
		}
	}

	if cs.db != nil {
		ref, err := cs.db.GetCredentialRef(ctx, idOrName)
		if err == nil && ref != nil {
			return ref, nil
		}
	}

	return nil, storage.ErrNotFound
}

// ValidateCredentialRef resolves a credential reference and ensures it authorizes the target provider.
// A persisted CredentialRef alone must not prove authorization unless its backing credential reference is validated through the credential-resolver seam.
// Prevents callers from self-authorizing merely by passing a provider ID string.
func (cs *CredentialService) ValidateCredentialRef(ctx context.Context, refIDOrName string, providerID string) (bool, error) {
	if IsPotentialRawSecret(refIDOrName) {
		return false, fmt.Errorf("%w: passing raw secrets in credential references is prohibited", domain.ErrRawSecretForbidden)
	}

	ref, err := cs.GetCredentialRef(ctx, refIDOrName)
	if err != nil {
		return false, fmt.Errorf("%w: credential reference not found", domain.ErrAuthRequired)
	}

	if IsPotentialRawSecret(ref.KeyRef) {
		return false, fmt.Errorf("%w: stored credential reference contains invalid raw secret", domain.ErrRawSecretForbidden)
	}

	// If the credential specifies an authorized ProviderID, verify it matches
	if ref.ProviderID != "" && !strings.EqualFold(ref.ProviderID, providerID) {
		return false, fmt.Errorf("%w: credential reference is bound to another provider", domain.ErrAuthRequired)
	}

	// Validate backing credential reference through the credential-resolver seam
	cs.mu.RLock()
	resolver := cs.resolver
	cs.mu.RUnlock()

	if resolver != nil {
		ok, err := resolver.ResolveCredential(ctx, *ref)
		if err != nil {
			return false, fmt.Errorf("%w: backing credential validation failed: %v", domain.ErrAuthRequired, err)
		}
		if !ok {
			return false, fmt.Errorf("%w: backing credential failed validation in credential store", domain.ErrAuthRequired)
		}
	}

	return true, nil
}

// MaterializeSecret validates a credential reference for a provider and
// returns the backing secret value transiently, for one authorized subprocess
// call. Raw session material never leaves this function except to the
// immediate caller; it must not be logged, persisted, or placed in provenance
// (Issue #17). Callers receive an empty string only when the backing store
// legitimately holds no value.
func (cs *CredentialService) MaterializeSecret(ctx context.Context, refIDOrName string, providerID string) (string, error) {
	ok, err := cs.ValidateCredentialRef(ctx, refIDOrName, providerID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: credential reference validation failed", domain.ErrAuthRequired)
	}
	ref, err := cs.GetCredentialRef(ctx, refIDOrName)
	if err != nil {
		return "", fmt.Errorf("%w: credential reference not found", domain.ErrAuthRequired)
	}

	key := strings.TrimSpace(ref.KeyRef)
	switch strings.ToLower(strings.TrimSpace(ref.StorageType)) {
	case domain.StorageTypeEnvRef:
		return os.Getenv(key), nil
	case domain.StorageTypeOSCredentialStore:
		return readWindowsCredential(key)
	default:
		return "", fmt.Errorf("%w: unsupported storage_type", domain.ErrAuthRequired)
	}
}

// ListCredentialRefs lists all registered credential references.
func (cs *CredentialService) ListCredentialRefs(ctx context.Context) ([]domain.CredentialRef, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	if cs.db != nil {
		return cs.db.ListCredentialRefs(ctx)
	}

	var list []domain.CredentialRef
	for _, v := range cs.inMem {
		list = append(list, v)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Name != list[j].Name {
			return list[i].Name < list[j].Name
		}
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.After(list[j].CreatedAt)
		}
		return list[i].ID < list[j].ID
	})
	return list, nil
}
