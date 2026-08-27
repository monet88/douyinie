package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound            = errors.New("record not found")
	ErrNotActiveQueueEntry = errors.New("run is not an active queue entry")
	ErrSlotBusy            = errors.New("another run is already running (active_run_slots=1)")
)

// DB wraps a SQLite database connection with helper methods and single-writer concurrency management.
type DB struct {
	db   *sql.DB
	mu   sync.RWMutex
	txMu sync.Mutex // serializes explicit multi-statement transactions (reorder, recovery)
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

	// 5. Schema migration v4 (Persisted Queue + Stage Executions)
	var countV4 int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 4`).Scan(&countV4)
	if err != nil {
		return fmt.Errorf("check migration version 4: %w", err)
	}

	if countV4 == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration v4 tx: %w", err)
		}
		defer tx.Rollback()

		schemaV4SQL := `
		CREATE TABLE IF NOT EXISTS queue_entries (
			id          TEXT PRIMARY KEY,
			run_id      TEXT NOT NULL REFERENCES localization_runs(id),
			job_id      TEXT NOT NULL REFERENCES localization_jobs(id),
			position    INTEGER,          -- NULL for terminal entries (cancelled/completed/interrupted); active entries own 1..N
			status      TEXT NOT NULL,
			inserted_at TEXT NOT NULL,
			updated_at  TEXT NOT NULL
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_queue_entries_position ON queue_entries(position);

		CREATE TABLE IF NOT EXISTS stage_executions (
			id              TEXT PRIMARY KEY,
			run_id          TEXT NOT NULL REFERENCES localization_runs(id),
			stage           TEXT NOT NULL,
			status          TEXT NOT NULL,
			started_at      TEXT,
			completed_at    TEXT,
			artifact_sha256 TEXT,
			error_message   TEXT,
			created_at      TEXT NOT NULL,
			updated_at      TEXT NOT NULL
		);

		INSERT INTO schema_migrations (version, applied_at) VALUES (4, datetime('now'));
		`

		if _, err := tx.ExecContext(ctx, schemaV4SQL); err != nil {
			return fmt.Errorf("execute migration v4: %w", err)
		}

		// Backfill queue_entries for pre-existing localization_runs from pre-T03/v3 databases.
		// Without this, upgrade + restart/recovery would strand old runs: they'd be invisible
		// to MarkAllActiveInterrupted, NextQueuedEntry, and every queue operation.
		//
		// Guard on the table existing: an ancient pre-v1 hand-built DB may never have had
		// localization_runs created (migration v1 creates it in its own transaction), so the
		// backfill must be a no-op when the table is absent.
		var runTableCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='localization_runs'`).Scan(&runTableCount); err != nil {
			return fmt.Errorf("check localization_runs table for backfill: %w", err)
		}
		if runTableCount == 0 {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit migration v4: %w", err)
			}
			return nil
		}

		rows, err := tx.QueryContext(ctx, `SELECT id, job_id, status FROM localization_runs ORDER BY created_at ASC, id ASC`)
		if err != nil {
			return fmt.Errorf("query existing runs for backfill: %w", err)
		}
		type runRow struct {
			id, jobID, status string
		}
		var existingRuns []runRow
		for rows.Next() {
			var r runRow
			if err := rows.Scan(&r.id, &r.jobID, &r.status); err != nil {
				rows.Close()
				return fmt.Errorf("scan backfill run: %w", err)
			}
			existingRuns = append(existingRuns, r)
		}
		rows.Close()

		// Determine the next sequential position for active entries.
		var maxPos sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(position) FROM queue_entries`).Scan(&maxPos); err != nil {
			return fmt.Errorf("query max position for backfill: %w", err)
		}
		nextPos := 1
		if maxPos.Valid {
			nextPos = int(maxPos.Int64) + 1
		}

		nowStr := time.Now().UTC().Format(time.RFC3339Nano)
		for _, r := range existingRuns {
			// Skip runs that already have a queue entry (shouldn't happen on v3→v4 but safe).
			var existingCount int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM queue_entries WHERE run_id = ?`, r.id).Scan(&existingCount); err != nil {
				return fmt.Errorf("check existing queue entry for run %s: %w", r.id, err)
			}
			if existingCount > 0 {
				continue
			}

			// Map old status names to the new queue statuses.
			// Old set: "queued", "running", "succeeded", "failed", "interrupted"
			// New set: "queued", "running", "paused", "cancelled", "completed", "interrupted"
			queueStatus := r.status
			var pos sql.NullInt64
			switch r.status {
			case "succeeded":
				queueStatus = domain.RunStatusCompleted
			case "failed":
				queueStatus = domain.RunStatusInterrupted
			case "queued", "running":
				pos = sql.NullInt64{Int64: int64(nextPos), Valid: true}
				nextPos++
			}

			entryID := uuid.NewString()
			if _, err := tx.ExecContext(ctx, `INSERT INTO queue_entries (id, run_id, job_id, position, status, inserted_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				entryID, r.id, r.jobID, pos, queueStatus, nowStr, nowStr,
			); err != nil {
				return fmt.Errorf("backfill queue entry for run %s: %w", r.id, err)
			}

			// Keep the run row itself consistent with the new status set. Old names that
			// map to a different new status (succeeded->completed, failed->interrupted)
			// must be rewritten here; without this the run row would carry a status value
			// that no longer exists in the new enum and readers like GET /runs/{id} would
			// report an invalid status forever.
			if queueStatus != r.status {
				if _, err := tx.ExecContext(ctx, `UPDATE localization_runs SET status = ? WHERE id = ?`, queueStatus, r.id); err != nil {
					return fmt.Errorf("update run %s status during backfill: %w", r.id, err)
				}
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v4: %w", err)
		}
	}

	// 6. Schema migration v5 (Audio Role Plans)
	var countV5 int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 5`).Scan(&countV5)
	if err != nil {
		return fmt.Errorf("check migration version 5: %w", err)
	}

	if countV5 == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration v5 tx: %w", err)
		}
		defer tx.Rollback()

		schemaV5SQL := `
		CREATE TABLE IF NOT EXISTS audio_role_plans (
			id TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			plan_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		);

		INSERT INTO schema_migrations (version, applied_at) VALUES (5, datetime('now'));
		`

		if _, err := tx.ExecContext(ctx, schemaV5SQL); err != nil {
			return fmt.Errorf("execute migration v5: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v5: %w", err)
		}
	}

	// 7. Schema migration v6 (Transcript Artifacts - Speech Understanding T08)
	var countV6 int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 6`).Scan(&countV6)
	if err != nil {
		return fmt.Errorf("check migration version 6: %w", err)
	}

	if countV6 == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration v6 tx: %w", err)
		}
		defer tx.Rollback()

		schemaV6SQL := `
		CREATE TABLE IF NOT EXISTS transcript_artifacts (
			id TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			run_id TEXT NOT NULL,
			cas_hash TEXT NOT NULL,
			provenance_hash TEXT NOT NULL,
			asr_provider_id TEXT NOT NULL,
			asr_model_name TEXT NOT NULL,
			asr_model_version TEXT NOT NULL,
			aligner_provider_id TEXT NOT NULL,
			aligner_model_name TEXT NOT NULL,
			aligner_model_version TEXT NOT NULL,
			segment_cfg_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_transcript_artifacts_provenance ON transcript_artifacts(provenance_hash);
		CREATE INDEX IF NOT EXISTS idx_transcript_artifacts_asset ON transcript_artifacts(asset_id);

		INSERT INTO schema_migrations (version, applied_at) VALUES (6, datetime('now'));
		`

		if _, err := tx.ExecContext(ctx, schemaV6SQL); err != nil {
			return fmt.Errorf("execute migration v6: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v6: %w", err)
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

// UpdateJobStatus updates the status of a job.
func (s *DB) UpdateJobStatus(ctx context.Context, id string, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `UPDATE localization_jobs SET status = ?, updated_at = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, query, status, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("update job status: %w", err)
	}
	return nil
}

