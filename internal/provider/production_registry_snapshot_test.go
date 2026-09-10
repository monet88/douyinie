package provider

import (
	"path/filepath"
	"testing"

	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/storage"
)

func TestProductionSpeechRegistry_BindsSnapshotServiceToLocalWorkerProviders(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "registry-snapshot.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)
	reg, err := NewProductionSpeechRegistry(snapSvc)
	if err != nil {
		t.Fatalf("NewProductionSpeechRegistry: %v", err)
	}

	for _, p := range reg.ListAll() {
		if p.Capability().ExecutionTier != "local" {
			continue
		}
		base, ok := localWorkerProviderBase(p)
		if !ok {
			t.Fatalf("local production provider %s is not worker-backed", p.ID())
		}
		if base.snapshotSvc != snapSvc {
			t.Errorf("provider %s snapshot service = %p, want %p", p.ID(), base.snapshotSvc, snapSvc)
		}
	}
}

func localWorkerProviderBase(p Provider) (*workerProviderBase, bool) {
	switch v := p.(type) {
	case *WorkerASRProvider:
		return &v.workerProviderBase, true
	case *WorkerAlignerProvider:
		return &v.workerProviderBase, true
	case *WorkerDiarizationProvider:
		return &v.workerProviderBase, true
	case *WorkerTTSProvider:
		return &v.workerProviderBase, true
	case *WorkerSeparatorProvider:
		return &v.workerProviderBase, true
	case *WorkerOCRProvider:
		return &v.workerProviderBase, true
	case *WorkerTranslationProvider:
		return &v.workerProviderBase, true
	case *WorkerAudioRoleProvider:
		return &v.workerProviderBase, true
	default:
		return nil, false
	}
}
