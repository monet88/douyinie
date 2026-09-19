package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/monet88/douyinie/internal/cas"
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

func findAvailablePort(preferred int) int {
	addr := fmt.Sprintf("127.0.0.1:%d", preferred)
	l, err := net.Listen("tcp", addr)
	if err == nil {
		_ = l.Close()
		return preferred
	}
	l, err = net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		p := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		return p
	}
	return preferred
}

func resolveDefaultDataDir() string {
	if dir := os.Getenv("DOUYINIE_DATA_DIR"); dir != "" {
		return dir
	}
	configDir, err := os.UserConfigDir()
	if err == nil && configDir != "" {
		appDir := filepath.Join(configDir, "Douyinie", "data")
		if err := os.MkdirAll(appDir, 0755); err == nil {
			return appDir
		}
	}
	return "./data"
}

func main() {
	var (
		portFlag    = flag.Int("port", 0, "Optional HTTP server listening port (defaults to auto-allocated)")
		dataDirFlag = flag.String("data-dir", "", "Directory for SQLite database and CAS storage")
		ffprobePath = flag.String("ffprobe", "ffprobe", "Path to ffprobe executable")
	)
	flag.Parse()

	targetDataDir := *dataDirFlag
	if targetDataDir == "" {
		targetDataDir = resolveDefaultDataDir()
	}
	absDataDir, err := filepath.Abs(targetDataDir)
	if err != nil {
		log.Fatalf("[Desktop] failed to resolve data dir: %v", err)
	}
	if err := os.MkdirAll(absDataDir, 0755); err != nil {
		log.Fatalf("[Desktop] failed to create data dir: %v", err)
	}

	listenPort := *portFlag
	if listenPort <= 0 {
		listenPort = findAvailablePort(8080)
	}

	log.Printf("[Desktop] Initializing Douyinie RuntimeHost (port: %d, data: %s)...", listenPort, absDataDir)

	// 1. CAS & Storage
	casStore, err := cas.NewStore(absDataDir)
	if err != nil {
		log.Fatalf("[Desktop] CAS init failed: %v", err)
	}
	dbPath := filepath.Join(absDataDir, "douyinie.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		log.Fatalf("[Desktop] SQLite init failed: %v", err)
	}
	defer db.Close()

	// 2. Core services
	prober := media.NewFFprobeProber(*ffprobePath)
	ingestSvc := service.NewIngestService(db, casStore, prober)
	queueSvc := queue.NewService(db)
	resScheduler := scheduler.New()
	gpuLeaseMgr := worker.NewGPULeaseManager(resScheduler)
	speechSvc := service.NewSpeechService(db, casStore)
	translationSvc := service.NewTranslationService(db, casStore)
	dubbingSvc := service.NewDubbingService(db, casStore)
	audioMixSvc := service.NewAudioMixService(db, casStore)
	visualTextSvc := service.NewVisualTextService(db, casStore)
	renderSvc := service.NewRenderService(db, casStore)

	// Crash recovery
	if recovered, err := queueSvc.Recover(context.Background()); err == nil && len(recovered) > 0 {
		log.Printf("[Desktop] Recovered %d interrupted run(s): %v", len(recovered), recovered)
	}

	// 3. Governance & Registry
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)
	_ = provider.BootstrapYAMNetLicenseManifest(context.Background(), licSvc)

	reg, err := provider.NewProductionSpeechRegistry(gpuLeaseMgr, snapSvc, credSvc.MaterializeSecret)
	if err != nil {
		log.Fatalf("[Desktop] speech registry init failed: %v", err)
	}
	router := provider.NewRouter(reg, polSvc, licSvc, credSvc, nil, db)
	router.SetSnapshotService(snapSvc)
	acquisitionSvc := service.NewAcquisitionService(db, ingestSvc, router, absDataDir)

	// 4. RuntimeHost HTTP Server
	addr := fmt.Sprintf("127.0.0.1:%d", listenPort)
	srv := server.New(server.Config{
		Addr:            addr,
		DB:              db,
		CASStore:        casStore,
		Ingest:          ingestSvc,
		Acquisition:     acquisitionSvc,
		Registry:        reg,
		PolicySvc:       polSvc,
		LicenseSvc:      licSvc,
		CredSvc:         credSvc,
		Router:          router,
		QueueSvc:        queueSvc,
		AutoRunExecutor: true,
		SpeechSvc:       speechSvc,
		TranslationSvc:  translationSvc,
		DubbingSvc:      dubbingSvc,
		AudioMixSvc:     audioMixSvc,
		AudioRoleSvc:    service.NewAudioRoleService(db, casStore, audioMixSvc),
		VisualTextSvc:   visualTextSvc,
		RenderSvc:       renderSvc,
		BundleSvc:       service.NewBundleService(db, casStore, licSvc),
		SnapshotSvc:     snapSvc,
	})

	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			log.Printf("[Desktop] RuntimeHost server error: %v", err)
		}
	}()

	// 5. Wails Desktop Application
	wailsApp := &options.App{
		Title:     "Douyinie Operator Workspace",
		Width:     1440,
		Height:    900,
		MinWidth:  1024,
		MinHeight: 768,
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "douyinie-operator-desktop-lock-v1",
			OnSecondInstanceLaunch: func(secondData options.SecondInstanceData) {
				log.Println("[Desktop] Secondary instance launch detected, focusing primary window")
			},
		},
		AssetServer: &assetserver.Options{
			Handler: srv.Handler(),
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:   false,
			DisableWindowIcon:     false,
		},
		Mac: &mac.Options{
			TitleBar: mac.TitleBarDefault(),
			About: &mac.AboutInfo{
				Title:   "Douyinie",
				Message: "Automated Video Localization Operator",
			},
		},
		OnStartup: func(ctx context.Context) {
			log.Printf("[Desktop] Wails desktop shell ready (Backend URL: http://%s)", addr)
		},
		OnShutdown: func(ctx context.Context) {
			log.Println("[Desktop] Wails desktop shell terminating, stopping RuntimeHost...")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
			log.Println("[Desktop] RuntimeHost stopped.")
		},
	}

	if err := wails.Run(wailsApp); err != nil {
		log.Fatalf("[Desktop] Error running Wails application: %v", err)
	}
}