// SaveAudioRolePlan saves or replaces an audio role plan.
func (s *DB) SaveAudioRolePlan(ctx context.Context, plan domain.AudioRolePlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, seg := range plan.Segments {
		switch seg.Role {
		case domain.AudioRoleNarrationDialogue,
			domain.AudioRoleSingingMusicVocal,
			domain.AudioRoleInstrumentalBgm,
			domain.AudioRoleAmbienceSFX,
			domain.AudioRoleUncertain:
			// valid
		default:
			return fmt.Errorf("invalid audio role value: %q", seg.Role)
		}
	}

	segmentsJSON, err := json.Marshal(plan.Segments)
	if err != nil {
		return fmt.Errorf("marshal audio segments: %w", err)
	}

	createdAtStr := plan.CreatedAt.Format(time.RFC3339Nano)

	// Check if a plan for this asset_id already exists to update it
	var existingID string
	err = s.db.QueryRowContext(ctx, `SELECT id FROM audio_role_plans WHERE asset_id = ?`, plan.AssetID).Scan(&existingID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check existing audio role plan: %w", err)
	}

	if existingID != "" {
		// Update existing plan, retaining its ID (or update ID to match plan.ID to be consistent with client request)
		query := `UPDATE audio_role_plans SET id = ?, plan_json = ?, created_at = ? WHERE asset_id = ?`
		_, err = s.db.ExecContext(ctx, query, plan.ID, string(segmentsJSON), createdAtStr, plan.AssetID)
		if err != nil {
			return fmt.Errorf("update audio role plan: %w", err)
		}
	} else {
		// Insert new plan
		query := `INSERT INTO audio_role_plans (id, asset_id, plan_json, created_at) VALUES (?, ?, ?, ?)`
		_, err = s.db.ExecContext(ctx, query, plan.ID, plan.AssetID, string(segmentsJSON), createdAtStr)
		if err != nil {
			return fmt.Errorf("save audio role plan: %w", err)
		}
	}
	return nil
}

