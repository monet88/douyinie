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

	query := `SELECT id, source_asset_id, target_language, status, created_at, updated_at FROM localization_jobs ORDER BY created_at DESC`
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
