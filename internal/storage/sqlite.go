package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("record not found")
)

// DB wraps a SQLite database connection with helper methods and single-writer concurrency management.
type DB struct {
	db *sql.DB
	mu sync.RWMutex
}

// Open initializes SQLite database with foreign keys and WAL mode.
func Open(dbPath string) (*DB, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	// SQLite connection string with pragmas
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	// Configure pool for single-writer consistency
	db.SetMaxOpenConns(1)

	sdb := &DB{db: db}
	if err := sdb.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return sdb, nil
}

// Close closes the underlying SQLite database.
func (s *DB) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

// QueryRow executes a query that is expected to return at most one row.
func (s *DB) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db.QueryRowContext(ctx, query, args...)
}

func (s *DB) migrate(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Create migrations tracking table
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		);
	`)
	if err != nil {
		return fmt.Errorf("init schema_migrations: %w", err)
	}

	// 2. Initial schema migration (v1)
	var count int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).Scan(&count)
	if err != nil {
		return fmt.Errorf("check migration version 1: %w", err)
	}

	if count == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration tx: %w", err)
		}
		defer tx.Rollback()

		schemaSQL := `
		CREATE TABLE IF NOT EXISTS rights_attestations (
			id TEXT PRIMARY KEY,
			attestation_type TEXT NOT NULL,
			declared_by TEXT NOT NULL,
			terms_accepted INTEGER NOT NULL,
			notes TEXT,
			confirmed_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS source_assets (
			id TEXT PRIMARY KEY,
			sha256 TEXT NOT NULL UNIQUE,
			byte_size INTEGER NOT NULL,
			mime_type TEXT NOT NULL,
			original_filename TEXT NOT NULL,
			rights_attestation_id TEXT NOT NULL REFERENCES rights_attestations(id),
			cas_path TEXT NOT NULL,
			created_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS preflight_reports (
			id TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL UNIQUE REFERENCES source_assets(id) ON DELETE CASCADE,
			duration_sec REAL NOT NULL,
			duration_ms INTEGER NOT NULL,
			video_codec TEXT,
			audio_codec TEXT,
			width INTEGER,
			height INTEGER,
			frame_rate REAL,
			audio_channels INTEGER,
			audio_sample_rate INTEGER,
			audio_bit_rate INTEGER,
			video_bit_rate INTEGER,
			container_format TEXT NOT NULL,
			container_valid INTEGER NOT NULL,
			fingerprint_match INTEGER NOT NULL,
			errors_json TEXT,
			created_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS localization_jobs (
			id TEXT PRIMARY KEY,
			source_asset_id TEXT NOT NULL REFERENCES source_assets(id),
			target_language TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS localization_runs (
			id TEXT PRIMARY KEY,
			job_id TEXT NOT NULL REFERENCES localization_jobs(id),
			status TEXT NOT NULL,
			config_snapshot_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			completed_at TEXT
		);

		INSERT INTO schema_migrations (version, applied_at) VALUES (1, datetime('now'));
		`

		if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("execute migration v1: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v1: %w", err)
		}
	}

	// 3. Schema migration v2 (Governance, Licences, Routing, Decisions, Attempts)
	var countV2 int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 2`).Scan(&countV2)
	if err != nil {
		return fmt.Errorf("check migration version 2: %w", err)
	}

	if countV2 == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration v2 tx: %w", err)
		}
		defer tx.Rollback()

		schemaV2SQL := `
		CREATE TABLE IF NOT EXISTS license_manifests (
			id TEXT PRIMARY KEY,
			dependency_name TEXT NOT NULL UNIQUE,
			sha256 TEXT NOT NULL,
			source_repo TEXT NOT NULL,
			code_license TEXT NOT NULL,
			model_license TEXT NOT NULL,
			data_license TEXT NOT NULL,
			service_terms TEXT NOT NULL,
			verified INTEGER NOT NULL,
			created_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS credential_references (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			provider_id TEXT,
			storage_type TEXT NOT NULL,
			key_ref TEXT NOT NULL,
			created_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS provider_policies (
			provider_id TEXT PRIMARY KEY,
			policy_state TEXT NOT NULL,
			reason TEXT,
			updated_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS provider_attempts (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL,
			stage TEXT NOT NULL,
			provider_id TEXT NOT NULL,
			model_name TEXT NOT NULL,
			model_version TEXT NOT NULL,
			input_hash TEXT NOT NULL,
			attempt_number INTEGER NOT NULL,
			status TEXT NOT NULL,
			error_message TEXT,
			latency_ms INTEGER NOT NULL,
			cost_units REAL NOT NULL,
			created_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS selection_decisions (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL,
			stage TEXT NOT NULL,
			selected_provider_id TEXT NOT NULL,
			candidates_evaluated_json TEXT NOT NULL,
			policy_check_result TEXT NOT NULL,
			decision_reason TEXT NOT NULL,
			created_at TEXT NOT NULL
		);

		INSERT INTO schema_migrations (version, applied_at) VALUES (2, datetime('now'));
		`

		if _, err := tx.ExecContext(ctx, schemaV2SQL); err != nil {
			return fmt.Errorf("execute migration v2: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v2: %w", err)
		}
	}

	// 4. Schema migration v3 (Versioned License Manifests with composite dependency+version uniqueness)
	var countV3 int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 3`).Scan(&countV3)
	if err != nil {
		return fmt.Errorf("check migration version 3: %w", err)
	}

	if countV3 == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration v3 tx: %w", err)
		}
		defer tx.Rollback()

		var hasVersionCol bool
		rows, err := tx.QueryContext(ctx, `PRAGMA table_info(license_manifests)`)
		if err != nil {
			return fmt.Errorf("query table_info for license_manifests: %w", err)
		}
		for rows.Next() {
			var cid int
			var name, colType string
			var notnull, pk int
			var dfltValue sql.NullString
			if err := rows.Scan(&cid, &name, &colType, &notnull, &dfltValue, &pk); err == nil {
				if name == "version" {
					hasVersionCol = true
				}
			}
		}
		rows.Close()

		var copySQL string
		if hasVersionCol {
			copySQL = `
			INSERT INTO license_manifests_v3 (id, dependency_name, version, sha256, source_repo, code_license, model_license, data_license, service_terms, verified, created_at)
			SELECT id, dependency_name, CASE WHEN version IS NULL OR version = '' THEN 'v1' ELSE version END, sha256, source_repo, code_license, model_license, data_license, service_terms, verified, created_at
			FROM license_manifests;
			`
		} else {
			copySQL = `
			INSERT INTO license_manifests_v3 (id, dependency_name, version, sha256, source_repo, code_license, model_license, data_license, service_terms, verified, created_at)
			SELECT id, dependency_name, 'v1', sha256, source_repo, code_license, model_license, data_license, service_terms, verified, created_at
			FROM license_manifests;
			`
		}

		schemaV3SQL := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS license_manifests_v3 (
			id TEXT PRIMARY KEY,
			dependency_name TEXT NOT NULL,
			version TEXT NOT NULL DEFAULT 'v1',
			sha256 TEXT NOT NULL,
			source_repo TEXT NOT NULL,
			code_license TEXT NOT NULL,
			model_license TEXT NOT NULL,
			data_license TEXT NOT NULL,
			service_terms TEXT NOT NULL,
			verified INTEGER NOT NULL,
			created_at TEXT NOT NULL
		);

		%s

		DROP TABLE license_manifests;

		ALTER TABLE license_manifests_v3 RENAME TO license_manifests;

		CREATE UNIQUE INDEX IF NOT EXISTS idx_license_manifests_dep_ver ON license_manifests(dependency_name, version);

		INSERT INTO schema_migrations (version, applied_at) VALUES (3, datetime('now'));
		`, copySQL)

		if _, err := tx.ExecContext(ctx, schemaV3SQL); err != nil {
			return fmt.Errorf("execute migration v3: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v3: %w", err)
		}
	}

	return nil
}