// GetAudioRolePlan retrieves an audio role plan by asset ID.
func (s *DB) GetAudioRolePlan(ctx context.Context, assetID string) (*domain.AudioRolePlan, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var plan domain.AudioRolePlan
	var planJSON string
	var createdAtStr string

	query := `SELECT id, asset_id, plan_json, created_at FROM audio_role_plans WHERE asset_id = ?`
	err := s.db.QueryRowContext(ctx, query, assetID).Scan(&plan.ID, &plan.AssetID, &planJSON, &createdAtStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query audio role plan: %w", err)
	}

	if err := json.Unmarshal([]byte(planJSON), &plan.Segments); err != nil {
		return nil, fmt.Errorf("unmarshal audio segments: %w", err)
	}

	t, err := time.Parse(time.RFC3339Nano, createdAtStr)
	if err != nil {
		return nil, fmt.Errorf("parse created_at time: %w", err)
	}
	plan.CreatedAt = t

	return &plan, nil
}

// TranscriptArtifactIndex is the SQLite index row for a content-addressed
// TranscriptArtifact. The artifact JSON blob itself lives in the CAS store;
// SQLite indexes it by asset and by deterministic provenance identity.
type TranscriptArtifactIndex struct {
	ID                  string
	AssetID             string
	RunID               string
	CASHash             string
	ProvenanceHash      string
	ASRProviderID       string
	ASRModelName        string
	ASRModelVersion     string
	AlignerProviderID   string
	AlignerModelName    string
	AlignerModelVersion string
	SegmentCfgJSON      string
	CreatedAt           time.Time
}

// SaveTranscriptArtifactIndex records the index row for a CAS-stored
// TranscriptArtifact. The provenance hash is the deterministic cache identity:
// re-deriving the same pipeline inputs produces the same provenance hash, so
// the write is idempotent (a repeated save for identical provenance is a no-op
// rather than a permanent write-once failure). A changed provider/model/config
// yields a different provenance hash and thus a new artifact row.
func (s *DB) SaveTranscriptArtifactIndex(ctx context.Context, idx TranscriptArtifactIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(idx.AssetID) == "" || strings.TrimSpace(idx.CASHash) == "" || strings.TrimSpace(idx.ProvenanceHash) == "" {
		return errors.New("transcript artifact index requires asset_id, cas_hash, and provenance_hash")
	}

	createdAtStr := idx.CreatedAt.Format(time.RFC3339Nano)
	query := `
		INSERT INTO transcript_artifacts (id, asset_id, run_id, cas_hash, provenance_hash, asr_provider_id, asr_model_name, asr_model_version, aligner_provider_id, aligner_model_name, aligner_model_version, segment_cfg_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provenance_hash) DO NOTHING
	`
	_, err := s.db.ExecContext(ctx, query,
		idx.ID, idx.AssetID, idx.RunID, idx.CASHash, idx.ProvenanceHash,
		idx.ASRProviderID, idx.ASRModelName, idx.ASRModelVersion,
		idx.AlignerProviderID, idx.AlignerModelName, idx.AlignerModelVersion,
		idx.SegmentCfgJSON, createdAtStr)
	if err != nil {
		return fmt.Errorf("save transcript artifact index: %w", err)
	}
	return nil
}

