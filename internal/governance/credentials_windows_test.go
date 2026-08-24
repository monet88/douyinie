//go:build windows

package governance

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
)

func TestWindowsCredentialStore_RealIntegration(t *testing.T) {
	targetName := "douyinie_test_target_" + uuid.NewString()
	defer deleteWindowsCredential(targetName)

	resolver := &DefaultCredentialResolver{}
	ctx := context.Background()

	ref := domain.CredentialRef{
		ID:          uuid.NewString(),
		Name:        "win_cred_test",
		StorageType: "os_credential_store",
		KeyRef:      targetName,
		CreatedAt:   time.Now().UTC(),
	}

	// 1. Target not written in Windows Credential Manager -> fails closed (false, nil)
	ok, err := resolver.ResolveCredential(ctx, ref)
	if err != nil {
		t.Fatalf("unexpected error resolving missing cred: %v", err)
	}
	if ok {
		t.Errorf("expected missing credential to return false")
	}

	// 2. Write credential to Windows Credential Manager -> resolves true
	if err := writeWindowsCredential(targetName, []byte("super_secret_blob")); err != nil {
		t.Fatalf("writeWindowsCredential failed: %v", err)
	}

	ok, err = resolver.ResolveCredential(ctx, ref)
	if err != nil {
		t.Fatalf("resolve existing cred error: %v", err)
	}
	if !ok {
		t.Errorf("expected existing Windows credential to resolve true")
	}

	// 3. Delete credential -> resolves false again
	if err := deleteWindowsCredential(targetName); err != nil {
		t.Fatalf("deleteWindowsCredential failed: %v", err)
	}

	ok, err = resolver.ResolveCredential(ctx, ref)
	if err != nil || ok {
		t.Errorf("expected deleted credential to return false, got ok=%v, err=%v", ok, err)
	}
}