// CreateRightsAttestation records rights attestation.
func (s *DB) CreateRightsAttestation(ctx context.Context, ra domain.RightsAttestation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	termsInt := 0
	if ra.TermsAccepted {
		termsInt = 1
	}

	query := `
		INSERT INTO rights_attestations (id, attestation_type, declared_by, terms_accepted, notes, confirmed_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, query,
		ra.ID,
		ra.AttestationType,
		ra.DeclaredBy,
		termsInt,
		ra.Notes,
		ra.ConfirmedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert rights_attestation: %w", err)
	}
	return nil
}

// GetRightsAttestation retrieves rights attestation by ID.
func (s *DB) GetRightsAttestation(ctx context.Context, id string) (*domain.RightsAttestation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, attestation_type, declared_by, terms_accepted, notes, confirmed_at FROM rights_attestations WHERE id = ?`
	row := s.db.QueryRowContext(ctx, query, id)

	var ra domain.RightsAttestation
	var termsInt int
	var confStr string
	var notes sql.NullString

	err := row.Scan(&ra.ID, &ra.AttestationType, &ra.DeclaredBy, &termsInt, &notes, &confStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query rights_attestation: %w", err)
	}

	ra.TermsAccepted = termsInt == 1
	if notes.Valid {
		ra.Notes = notes.String
	}
	ra.ConfirmedAt, _ = time.Parse(time.RFC3339Nano, confStr)

	return &ra, nil
}