// GetTranscriptArtifactIndex retrieves the latest index row for an asset.
func (s *DB) GetTranscriptArtifactIndex(ctx context.Context, assetID string) (*TranscriptArtifactIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx TranscriptArtifactIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, cas_hash, provenance_hash, asr_provider_id, asr_model_name, asr_model_version, aligner_provider_id, aligner_model_name, aligner_model_version, segment_cfg_json, created_at
		FROM transcript_artifacts WHERE asset_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`
	err := s.db.QueryRowContext(ctx, query, assetID).Scan(
		&idx.ID, &idx.AssetID, &idx.RunID, &idx.CASHash, &idx.ProvenanceHash,
		&idx.ASRProviderID, &idx.ASRModelName, &idx.ASRModelVersion,
		&idx.AlignerProviderID, &idx.AlignerModelName, &idx.AlignerModelVersion,
		&idx.SegmentCfgJSON, &createdStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query transcript artifact index: %w", err)
	}
	idx.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return &idx, nil
}

// GetTranscriptArtifactByProvenance retrieves the index row for a deterministic
// provenance identity, enabling idempotent re-derivation of unchanged inputs.
func (s *DB) GetTranscriptArtifactByProvenance(ctx context.Context, provenanceHash string) (*TranscriptArtifactIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx TranscriptArtifactIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, cas_hash, provenance_hash, asr_provider_id, asr_model_name, asr_model_version, aligner_provider_id, aligner_model_name, aligner_model_version, segment_cfg_json, created_at
		FROM transcript_artifacts WHERE provenance_hash = ? LIMIT 1`
	err := s.db.QueryRowContext(ctx, query, provenanceHash).Scan(
		&idx.ID, &idx.AssetID, &idx.RunID, &idx.CASHash, &idx.ProvenanceHash,
		&idx.ASRProviderID, &idx.ASRModelName, &idx.ASRModelVersion,
		&idx.AlignerProviderID, &idx.AlignerModelName, &idx.AlignerModelVersion,
		&idx.SegmentCfgJSON, &createdStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query transcript artifact by provenance: %w", err)
	}
	idx.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return &idx, nil
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

// CreateRunEnqueued creates a run and its queue entry in a single transaction,
// so a partial failure never leaves an orphaned run without a queue entry.
func (s *DB) CreateRunEnqueued(ctx context.Context, run domain.LocalizationRun, jobID string) (int, error) {
	s.txMu.Lock()
	defer s.txMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin create-run+enqueue tx: %w", err)
	}
	defer tx.Rollback()

	var compStr sql.NullString
	if run.CompletedAt != nil {
		compStr = sql.NullString{String: run.CompletedAt.Format(time.RFC3339Nano), Valid: true}
	}
	query := `
		INSERT INTO localization_runs (id, job_id, status, config_snapshot_json, created_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`
	if _, err := tx.ExecContext(ctx, query,
		run.ID, run.JobID, run.Status, run.ConfigSnapshotJSON,
		run.CreatedAt.Format(time.RFC3339Nano), compStr,
	); err != nil {
		return 0, fmt.Errorf("insert localization_run: %w", err)
	}

	var maxPos sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(position) FROM queue_entries`).Scan(&maxPos); err != nil {
		return 0, fmt.Errorf("query max queue position: %w", err)
	}
	pos := 1
	if maxPos.Valid {
		pos = int(maxPos.Int64) + 1
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	entryID := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `INSERT INTO queue_entries (id, run_id, job_id, position, status, inserted_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entryID, run.ID, jobID, pos, domain.RunStatusQueued, now, now,
	); err != nil {
		return 0, fmt.Errorf("insert queue_entry: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit create-run+enqueue tx: %w", err)
	}
	return pos, nil
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

// ---- Queue (persisted run ordering) ----

func scanQueueEntry(row *sql.Row) (*domain.QueueEntry, error) {
	var e domain.QueueEntry
	var pos sql.NullInt64
	var insStr, updStr string
	if err := row.Scan(&e.ID, &e.RunID, &e.JobID, &pos, &e.Status, &insStr, &updStr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query queue_entry: %w", err)
	}
	if pos.Valid {
		e.Position = int(pos.Int64)
	}
	e.InsertedAt, _ = time.Parse(time.RFC3339Nano, insStr)
	e.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updStr)
	return &e, nil
}

