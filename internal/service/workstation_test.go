package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/storage"
)

func TestWorkstationShutdownCancellationRemainsRecoverable(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "shutdown_recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	video := domain.DouyinVideo{
		DiscoveredVideo: domain.DiscoveredVideo{
			AwemeID: "7600000000000000150", SourceID: "douyin:aweme:7600000000000000150",
			CanonicalURL: "https://www.douyin.com/video/7600000000000000150",
		},
		FirstObservedAt: now, LastObservedAt: now, Origin: domain.DiscoveryOriginHistorical,
		Disposition: domain.DiscoveryDispositionSeen, AcquisitionRequestState: domain.AcquisitionRequestNone,
		LibraryVisible: true, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := db.QueueDouyinDownloads(context.Background(), []domain.DouyinVideo{video}, []string{video.AwemeID}, "att-safe", "", true, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimNextDouyinDownload(context.Background(), now.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc := &WorkstationService{db: db}
	if err := svc.finishAcquire(ctx, claimed, context.Canceled); err != nil {
		t.Fatal(err)
	}
	interrupted, err := db.GetDouyinVideo(context.Background(), video.AwemeID)
	if err != nil || interrupted.AcquisitionRequestState != domain.AcquisitionRequestDownloading {
		t.Fatalf("shutdown cancellation must remain resumable downloading state: %+v err=%v", interrupted, err)
	}
	if n, err := db.RecoverDouyinDownloads(context.Background(), now.Add(2*time.Millisecond)); err != nil || n != 1 {
		t.Fatalf("restart recovery failed: n=%d err=%v", n, err)
	}
	recovered, _ := db.GetDouyinVideo(context.Background(), video.AwemeID)
	if recovered.AcquisitionRequestState != domain.AcquisitionRequestQueued {
		t.Fatalf("restart must requeue interrupted download, got %s", recovered.AcquisitionRequestState)
	}
}