// CreateSourceAsset stores a new source asset record.
func (s *DB) CreateSourceAsset(ctx context.Context, sa domain.SourceAsset) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		INSERT INTO source_assets (id, sha256, byte_size, mime_type, original_filename, rights_attestation_id, cas_path, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, query,
		sa.ID,
		sa.SHA256,
		sa.ByteSize,
		sa.MimeType,
		sa.OriginalFilename,
		sa.RightsAttestationID,
		sa.CASPath,
		sa.CreatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert source_asset: %w", err)
	}
	return nil
}

// GetSourceAsset retrieves a source asset by ID.
func (s *DB) GetSourceAsset(ctx context.Context, id string) (*domain.SourceAsset, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, sha256, byte_size, mime_type, original_filename, rights_attestation_id, cas_path, created_at FROM source_assets WHERE id = ?`
	row := s.db.QueryRowContext(ctx, query, id)

	var sa domain.SourceAsset
	var createdStr string
	err := row.Scan(&sa.ID, &sa.SHA256, &sa.ByteSize, &sa.MimeType, &sa.OriginalFilename, &sa.RightsAttestationID, &sa.CASPath, &createdStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrAssetNotFound
		}
		return nil, fmt.Errorf("query source_asset: %w", err)
	}
	sa.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return &sa, nil
}

// GetSourceAssetBySHA256 retrieves a source asset by its SHA256 content hash.
func (s *DB) GetSourceAssetBySHA256(ctx context.Context, sha256Hash string) (*domain.SourceAsset, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, sha256, byte_size, mime_type, original_filename, rights_attestation_id, cas_path, created_at FROM source_assets WHERE sha256 = ?`
	row := s.db.QueryRowContext(ctx, query, sha256Hash)

	var sa domain.SourceAsset
	var createdStr string
	err := row.Scan(&sa.ID, &sa.SHA256, &sa.ByteSize, &sa.MimeType, &sa.OriginalFilename, &sa.RightsAttestationID, &sa.CASPath, &createdStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrAssetNotFound
		}
		return nil, fmt.Errorf("query source_asset by sha: %w", err)
	}
	sa.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return &sa, nil
}