func scanQueueEntries(rows *sql.Rows) ([]domain.QueueEntry, error) {
	var list []domain.QueueEntry
	for rows.Next() {
		var e domain.QueueEntry
		var pos sql.NullInt64
		var insStr, updStr string
		if err := rows.Scan(&e.ID, &e.RunID, &e.JobID, &pos, &e.Status, &insStr, &updStr); err != nil {
			return nil, fmt.Errorf("scan queue_entry: %w", err)
		}
		if pos.Valid {
			e.Position = int(pos.Int64)
		}
		e.InsertedAt, _ = time.Parse(time.RFC3339Nano, insStr)
		e.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updStr)
		list = append(list, e)
	}
	return list, nil
}

const queueEntryColumns = `id, run_id, job_id, position, status, inserted_at, updated_at`

// CreateQueueEntry appends a queue entry at the next position.
func (s *DB) CreateQueueEntry(ctx context.Context, e domain.QueueEntry) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var maxPos sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(position) FROM queue_entries`).Scan(&maxPos); err != nil {
		return 0, fmt.Errorf("query max queue position: %w", err)
	}
	pos := 1
	if maxPos.Valid {
		pos = int(maxPos.Int64) + 1
	}
	e.Position = pos

	query := `INSERT INTO queue_entries (id, run_id, job_id, position, status, inserted_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, query,
		e.ID, e.RunID, e.JobID, e.Position, e.Status,
		e.InsertedAt.Format(time.RFC3339Nano), e.UpdatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return 0, fmt.Errorf("insert queue_entry: %w", err)
	}
	return pos, nil
}

// GetQueueEntryByRunID retrieves the queue entry for a run.
func (s *DB) GetQueueEntryByRunID(ctx context.Context, runID string) (*domain.QueueEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRowContext(ctx, `SELECT `+queueEntryColumns+` FROM queue_entries WHERE run_id = ?`, runID)
	return scanQueueEntry(row)
}

