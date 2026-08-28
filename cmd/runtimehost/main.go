package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/queue"
	"github.com/monet88/douyinie/internal/scheduler"
	"github.com/monet88/douyinie/internal/server"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
	"github.com/monet88/douyinie/internal/worker"
)

func main() {
	var (
		port        = flag.Int("port", 8080, "HTTP server listening port")
		dataDir     = flag.String("data-dir", "./data", "Directory for SQLite database and CAS storage")
		ffprobePath = flag.String("ffprobe", "ffprobe", "Path to ffprobe executable")
		demoFile    = flag.String("demo-file", "", "Optional local media file to execute demo ingestion pipeline on startup")
	)
	flag.Parse()

	absDataDir, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("failed to resolve data dir: %v", err)
	}

	log.Printf("[RuntimeHost] Initializing Douyinie Control Plane (data: %s)...", absDataDir)

	// 1. Initialize CAS Store
	casStore, err := cas.NewStore(absDataDir)
	if err != nil {
		log.Fatalf("failed to initialize CAS store: %v", err)
	}

	// 2. Initialize SQLite Database
	dbPath := filepath.Join(absDataDir, "douyinie.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		log.Fatalf("failed to initialize SQLite database: %v", err)
	}
	defer db.Close()

	// 3. Initialize Media Prober & Ingest Service
	prober := media.NewFFprobeProber(*ffprobePath)
	ingestSvc := service.NewIngestService(db, casStore, prober)

	// 4. Initialize Persisted Queue + ResourceScheduler + GPU Lease Manager + Crash Recovery
	queueSvc := queue.NewService(db)
	resScheduler := scheduler.New()
	gpuLeaseMgr := worker.NewGPULeaseManager(resScheduler)

	// 4b. Initialize Speech Understanding service (T08)
	speechSvc := service.NewSpeechService(db, casStore)
	recovered, err := queueSvc.Recover(context.Background())
	if err != nil {
		log.Fatalf("[RuntimeHost] crash recovery failed: %v", err)
	}
	if len(recovered) > 0 {
		log.Printf("[RuntimeHost] Crash recovery: %d interrupted run(s): %v", len(recovered), recovered)
	}

	// 5. Initialize Provider Registry with Governance (Issue #44 Finding 1)
	// Production registers concrete worker-backed providers (Qwen3-ASR 1.7B quality,
	// Qwen3-ASR 0.6B fallback, Qwen3-ForcedAligner, and conditional Diarization)
	// wired through the authoritative single-GPU lease path. No fake providers are registered.
	reg, err := provider.NewProductionSpeechRegistry(gpuLeaseMgr)
	if err != nil {
		log.Fatalf("[RuntimeHost] failed to initialize production speech registry: %v", err)
	}
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)
	router := provider.NewRouter(reg, polSvc, licSvc, credSvc, nil, db)
	// 6. Optional Demo Ingestion
	if *demoFile != "" {
		log.Printf("[RuntimeHost] Running DEMO ingestion on: %s", *demoFile)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		demoReq := service.IngestRequest{
			FilePath: *demoFile,
			Attestation: &domain.RightsAttestation{
				AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
				DeclaredBy:      "runtimehost-demo-operator",
				TermsAccepted:   true,
				Notes:           "Demo file ingestion from CLI flag",
			},
		}

		res, err := ingestSvc.IngestLocalFile(ctx, demoReq)
		if err != nil {
			log.Printf("[RuntimeHost] DEMO ingest failed: %v", err)
		} else {
			log.Printf("[RuntimeHost] DEMO Ingest Success!")
			log.Printf("  ├── Asset ID:   %s", res.Asset.ID)
			log.Printf("  ├── SHA-256:    %s", res.Asset.SHA256)
			log.Printf("  ├── CAS Path:   %s", res.Asset.CASPath)
			log.Printf("  ├── Duration:   %.2fs (%d ms)", res.PreflightReport.DurationSec, res.PreflightReport.DurationMs)
			log.Printf("  ├── Resolution: %dx%d (%s, %.2f fps)", res.PreflightReport.Width, res.PreflightReport.Height, res.PreflightReport.VideoCodec, res.PreflightReport.FrameRate)
			log.Printf("  └── Audio:      %s (%d channels, %d Hz)", res.PreflightReport.AudioCodec, res.PreflightReport.AudioChannels, res.PreflightReport.AudioSampleRate)
		}
	}

	// 7. Start HTTP Server
	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	srv := server.New(server.Config{
		Addr:       addr,
		DB:         db,
		CASStore:   casStore,
		Ingest:     ingestSvc,
		Registry:   reg,
		PolicySvc:  polSvc,
		LicenseSvc: licSvc,
		CredSvc:    credSvc,
		Router:     router,
		QueueSvc:   queueSvc,
		Scheduler:  resScheduler,
		SpeechSvc:  speechSvc,
	})

	go func() {
		log.Printf("[RuntimeHost] API daemon listening on http://%s", addr)
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[RuntimeHost] Server failed: %v", err)
		}
	}()

	// Graceful shutdown on signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Printf("[RuntimeHost] Shutting down gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[RuntimeHost] Graceful shutdown error: %v", err)
	}
	log.Printf("[RuntimeHost] Stopped.")
}