// SavePreflightReport stores or updates a preflight report.
func (s *DB) SavePreflightReport(ctx context.Context, pr domain.PreflightReport) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	errorsJSON, _ := json.Marshal(pr.Errors)
	validInt := 0
	if pr.ContainerValid {
		validInt = 1
	}
	fpInt := 0
	if pr.FingerprintMatch {
		fpInt = 1
	}

	query := `
		INSERT INTO preflight_reports (
			id, asset_id, duration_sec, duration_ms, video_codec, audio_codec,
			width, height, frame_rate, audio_channels, audio_sample_rate,
			audio_bit_rate, video_bit_rate, container_format, container_valid,
			fingerprint_match, errors_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(asset_id) DO UPDATE SET
			duration_sec=excluded.duration_sec,
			duration_ms=excluded.duration_ms,
			video_codec=excluded.video_codec,
			audio_codec=excluded.audio_codec,
			width=excluded.width,
			height=excluded.height,
			frame_rate=excluded.frame_rate,
			audio_channels=excluded.audio_channels,
			audio_sample_rate=excluded.audio_sample_rate,
			audio_bit_rate=excluded.audio_bit_rate,
			video_bit_rate=excluded.video_bit_rate,
			container_format=excluded.container_format,
			container_valid=excluded.container_valid,
			fingerprint_match=excluded.fingerprint_match,
			errors_json=excluded.errors_json,
			created_at=excluded.created_at
	`
	_, err := s.db.ExecContext(ctx, query,
		pr.ID,
		pr.AssetID,
		pr.DurationSec,
		pr.DurationMs,
		pr.VideoCodec,
		pr.AudioCodec,
		pr.Width,
		pr.Height,
		pr.FrameRate,
		pr.AudioChannels,
		pr.AudioSampleRate,
		pr.AudioBitRate,
		pr.VideoBitRate,
		pr.ContainerFormat,
		validInt,
		fpInt,
		string(errorsJSON),
		pr.CreatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save preflight_report: %w", err)
	}
	return nil
}

// GetPreflightReport retrieves preflight report for an asset.
func (s *DB) GetPreflightReport(ctx context.Context, assetID string) (*domain.PreflightReport, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT id, asset_id, duration_sec, duration_ms, video_codec, audio_codec,
		       width, height, frame_rate, audio_channels, audio_sample_rate,
		       audio_bit_rate, video_bit_rate, container_format, container_valid,
		       fingerprint_match, errors_json, created_at
		FROM preflight_reports WHERE asset_id = ?
	`
	row := s.db.QueryRowContext(ctx, query, assetID)

	var pr domain.PreflightReport
	var validInt, fpInt int
	var errorsJSON sql.NullString
	var createdStr string
	var vCodec, aCodec, cFmt sql.NullString

	err := row.Scan(
		&pr.ID, &pr.AssetID, &pr.DurationSec, &pr.DurationMs, &vCodec, &aCodec,
		&pr.Width, &pr.Height, &pr.FrameRate, &pr.AudioChannels, &pr.AudioSampleRate,
		&pr.AudioBitRate, &pr.VideoBitRate, &cFmt, &validInt,
		&fpInt, &errorsJSON, &createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query preflight_report: %w", err)
	}

	if vCodec.Valid {
		pr.VideoCodec = vCodec.String
	}
	if aCodec.Valid {
		pr.AudioCodec = aCodec.String
	}
	if cFmt.Valid {
		pr.ContainerFormat = cFmt.String
	}
	pr.ContainerValid = validInt == 1
	pr.FingerprintMatch = fpInt == 1
	if errorsJSON.Valid && errorsJSON.String != "" {
		_ = json.Unmarshal([]byte(errorsJSON.String), &pr.Errors)
	}
	pr.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)

	return &pr, nil
}

// CreateJob creates a new localization job.
func (s *DB) CreateJob(ctx context.Context, job domain.LocalizationJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		INSERT INTO localization_jobs (id, source_asset_id, target_language, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, query,
		job.ID,
		job.SourceAssetID,
		job.TargetLanguage,
		job.Status,
		job.CreatedAt.Format(time.RFC3339Nano),
		job.UpdatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert localization_job: %w", err)
	}
	return nil
}

// GetJob retrieves a job by ID.
func (s *DB) GetJob(ctx context.Context, id string) (*domain.LocalizationJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, source_asset_id, target_language, status, created_at, updated_at FROM localization_jobs WHERE id = ?`
	row := s.db.QueryRowContext(ctx, query, id)

	var j domain.LocalizationJob
	var createdStr, updatedStr string
	err := row.Scan(&j.ID, &j.SourceAssetID, &j.TargetLanguage, &j.Status, &createdStr, &updatedStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrJobNotFound
		}
		return nil, fmt.Errorf("query localization_job: %w", err)
	}
	j.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	j.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
	return &j, nil
}