// ListQueueEntries returns all queue entries ordered by position.
func (s *DB) ListQueueEntries(ctx context.Context) ([]domain.QueueEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `SELECT `+queueEntryColumns+` FROM queue_entries ORDER BY position ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query queue_entries: %w", err)
	}
	defer rows.Close()
	return scanQueueEntries(rows)
}

// NextQueuedEntry returns the lowest-position entry with status 'queued'.
func (s *DB) NextQueuedEntry(ctx context.Context) (*domain.QueueEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRowContext(ctx, `SELECT `+queueEntryColumns+` FROM queue_entries WHERE status = 'queued' ORDER BY position ASC, id ASC LIMIT 1`)
	return scanQueueEntry(row)
}

// UpdateQueueStatus updates the status of a queue entry (and its linked run status) atomically.
// Terminal transitions (cancelled/completed/interrupted) release the entry's position (set NULL)
// so active entries always own positions 1..N, and completed_at is set only on terminal transitions.
//
// Stage execution alignment: because active stage states are QUEUED/RUNNING/CANCELLING
// (locked #13/#16), a run that stops actively executing must never leave its stages in the
// active set. Pause returns running/cancelling stages to queued (reversible); cancel and
// interrupted terminalize running/cancelling stages to interrupted. Both happen in the same
// transaction so the run and its stages always agree.
func (s *DB) UpdateQueueStatus(ctx context.Context, runID, queueStatus, runStatus string) error {
	s.txMu.Lock()
	defer s.txMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin queue status tx: %w", err)
	}
	defer tx.Rollback()

	isTerminal := queueStatus == domain.RunStatusCancelled || queueStatus == domain.RunStatusCompleted || queueStatus == domain.RunStatusInterrupted

	if isTerminal {
		// Release the position so active entries keep an unbroken 1..N position space.
		if _, err := tx.ExecContext(ctx, `UPDATE queue_entries SET status = ?, position = NULL, updated_at = ? WHERE run_id = ?`,
			queueStatus, time.Now().UTC().Format(time.RFC3339Nano), runID); err != nil {
			return fmt.Errorf("update queue_entry status: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE queue_entries SET status = ?, updated_at = ? WHERE run_id = ?`,
			queueStatus, time.Now().UTC().Format(time.RFC3339Nano), runID); err != nil {
			return fmt.Errorf("update queue_entry status: %w", err)
		}
	}

	// Align the run's stage executions with the new run state (see doc comment).
	stageSnapSQL := `UPDATE stage_executions SET status = ?, updated_at = ? WHERE run_id = ? AND status IN ('running','cancelling')`
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	switch queueStatus {
	case domain.RunStatusPaused:
		// Paused runs are not actively executing; return in-flight stages to queued so resume can restart them.
		if _, err := tx.ExecContext(ctx, stageSnapSQL, domain.StageStatusQueued, nowStr, runID); err != nil {
			return fmt.Errorf("snap stages to queued on pause: %w", err)
		}
	case domain.RunStatusCancelled:
		// Deliberate termination mid-execution is interrupted (same as crash recovery).
		if _, err := tx.ExecContext(ctx, stageSnapSQL, domain.StageStatusInterrupted, nowStr, runID); err != nil {
			return fmt.Errorf("snap stages to interrupted on cancel: %w", err)
		}
	}

	// Set completed_at only for terminal statuses; preserve it otherwise (pass SQL NULL).
	var completedExpr any
	if runStatus == domain.RunStatusCompleted || runStatus == domain.RunStatusInterrupted {
		completedExpr = time.Now().UTC().Format(time.RFC3339Nano)
	} else {
		completedExpr = nil // SQL NULL -> COALESCE(NULL, completed_at) preserves prior value
	}
	if runStatus != "" {
		query := `UPDATE localization_runs SET status = ?, completed_at = COALESCE(?, completed_at) WHERE id = ?`
		if _, err := tx.ExecContext(ctx, query, runStatus, completedExpr, runID); err != nil {
			return fmt.Errorf("update localization_run status: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit queue status tx: %w", err)
	}
	return nil
}

// ReorderQueueEntries reassigns positions within a single transaction (append-only: never deletes).
func (s *DB) ReorderQueueEntries(ctx context.Context, runID string, newPosition int) error {
	s.txMu.Lock()
	defer s.txMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reorder tx: %w", err)
	}
	defer tx.Rollback()

	// Fetch current entry + all active entries (queued/running/paused), excluding cancelled/completed/interrupted.
	rows, err := tx.QueryContext(ctx, `SELECT run_id, position FROM queue_entries WHERE status IN ('queued','running','paused') ORDER BY position ASC, id ASC`)
	if err != nil {
		return fmt.Errorf("query reorder entries: %w", err)
	}
	type rowT struct {
		runID string
		pos   int
	}
	var entries []rowT
	for rows.Next() {
		var r rowT
		if err := rows.Scan(&r.runID, &r.pos); err != nil {
			rows.Close()
			return fmt.Errorf("scan reorder entry: %w", err)
		}
		entries = append(entries, r)
	}
	rows.Close()

	targetIdx := -1
	for i, e := range entries {
		if e.runID == runID {
			targetIdx = i
			break
		}
	}
	if targetIdx == -1 {
		return fmt.Errorf("%w: run %s", ErrNotActiveQueueEntry, runID)
	}
	if newPosition < 1 || newPosition > len(entries) {
		return fmt.Errorf("new position %d out of range [1, %d]", newPosition, len(entries))
	}

	// Remove target, re-insert at newPosition-1, reassign sequential positions.
	moved := entries[targetIdx]
	entries = append(entries[:targetIdx], entries[targetIdx+1:]...)
	entries = append(entries, rowT{}) // grow
	copy(entries[newPosition:], entries[newPosition-1:])
	entries[newPosition-1] = moved

	// Two-phase update to avoid transient UNIQUE(position) collisions during a swap:
	// 1) shift every active entry to a temporary negative position, 2) set final positions.
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	for i, e := range entries {
		if _, err := tx.ExecContext(ctx, `UPDATE queue_entries SET position = ?, updated_at = ? WHERE run_id = ?`,
			-(i + 1), nowStr, e.runID); err != nil {
			return fmt.Errorf("phase-1 shift queue position: %w", err)
		}
	}
	for i, e := range entries {
		if _, err := tx.ExecContext(ctx, `UPDATE queue_entries SET position = ?, updated_at = ? WHERE run_id = ?`,
			i+1, nowStr, e.runID); err != nil {
			return fmt.Errorf("update queue position: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reorder tx: %w", err)
	}
	return nil
}

// MarkAllActiveInterrupted transitions every running queue entry (and only running
// runs) to interrupted at daemon startup. Queued never-started runs survive restart
// as queued; paused runs stay paused. Only stages that were actively executing
// (running or cancelling) are interrupted — queued stages stay queued and terminal
// (succeeded/failed) stage outcomes are left untouched so crash recovery never
// conflates active and terminal stage states.
func (s *DB) MarkAllActiveInterrupted(ctx context.Context) ([]string, error) {
	s.txMu.Lock()
	defer s.txMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin recovery tx: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT run_id FROM queue_entries WHERE status = 'running'`)
	if err != nil {
		return nil, fmt.Errorf("query active queue entries: %w", err)
	}
	var runIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan active queue entry: %w", err)
		}
		runIDs = append(runIDs, id)
	}
	rows.Close()

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	for _, rid := range runIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE queue_entries SET status = 'interrupted', position = NULL, updated_at = ? WHERE run_id = ?`, nowStr, rid); err != nil {
			return nil, fmt.Errorf("mark queue entry interrupted: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE localization_runs SET status = 'interrupted', completed_at = ? WHERE id = ?`, nowStr, rid); err != nil {
			return nil, fmt.Errorf("mark run interrupted: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE stage_executions SET status = 'interrupted', updated_at = ? WHERE run_id = ? AND status IN ('running','cancelling')`, nowStr, rid); err != nil {
			return nil, fmt.Errorf("mark stage interrupted: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit recovery tx: %w", err)
	}
	return runIDs, nil
}

// MarkRunRunning atomically transitions a queued run to running while enforcing
// active_run_slots=1 in persisted state: it fails if the target run is not queued,
// or if any other run is already running. All transitions to 'running' must go
// through here so the single-active-run invariant holds even across a concurrent
// caller that bypasses the in-memory scheduler lease.
func (s *DB) MarkRunRunning(ctx context.Context, runID string) error {
	s.txMu.Lock()
	defer s.txMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mark-running tx: %w", err)
	}
	defer tx.Rollback()

	var targetStatus string
	err = tx.QueryRowContext(ctx, `SELECT status FROM queue_entries WHERE run_id = ?`, runID).Scan(&targetStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: run %s not found", ErrNotActiveQueueEntry, runID)
		}
		return fmt.Errorf("query target queue entry: %w", err)
	}
	if targetStatus != domain.RunStatusQueued {
		return fmt.Errorf("%w: status is %s", ErrNotActiveQueueEntry, targetStatus)
	}

	var runningCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM queue_entries WHERE status = 'running' AND run_id != ?`, runID).Scan(&runningCount); err != nil {
		return fmt.Errorf("count running entries: %w", err)
	}
	if runningCount > 0 {
		return ErrSlotBusy
	}

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE queue_entries SET status = 'running', updated_at = ? WHERE run_id = ?`, nowStr, runID); err != nil {
		return fmt.Errorf("mark queue entry running: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE localization_runs SET status = 'running' WHERE id = ?`, runID); err != nil {
		return fmt.Errorf("mark run running: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mark-running tx: %w", err)
	}
	return nil
}

// ---- Stage Executions ----

const stageExecutionColumns = `id, run_id, stage, status, started_at, completed_at, artifact_sha256, error_message, created_at, updated_at`

func nullableTime(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: t.Format(time.RFC3339Nano), Valid: true}
}

func parseNullableTime(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return nil
	}
	return &t
}