// ListJobs retrieves all localization jobs ordered by creation date descending.
func (s *DB) ListJobs(ctx context.Context) ([]domain.LocalizationJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, source_asset_id, target_language, status, created_at, updated_at FROM localization_jobs ORDER BY created_at DESC, id ASC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query jobs: %w", err)
	}
	defer rows.Close()

	var jobs []domain.LocalizationJob
	for rows.Next() {
		var j domain.LocalizationJob
		var createdStr, updatedStr string
		if err := rows.Scan(&j.ID, &j.SourceAssetID, &j.TargetLanguage, &j.Status, &createdStr, &updatedStr); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		j.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		j.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// CreateRun creates a new localization run.
func (s *DB) CreateRun(ctx context.Context, run domain.LocalizationRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var compStr sql.NullString
	if run.CompletedAt != nil {
		compStr = sql.NullString{String: run.CompletedAt.Format(time.RFC3339Nano), Valid: true}
	}

	query := `
		INSERT INTO localization_runs (id, job_id, status, config_snapshot_json, created_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, query,
		run.ID,
		run.JobID,
		run.Status,
		run.ConfigSnapshotJSON,
		run.CreatedAt.Format(time.RFC3339Nano),
		compStr,
	)
	if err != nil {
		return fmt.Errorf("insert localization_run: %w", err)
	}
	return nil
}

// GetRun retrieves a run by ID.
func (s *DB) GetRun(ctx context.Context, id string) (*domain.LocalizationRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, job_id, status, config_snapshot_json, created_at, completed_at FROM localization_runs WHERE id = ?`
	row := s.db.QueryRowContext(ctx, query, id)

	var r domain.LocalizationRun
	var createdStr string
	var compStr sql.NullString

	err := row.Scan(&r.ID, &r.JobID, &r.Status, &r.ConfigSnapshotJSON, &createdStr, &compStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query localization_run: %w", err)
	}
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	if compStr.Valid {
		t, _ := time.Parse(time.RFC3339Nano, compStr.String)
		r.CompletedAt = &t
	}
	return &r, nil
}

// SaveLicenseManifest stores an immutable versioned license manifest entry.
func (s *DB) SaveLicenseManifest(ctx context.Context, entry domain.LicenseManifestEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	verInt := 0
	if entry.Verified {
		verInt = 1
	}

	if entry.ID == "" {
		entry.ID = uuid.NewString()
	}
	if entry.Version == "" {
		entry.Version = "v1"
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}

	var existingID string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM license_manifests WHERE dependency_name = ? AND version = ?`, entry.DependencyName, entry.Version).Scan(&existingID)
	if err == nil {
		return fmt.Errorf("license manifest for dependency %q version %q already exists (immutable)", entry.DependencyName, entry.Version)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check existing license manifest: %w", err)
	}

	query := `
		INSERT INTO license_manifests (id, dependency_name, version, sha256, source_repo, code_license, model_license, data_license, service_terms, verified, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err = s.db.ExecContext(ctx, query,
		entry.ID,
		entry.DependencyName,
		entry.Version,
		entry.SHA256,
		entry.SourceRepo,
		entry.CodeLicense,
		entry.ModelLicense,
		entry.DataLicense,
		entry.ServiceTerms,
		verInt,
		entry.CreatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert license_manifest: %w", err)
	}
	return nil
}

// GetLicenseManifest retrieves a license manifest by dependency name and optional version.
func (s *DB) GetLicenseManifest(ctx context.Context, dependencyName string, version string) (*domain.LicenseManifestEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var query string
	var args []any
	if version != "" {
		query = `SELECT id, dependency_name, version, sha256, source_repo, code_license, model_license, data_license, service_terms, verified, created_at FROM license_manifests WHERE dependency_name = ? AND version = ? ORDER BY created_at DESC LIMIT 1`
		args = []any{dependencyName, version}
	} else {
		query = `SELECT id, dependency_name, version, sha256, source_repo, code_license, model_license, data_license, service_terms, verified, created_at FROM license_manifests WHERE dependency_name = ? ORDER BY created_at DESC LIMIT 1`
		args = []any{dependencyName}
	}

	row := s.db.QueryRowContext(ctx, query, args...)

	var entry domain.LicenseManifestEntry
	var verInt int
	var createdStr string
	err := row.Scan(&entry.ID, &entry.DependencyName, &entry.Version, &entry.SHA256, &entry.SourceRepo, &entry.CodeLicense, &entry.ModelLicense, &entry.DataLicense, &entry.ServiceTerms, &verInt, &createdStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query license_manifest: %w", err)
	}
	entry.Verified = verInt == 1
	entry.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return &entry, nil
}

// ListLicenseManifests retrieves all license manifests, optionally filtered by dependency name.
func (s *DB) ListLicenseManifests(ctx context.Context, dependencyName string) ([]domain.LicenseManifestEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var query string
	var args []any
	if dependencyName != "" {
		query = `SELECT id, dependency_name, version, sha256, source_repo, code_license, model_license, data_license, service_terms, verified, created_at FROM license_manifests WHERE dependency_name = ? ORDER BY version ASC, created_at DESC, id ASC`
		args = []any{dependencyName}
	} else {
		query = `SELECT id, dependency_name, version, sha256, source_repo, code_license, model_license, data_license, service_terms, verified, created_at FROM license_manifests ORDER BY dependency_name ASC, version ASC, created_at DESC, id ASC`
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query license_manifests: %w", err)
	}
	defer rows.Close()

	var list []domain.LicenseManifestEntry
	for rows.Next() {
		var entry domain.LicenseManifestEntry
		var verInt int
		var createdStr string
		if err := rows.Scan(&entry.ID, &entry.DependencyName, &entry.Version, &entry.SHA256, &entry.SourceRepo, &entry.CodeLicense, &entry.ModelLicense, &entry.DataLicense, &entry.ServiceTerms, &verInt, &createdStr); err != nil {
			return nil, fmt.Errorf("scan license_manifest: %w", err)
		}
		entry.Verified = verInt == 1
		entry.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		list = append(list, entry)
	}
	return list, nil
}

// SaveCredentialRef stores a credential reference.
func (s *DB) SaveCredentialRef(ctx context.Context, ref domain.CredentialRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ref.ID == "" {
		ref.ID = uuid.NewString()
	}
	if ref.CreatedAt.IsZero() {
		ref.CreatedAt = time.Now().UTC()
	}

	query := `
		INSERT INTO credential_references (id, name, provider_id, storage_type, key_ref, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name,
			provider_id=excluded.provider_id,
			storage_type=excluded.storage_type,
			key_ref=excluded.key_ref
	`
	_, err := s.db.ExecContext(ctx, query,
		ref.ID,
		ref.Name,
		ref.ProviderID,
		ref.StorageType,
		ref.KeyRef,
		ref.CreatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert credential_reference: %w", err)
	}
	return nil
}

// GetCredentialRef retrieves a credential reference by ID or name.
func (s *DB) GetCredentialRef(ctx context.Context, idOrName string) (*domain.CredentialRef, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, name, provider_id, storage_type, key_ref, created_at FROM credential_references WHERE id = ? OR name = ? ORDER BY created_at DESC, id ASC LIMIT 1`
	row := s.db.QueryRowContext(ctx, query, idOrName, idOrName)

	var ref domain.CredentialRef
	var provID sql.NullString
	var createdStr string
	err := row.Scan(&ref.ID, &ref.Name, &provID, &ref.StorageType, &ref.KeyRef, &createdStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query credential_reference: %w", err)
	}
	if provID.Valid {
		ref.ProviderID = provID.String
	}
	ref.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return &ref, nil
}

// ListCredentialRefs lists all credential references.
func (s *DB) ListCredentialRefs(ctx context.Context) ([]domain.CredentialRef, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, name, provider_id, storage_type, key_ref, created_at FROM credential_references ORDER BY name ASC, created_at DESC, id ASC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query credential_references: %w", err)
	}
	defer rows.Close()

	var list []domain.CredentialRef
	for rows.Next() {
		var ref domain.CredentialRef
		var provID sql.NullString
		var createdStr string
		if err := rows.Scan(&ref.ID, &ref.Name, &provID, &ref.StorageType, &ref.KeyRef, &createdStr); err != nil {
			return nil, fmt.Errorf("scan credential_reference: %w", err)
		}
		if provID.Valid {
			ref.ProviderID = provID.String
		}
		ref.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		list = append(list, ref)
	}
	return list, nil
}

// SetProviderPolicy updates the policy state for a provider.
func (s *DB) SetProviderPolicy(ctx context.Context, providerID string, state domain.PolicyState, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		INSERT INTO provider_policies (provider_id, policy_state, reason, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(provider_id) DO UPDATE SET
			policy_state=excluded.policy_state,
			reason=excluded.reason,
			updated_at=excluded.updated_at
	`
	_, err := s.db.ExecContext(ctx, query,
		providerID,
		string(state),
		reason,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert provider_policy: %w", err)
	}
	return nil
}

// GetProviderPolicy retrieves the policy state for a provider.
func (s *DB) GetProviderPolicy(ctx context.Context, providerID string) (domain.PolicyState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT policy_state FROM provider_policies WHERE provider_id = ?`
	row := s.db.QueryRowContext(ctx, query, providerID)

	var stateStr string
	err := row.Scan(&stateStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("query provider_policy: %w", err)
	}
	return domain.PolicyState(stateStr), nil
}

// RecordProviderAttempt logs an immutable execution attempt.
func (s *DB) RecordProviderAttempt(ctx context.Context, attempt domain.ProviderAttempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		INSERT INTO provider_attempts (id, run_id, stage, provider_id, model_name, model_version, input_hash, attempt_number, status, error_message, latency_ms, cost_units, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, query,
		attempt.ID,
		attempt.RunID,
		attempt.Stage,
		attempt.ProviderID,
		attempt.ModelName,
		attempt.ModelVersion,
		attempt.InputHash,
		attempt.AttemptNumber,
		attempt.Status,
		attempt.ErrorMessage,
		attempt.LatencyMs,
		attempt.CostUnits,
		attempt.CreatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert provider_attempt: %w", err)
	}
	return nil
}

// ListProviderAttempts retrieves attempts for a run and stage.
func (s *DB) ListProviderAttempts(ctx context.Context, runID string, stage string) ([]domain.ProviderAttempt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var query string
	var args []any
	if stage != "" {
		query = `SELECT id, run_id, stage, provider_id, model_name, model_version, input_hash, attempt_number, status, error_message, latency_ms, cost_units, created_at FROM provider_attempts WHERE run_id = ? AND stage = ? ORDER BY attempt_number ASC, created_at ASC, id ASC`
		args = []any{runID, stage}
	} else if runID != "" {
		query = `SELECT id, run_id, stage, provider_id, model_name, model_version, input_hash, attempt_number, status, error_message, latency_ms, cost_units, created_at FROM provider_attempts WHERE run_id = ? ORDER BY attempt_number ASC, created_at ASC, id ASC`
		args = []any{runID}
	} else {
		query = `SELECT id, run_id, stage, provider_id, model_name, model_version, input_hash, attempt_number, status, error_message, latency_ms, cost_units, created_at FROM provider_attempts ORDER BY created_at DESC, id ASC LIMIT 100`
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query provider_attempts: %w", err)
	}
	defer rows.Close()

	var list []domain.ProviderAttempt
	for rows.Next() {
		var a domain.ProviderAttempt
		var createdStr string
		var errMsg sql.NullString
		if err := rows.Scan(&a.ID, &a.RunID, &a.Stage, &a.ProviderID, &a.ModelName, &a.ModelVersion, &a.InputHash, &a.AttemptNumber, &a.Status, &errMsg, &a.LatencyMs, &a.CostUnits, &createdStr); err != nil {
			return nil, fmt.Errorf("scan provider_attempt: %w", err)
		}
		if errMsg.Valid {
			a.ErrorMessage = errMsg.String
		}
		a.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		list = append(list, a)
	}
	return list, nil
}

// RecordSelectionDecision appends an immutable routing decision.
func (s *DB) RecordSelectionDecision(ctx context.Context, dec domain.SelectionDecision) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	candJSON, err := json.Marshal(dec.CandidatesEvaluated)
	if err != nil {
		return fmt.Errorf("marshal candidates evaluated: %w", err)
	}

	query := `
		INSERT INTO selection_decisions (id, run_id, stage, selected_provider_id, candidates_evaluated_json, policy_check_result, decision_reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err = s.db.ExecContext(ctx, query,
		dec.ID,
		dec.RunID,
		dec.Stage,
		dec.SelectedProviderID,
		string(candJSON),
		dec.PolicyCheckResult,
		dec.DecisionReason,
		dec.CreatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert selection_decision: %w", err)
	}
	return nil
}

// ListSelectionDecisions retrieves decision history.
func (s *DB) ListSelectionDecisions(ctx context.Context, runID string, stage string) ([]domain.SelectionDecision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var query string
	var args []any
	if stage != "" && runID != "" {
		query = `SELECT id, run_id, stage, selected_provider_id, candidates_evaluated_json, policy_check_result, decision_reason, created_at FROM selection_decisions WHERE run_id = ? AND stage = ? ORDER BY created_at ASC, id ASC`
		args = []any{runID, stage}
	} else if runID != "" {
		query = `SELECT id, run_id, stage, selected_provider_id, candidates_evaluated_json, policy_check_result, decision_reason, created_at FROM selection_decisions WHERE run_id = ? ORDER BY created_at ASC, id ASC`
		args = []any{runID}
	} else {
		query = `SELECT id, run_id, stage, selected_provider_id, candidates_evaluated_json, policy_check_result, decision_reason, created_at FROM selection_decisions ORDER BY created_at DESC, id ASC LIMIT 100`
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query selection_decisions: %w", err)
	}
	defer rows.Close()

	var list []domain.SelectionDecision
	for rows.Next() {
		var d domain.SelectionDecision
		var candJSON, createdStr string
		if err := rows.Scan(&d.ID, &d.RunID, &d.Stage, &d.SelectedProviderID, &candJSON, &d.PolicyCheckResult, &d.DecisionReason, &createdStr); err != nil {
			return nil, fmt.Errorf("scan selection_decision: %w", err)
		}
		_ = json.Unmarshal([]byte(candJSON), &d.CandidatesEvaluated)
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		list = append(list, d)
	}
	return list, nil
}