func scanStageExecution(row *sql.Row) (*domain.StageExecution, error) {
	var se domain.StageExecution
	var started, completed, created, updated sql.NullString
	var art sql.NullString
	var errMsg sql.NullString
	if err := row.Scan(&se.ID, &se.RunID, &se.Stage, &se.Status, &started, &completed, &art, &errMsg, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query stage_execution: %w", err)
	}
	se.StartedAt = parseNullableTime(started)
	se.CompletedAt = parseNullableTime(completed)
	se.ArtifactSHA256 = art.String
	se.ErrorMessage = errMsg.String
	if created.Valid {
		se.CreatedAt, _ = time.Parse(time.RFC3339Nano, created.String)
	}
	if updated.Valid {
		se.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated.String)
	}
	return &se, nil
}

func scanStageExecutions(rows *sql.Rows) ([]domain.StageExecution, error) {
	var list []domain.StageExecution
	for rows.Next() {
		var se domain.StageExecution
		var started, completed, created, updated sql.NullString
		var art sql.NullString
		var errMsg sql.NullString
		if err := rows.Scan(&se.ID, &se.RunID, &se.Stage, &se.Status, &started, &completed, &art, &errMsg, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan stage_execution: %w", err)
		}
		se.StartedAt = parseNullableTime(started)
		se.CompletedAt = parseNullableTime(completed)
		se.ArtifactSHA256 = art.String
		se.ErrorMessage = errMsg.String
		if created.Valid {
			se.CreatedAt, _ = time.Parse(time.RFC3339Nano, created.String)
		}
		if updated.Valid {
			se.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated.String)
		}
		list = append(list, se)
	}
	return list, nil
}

// CreateStageExecution records a new stage execution in queued (not-yet-started) state.
func (s *DB) CreateStageExecution(ctx context.Context, se domain.StageExecution) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `INSERT INTO stage_executions (id, run_id, stage, status, started_at, completed_at, artifact_sha256, error_message, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, query,
		se.ID, se.RunID, se.Stage, se.Status,
		nullableTime(se.StartedAt), nullableTime(se.CompletedAt),
		sql.NullString{String: se.ArtifactSHA256, Valid: se.ArtifactSHA256 != ""},
		sql.NullString{String: se.ErrorMessage, Valid: se.ErrorMessage != ""},
		se.CreatedAt.Format(time.RFC3339Nano), se.UpdatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert stage_execution: %w", err)
	}
	return nil
}

// UpdateStageExecution updates status and lifecycle timestamps of a stage execution.
func (s *DB) UpdateStageExecution(ctx context.Context, se domain.StageExecution) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `UPDATE stage_executions SET status = ?, started_at = ?, completed_at = ?, artifact_sha256 = ?, error_message = ?, updated_at = ? WHERE id = ?`
	res, err := s.db.ExecContext(ctx, query,
		se.Status,
		nullableTime(se.StartedAt), nullableTime(se.CompletedAt),
		sql.NullString{String: se.ArtifactSHA256, Valid: se.ArtifactSHA256 != ""},
		sql.NullString{String: se.ErrorMessage, Valid: se.ErrorMessage != ""},
		se.UpdatedAt.Format(time.RFC3339Nano),
		se.ID,
	)
	if err != nil {
		return fmt.Errorf("update stage_execution: %w", err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListStageExecutions returns stage executions for a run, ordered by creation.
func (s *DB) ListStageExecutions(ctx context.Context, runID string) ([]domain.StageExecution, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `SELECT `+stageExecutionColumns+` FROM stage_executions WHERE run_id = ? ORDER BY created_at ASC, id ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("query stage_executions: %w", err)
	}
	defer rows.Close()
	return scanStageExecutions(rows)
}

// RunStateSnapshot loads a run with its stage executions for crash recovery projection.
func (s *DB) RunStateSnapshot(ctx context.Context, runID string) (*domain.RunStateSnapshot, error) {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	stages, err := s.ListStageExecutions(ctx, runID)
	if err != nil {
		return nil, err
	}
	snap := &domain.RunStateSnapshot{
		ID:              run.ID,
		JobID:           run.JobID,
		Status:          run.Status,
		StageExecutions: stages,
		CreatedAt:       run.CreatedAt,
		CompletedAt:     run.CompletedAt,
	}
	if snap.StageExecutions == nil {
		snap.StageExecutions = []domain.StageExecution{}
	}
	return snap, nil
}
