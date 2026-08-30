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
			normalized_audio_sha256 TEXT,
			normalized_audio_cas_path TEXT,
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
	// 8. Schema migration v7 (Normalized Audio Preflight Artifacts)
	var countV7 int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 7`).Scan(&countV7)
	if err != nil {
		return fmt.Errorf("check migration version 7: %w", err)
	}

	if countV7 == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration v7 tx: %w", err)
		}
		defer tx.Rollback()

		var tableCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='preflight_reports'`).Scan(&tableCount); err != nil {
			return fmt.Errorf("check preflight_reports table for migration v7: %w", err)
		}
		if tableCount == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (7, datetime('now'));`); err != nil {
				return fmt.Errorf("record migration v7: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit migration v7: %w", err)
			}
			return nil
		}

		var hasSHACol, hasCASCol bool
		rows, err := tx.QueryContext(ctx, `PRAGMA table_info(preflight_reports)`)
		if err != nil {
			return fmt.Errorf("query table_info for preflight_reports: %w", err)
		}
		for rows.Next() {
			var cid int
			var name, colType string
			var notnull, pk int
			var dfltValue sql.NullString
			if err := rows.Scan(&cid, &name, &colType, &notnull, &dfltValue, &pk); err == nil {
				if name == "normalized_audio_sha256" {
					hasSHACol = true
				}
				if name == "normalized_audio_cas_path" {
					hasCASCol = true
				}
			}
		}
		rows.Close()

		if !hasSHACol {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE preflight_reports ADD COLUMN normalized_audio_sha256 TEXT;`); err != nil {
				if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
					return fmt.Errorf("add normalized_audio_sha256 column: %w", err)
				}
			}
		}
		if !hasCASCol {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE preflight_reports ADD COLUMN normalized_audio_cas_path TEXT;`); err != nil {
				if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
					return fmt.Errorf("add normalized_audio_cas_path column: %w", err)
				}
			}
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (7, datetime('now'));`); err != nil {
			return fmt.Errorf("record migration v7: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v7: %w", err)
		}
	}

	// 9. Schema migration v8 (Translation Variants - Meaning-First T06)
	var countV8 int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 8`).Scan(&countV8)
	if err != nil {
		return fmt.Errorf("check migration version 8: %w", err)
	}

	if countV8 == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration v8 tx: %w", err)
		}
		defer tx.Rollback()

		schemaV8SQL := `
		CREATE TABLE IF NOT EXISTS translation_variants (
			id TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			run_id TEXT NOT NULL,
			job_id TEXT NOT NULL,
			target_language TEXT NOT NULL,
			cas_hash TEXT NOT NULL,
			provenance_hash TEXT NOT NULL,
			provider_id TEXT NOT NULL,
			model_name TEXT NOT NULL,
			model_version TEXT NOT NULL,
			overall_qa_score REAL NOT NULL,
			created_at TEXT NOT NULL
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_translation_variants_provenance ON translation_variants(provenance_hash);
		CREATE INDEX IF NOT EXISTS idx_translation_variants_asset_lang ON translation_variants(asset_id, target_language);
		CREATE INDEX IF NOT EXISTS idx_translation_variants_run ON translation_variants(run_id);

		INSERT INTO schema_migrations (version, applied_at) VALUES (8, datetime('now'));
		`

		if _, err := tx.ExecContext(ctx, schemaV8SQL); err != nil {
			return fmt.Errorf("execute migration v8: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v8: %w", err)
		}
	}

	// 10. Schema migration v9 (DubScript Variants - Duration Adaptation T13)
	var countV9 int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 9`).Scan(&countV9)
	if err != nil {
		return fmt.Errorf("check migration version 9: %w", err)
	}

	if countV9 == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration v9 tx: %w", err)
		}
		defer tx.Rollback()

		schemaV9SQL := `
		CREATE TABLE IF NOT EXISTS dub_script_variants (
			id TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			run_id TEXT NOT NULL,
			job_id TEXT NOT NULL,
			target_language TEXT NOT NULL,
			cas_hash TEXT NOT NULL,
			provenance_hash TEXT NOT NULL,
			provider_id TEXT NOT NULL,
			model_name TEXT NOT NULL,
			model_version TEXT NOT NULL,
			overall_qa_score REAL NOT NULL,
			created_at TEXT NOT NULL
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_dub_script_variants_provenance ON dub_script_variants(provenance_hash);
		CREATE INDEX IF NOT EXISTS idx_dub_script_variants_asset_lang ON dub_script_variants(asset_id, target_language);
		CREATE INDEX IF NOT EXISTS idx_dub_script_variants_run ON dub_script_variants(run_id);

		INSERT INTO schema_migrations (version, applied_at) VALUES (9, datetime('now'));
		`

		if _, err := tx.ExecContext(ctx, schemaV9SQL); err != nil {
			return fmt.Errorf("execute migration v9: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v9: %w", err)
		}
	}

	// 11. Schema migration v10 (Voice Assignments + Dub Segments - T14)
	var countV10 int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 10`).Scan(&countV10)
	if err != nil {
		return fmt.Errorf("check migration version 10: %w", err)
	}

	if countV10 == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration v10 tx: %w", err)
		}
		defer tx.Rollback()

		schemaV10SQL := `
		CREATE TABLE IF NOT EXISTS voice_assignments (
			id TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			run_id TEXT NOT NULL,
			job_id TEXT NOT NULL,
			target_language TEXT NOT NULL,
			cas_hash TEXT NOT NULL,
			provenance_hash TEXT NOT NULL,
			assignments_json TEXT NOT NULL,
			use_same_voice_for_all INTEGER NOT NULL,
			created_at TEXT NOT NULL
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_voice_assignments_asset_run_lang ON voice_assignments(asset_id, run_id, target_language);
		CREATE INDEX IF NOT EXISTS idx_voice_assignments_provenance ON voice_assignments(provenance_hash);
		CREATE INDEX IF NOT EXISTS idx_voice_assignments_asset_lang ON voice_assignments(asset_id, target_language);
		CREATE INDEX IF NOT EXISTS idx_voice_assignments_run ON voice_assignments(run_id);
		CREATE TABLE IF NOT EXISTS dub_segments_variants (
			id TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			run_id TEXT NOT NULL,
			job_id TEXT NOT NULL,
			target_language TEXT NOT NULL,
			cas_hash TEXT NOT NULL,
			provenance_hash TEXT NOT NULL,
			overall_status TEXT NOT NULL,
			created_at TEXT NOT NULL
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_dub_segments_variants_provenance ON dub_segments_variants(provenance_hash);
		CREATE INDEX IF NOT EXISTS idx_dub_segments_variants_asset_lang ON dub_segments_variants(asset_id, target_language);
		CREATE INDEX IF NOT EXISTS idx_dub_segments_variants_run ON dub_segments_variants(run_id);

		INSERT INTO schema_migrations (version, applied_at) VALUES (10, datetime('now'));
		`

		if _, err := tx.ExecContext(ctx, schemaV10SQL); err != nil {
			return fmt.Errorf("execute migration v10: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v10: %w", err)
		}
	}

	// 12. Schema migration v11 (Audio Stems + Dub Mix - T15)
	var countV11 int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 11`).Scan(&countV11)
	if err != nil {
		return fmt.Errorf("check migration version 11: %w", err)
	}

	if countV11 == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration v11 tx: %w", err)
		}
		defer tx.Rollback()

		schemaV11SQL := `
		CREATE TABLE IF NOT EXISTS audio_stems_artifacts (
			id TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			provider_id TEXT NOT NULL,
			model_name TEXT NOT NULL,
			model_version TEXT NOT NULL,
			cas_hash TEXT NOT NULL,
			provenance_hash TEXT NOT NULL,
			created_at TEXT NOT NULL
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_audio_stems_provenance ON audio_stems_artifacts(provenance_hash);
		CREATE INDEX IF NOT EXISTS idx_audio_stems_asset ON audio_stems_artifacts(asset_id);

		CREATE TABLE IF NOT EXISTS dub_mix_artifacts (
			id TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			run_id TEXT NOT NULL,
			job_id TEXT NOT NULL,
			target_language TEXT NOT NULL,
			cas_hash TEXT NOT NULL,
			provenance_hash TEXT NOT NULL,
			overall_status TEXT NOT NULL,
			refusal_reason TEXT,
			created_at TEXT NOT NULL
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_dub_mix_provenance ON dub_mix_artifacts(provenance_hash);
		CREATE INDEX IF NOT EXISTS idx_dub_mix_asset_lang ON dub_mix_artifacts(asset_id, target_language);
		CREATE INDEX IF NOT EXISTS idx_dub_mix_run ON dub_mix_artifacts(run_id);

		INSERT INTO schema_migrations (version, applied_at) VALUES (11, datetime('now'));
		`

		if _, err := tx.ExecContext(ctx, schemaV11SQL); err != nil {
			return fmt.Errorf("execute migration v11: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v11: %w", err)
		}
	}

	// 13. Schema migration v12 (TextRegionPlan artifacts - T09)
	var countV12 int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 12`).Scan(&countV12)
	if err != nil {
		return fmt.Errorf("check migration version 12: %w", err)
	}

	if countV12 == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration v12 tx: %w", err)
		}
		defer tx.Rollback()

		schemaV12SQL := `
		CREATE TABLE IF NOT EXISTS text_region_plans (
			id TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			provider_id TEXT NOT NULL,
			model_name TEXT NOT NULL,
			model_version TEXT NOT NULL,
			cas_hash TEXT NOT NULL,
			provenance_hash TEXT NOT NULL,
			created_at TEXT NOT NULL
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_text_region_plans_provenance ON text_region_plans(provenance_hash);
		CREATE INDEX IF NOT EXISTS idx_text_region_plans_asset ON text_region_plans(asset_id);

		INSERT INTO schema_migrations (version, applied_at) VALUES (12, datetime('now'));
		`

		if _, err := tx.ExecContext(ctx, schemaV12SQL); err != nil {
			return fmt.Errorf("execute migration v12: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v12: %w", err)
		}
	}
	// 14. Schema migration v13 (Render Plans + Render Artifacts - T11)
	var countV13 int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 13`).Scan(&countV13)
	if err != nil {
		return fmt.Errorf("check migration version 12: %w", err)
	}

	if countV13 == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration v13 tx: %w", err)
		}
		defer tx.Rollback()

		schemaV13SQL := `
		CREATE TABLE IF NOT EXISTS render_plans (
			id TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			run_id TEXT NOT NULL,
			job_id TEXT NOT NULL,
			target_language TEXT NOT NULL,
			cas_hash TEXT NOT NULL,
			provenance_hash TEXT NOT NULL,
			created_at TEXT NOT NULL
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_render_plans_provenance ON render_plans(provenance_hash);
		CREATE INDEX IF NOT EXISTS idx_render_plans_asset_lang ON render_plans(asset_id, target_language);
		CREATE INDEX IF NOT EXISTS idx_render_plans_run ON render_plans(run_id);

		CREATE TABLE IF NOT EXISTS render_artifacts (
			id TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			run_id TEXT NOT NULL,
			job_id TEXT NOT NULL,
			target_language TEXT NOT NULL,
			kind TEXT NOT NULL CHECK(kind IN ('preview', 'final')),
			plan_provenance TEXT NOT NULL,
			plan_cas_hash TEXT NOT NULL,
			output_cas_hash TEXT NOT NULL,
			cas_hash TEXT NOT NULL,
			provenance_hash TEXT NOT NULL,
			overall_status TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_render_artifacts_provenance ON render_artifacts(provenance_hash);
		CREATE INDEX IF NOT EXISTS idx_render_artifacts_asset_lang_kind ON render_artifacts(asset_id, target_language, kind);
		CREATE INDEX IF NOT EXISTS idx_render_artifacts_run ON render_artifacts(run_id);

		INSERT INTO schema_migrations (version, applied_at) VALUES (13, datetime('now'));
		`

		if _, err := tx.ExecContext(ctx, schemaV13SQL); err != nil {
			return fmt.Errorf("execute migration v13: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v13: %w", err)
		}
	}

	// 15. Schema migration v14 (Visual Text Tracks & Localized Subtitle Tracks - T10)
	var countV14 int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 14`).Scan(&countV14)
	if err != nil {
		return fmt.Errorf("check migration version 14: %w", err)
	}

	if countV14 == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration v14 tx: %w", err)
		}
		defer tx.Rollback()

		schemaV14SQL := `
		CREATE TABLE IF NOT EXISTS localized_subtitle_tracks (
			id TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			run_id TEXT NOT NULL,
			job_id TEXT NOT NULL,
			target_language TEXT NOT NULL,
			cas_hash TEXT NOT NULL,
			provenance_hash TEXT NOT NULL,
			cue_count INTEGER NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_localized_subtitle_tracks_provenance ON localized_subtitle_tracks(provenance_hash);
		CREATE INDEX IF NOT EXISTS idx_localized_subtitle_tracks_asset_lang ON localized_subtitle_tracks(asset_id, target_language);
		CREATE INDEX IF NOT EXISTS idx_localized_subtitle_tracks_run ON localized_subtitle_tracks(run_id);

		CREATE TABLE IF NOT EXISTS localized_visual_tracks (
			id TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			run_id TEXT NOT NULL,
			job_id TEXT NOT NULL,
			target_language TEXT NOT NULL,
			text_region_plan_cas TEXT NOT NULL,
			cas_hash TEXT NOT NULL,
			provenance_hash TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_localized_visual_tracks_provenance ON localized_visual_tracks(provenance_hash);
		CREATE INDEX IF NOT EXISTS idx_localized_visual_tracks_asset_lang ON localized_visual_tracks(asset_id, target_language);
		CREATE INDEX IF NOT EXISTS idx_localized_visual_tracks_run ON localized_visual_tracks(run_id);

		INSERT INTO schema_migrations (version, applied_at) VALUES (14, datetime('now'));
		`

		if _, err := tx.ExecContext(ctx, schemaV14SQL); err != nil {
			return fmt.Errorf("execute migration v14: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v14: %w", err)
		}
	}

	// 16. Schema migration v15 (Review Overrides + Multimodal Quality Results - T19)
	var countV15 int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 15`).Scan(&countV15)
	if err != nil {
		return fmt.Errorf("check migration version 15: %w", err)
	}

	if countV15 == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration v15 tx: %w", err)
		}
		defer tx.Rollback()

		schemaV15SQL := `
		CREATE TABLE IF NOT EXISTS review_overrides (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL,
			job_id TEXT NOT NULL,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			target_language TEXT NOT NULL,
			review_item_id TEXT NOT NULL,
			item_type TEXT NOT NULL,
			stage TEXT NOT NULL,
			item_index INTEGER NOT NULL,
			segment_id TEXT,
			region_id TEXT,
			action TEXT NOT NULL,
			reason TEXT NOT NULL,
			operator TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_review_overrides_asset_lang ON review_overrides(asset_id, target_language);
		CREATE INDEX IF NOT EXISTS idx_review_overrides_item ON review_overrides(review_item_id);
		CREATE INDEX IF NOT EXISTS idx_review_overrides_run ON review_overrides(run_id);

		CREATE TABLE IF NOT EXISTS quality_results (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL,
			job_id TEXT NOT NULL,
			asset_id TEXT NOT NULL REFERENCES source_assets(id) ON DELETE CASCADE,
			target_language TEXT NOT NULL,
			stage TEXT NOT NULL,
			overall_status TEXT NOT NULL,
			metrics_json TEXT NOT NULL,
			issues_json TEXT NOT NULL,
			details_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_quality_results_asset_lang ON quality_results(asset_id, target_language);
		CREATE INDEX IF NOT EXISTS idx_quality_results_run ON quality_results(run_id);
		CREATE INDEX IF NOT EXISTS idx_quality_results_stage ON quality_results(stage);

		INSERT INTO schema_migrations (version, applied_at) VALUES (15, datetime('now'));
		`

		if _, err := tx.ExecContext(ctx, schemaV15SQL); err != nil {
			return fmt.Errorf("execute migration v15: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v15: %w", err)
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
			fingerprint_match, normalized_audio_sha256, normalized_audio_cas_path,
			errors_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			normalized_audio_sha256=excluded.normalized_audio_sha256,
			normalized_audio_cas_path=excluded.normalized_audio_cas_path,
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
		pr.NormalizedAudioSHA256,
		pr.NormalizedAudioCASPath,
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
		       fingerprint_match, normalized_audio_sha256, normalized_audio_cas_path,
		       errors_json, created_at
		FROM preflight_reports WHERE asset_id = ?
	`
	row := s.db.QueryRowContext(ctx, query, assetID)

	var pr domain.PreflightReport
	var validInt, fpInt int
	var errorsJSON sql.NullString
	var createdStr string
	var vCodec, aCodec, cFmt, normSHA, normPath sql.NullString

	err := row.Scan(
		&pr.ID, &pr.AssetID, &pr.DurationSec, &pr.DurationMs, &vCodec, &aCodec,
		&pr.Width, &pr.Height, &pr.FrameRate, &pr.AudioChannels, &pr.AudioSampleRate,
		&pr.AudioBitRate, &pr.VideoBitRate, &cFmt, &validInt,
		&fpInt, &normSHA, &normPath, &errorsJSON, &createdStr,
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
	if normSHA.Valid {
		pr.NormalizedAudioSHA256 = normSHA.String
	}
	if normPath.Valid {
		pr.NormalizedAudioCASPath = normPath.String
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

// TranslationVariantIndex is the SQLite index row for a content-addressed
// TranslationVariant. The full artifact JSON blob lives in CAS; SQLite indexes
// it by asset, target language, run, and deterministic provenance identity.
type TranslationVariantIndex struct {
	ID             string
	AssetID        string
	RunID          string
	JobID          string
	TargetLanguage string
	CASHash        string
	ProvenanceHash string
	ProviderID     string
	ModelName      string
	ModelVersion   string
	OverallQAScore float64
	CreatedAt      time.Time
}

// SaveTranslationVariantIndex records the index row for a CAS-stored TranslationVariant.
func (s *DB) SaveTranslationVariantIndex(ctx context.Context, idx TranslationVariantIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		INSERT INTO translation_variants (
			id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
			provider_id, model_name, model_version, overall_qa_score, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provenance_hash) DO UPDATE SET
			cas_hash = excluded.cas_hash,
			overall_qa_score = excluded.overall_qa_score
	`
	_, err := s.db.ExecContext(ctx, query,
		idx.ID,
		idx.AssetID,
		idx.RunID,
		idx.JobID,
		idx.TargetLanguage,
		idx.CASHash,
		idx.ProvenanceHash,
		idx.ProviderID,
		idx.ModelName,
		idx.ModelVersion,
		idx.OverallQAScore,
		idx.CreatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save translation_variant index: %w", err)
	}
	return nil
}

// GetTranslationVariantIndex retrieves the latest index row for an asset and target language.
func (s *DB) GetTranslationVariantIndex(ctx context.Context, assetID string, targetLang string) (*TranslationVariantIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx TranslationVariantIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
		provider_id, model_name, model_version, overall_qa_score, created_at
		FROM translation_variants WHERE asset_id = ? AND target_language = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, assetID, targetLang).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.ProviderID,
		&idx.ModelName,
		&idx.ModelVersion,
		&idx.OverallQAScore,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query translation_variant index: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// GetTranslationVariantByProvenance retrieves the index row for a deterministic provenance identity.
func (s *DB) GetTranslationVariantByProvenance(ctx context.Context, provenanceHash string) (*TranslationVariantIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx TranslationVariantIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
		provider_id, model_name, model_version, overall_qa_score, created_at
		FROM translation_variants WHERE provenance_hash = ? LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, provenanceHash).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.ProviderID,
		&idx.ModelName,
		&idx.ModelVersion,
		&idx.OverallQAScore,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query translation_variant by provenance: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// DubScriptVariantIndex captures the SQLite indexing metadata for a persisted DubScriptVariant.
type DubScriptVariantIndex struct {
	ID             string
	AssetID        string
	RunID          string
	JobID          string
	TargetLanguage string
	CASHash        string
	ProvenanceHash string
	ProviderID     string
	ModelName      string
	ModelVersion   string
	OverallQAScore float64
	CreatedAt      time.Time
}

// SaveDubScriptVariantIndex records the index row for a CAS-stored DubScriptVariant.
func (s *DB) SaveDubScriptVariantIndex(ctx context.Context, idx DubScriptVariantIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		INSERT INTO dub_script_variants (
			id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
			provider_id, model_name, model_version, overall_qa_score, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provenance_hash) DO UPDATE SET
			cas_hash = excluded.cas_hash,
			overall_qa_score = excluded.overall_qa_score
	`
	_, err := s.db.ExecContext(ctx, query,
		idx.ID,
		idx.AssetID,
		idx.RunID,
		idx.JobID,
		idx.TargetLanguage,
		idx.CASHash,
		idx.ProvenanceHash,
		idx.ProviderID,
		idx.ModelName,
		idx.ModelVersion,
		idx.OverallQAScore,
		idx.CreatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save dub_script_variant index: %w", err)
	}
	return nil
}

// GetDubScriptVariantIndex retrieves the latest index row for an asset and target language.
func (s *DB) GetDubScriptVariantIndex(ctx context.Context, assetID string, targetLang string) (*DubScriptVariantIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx DubScriptVariantIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
		provider_id, model_name, model_version, overall_qa_score, created_at
		FROM dub_script_variants WHERE asset_id = ? AND target_language = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, assetID, targetLang).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.ProviderID,
		&idx.ModelName,
		&idx.ModelVersion,
		&idx.OverallQAScore,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query dub_script_variant index: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// GetDubScriptVariantByProvenance retrieves the index row for a deterministic provenance identity.
func (s *DB) GetDubScriptVariantByProvenance(ctx context.Context, provenanceHash string) (*DubScriptVariantIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx DubScriptVariantIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
		provider_id, model_name, model_version, overall_qa_score, created_at
		FROM dub_script_variants WHERE provenance_hash = ? LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, provenanceHash).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.ProviderID,
		&idx.ModelName,
		&idx.ModelVersion,
		&idx.OverallQAScore,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query dub_script_variant by provenance: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// VoiceAssignmentIndex captures the SQLite indexing metadata for a persisted VoiceAssignment.
type VoiceAssignmentIndex struct {
	ID                 string
	AssetID            string
	RunID              string
	JobID              string
	TargetLanguage     string
	CASHash            string
	ProvenanceHash     string
	AssignmentsJSON    string
	UseSameVoiceForAll bool
	CreatedAt          time.Time
}

// SaveVoiceAssignmentIndex records the index row for a CAS-stored VoiceAssignment.
func (s *DB) SaveVoiceAssignmentIndex(ctx context.Context, idx VoiceAssignmentIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sameVoiceInt := 0
	if idx.UseSameVoiceForAll {
		sameVoiceInt = 1
	}
	query := `
		INSERT INTO voice_assignments (
			id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
			assignments_json, use_same_voice_for_all, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(asset_id, run_id, target_language) DO UPDATE SET
			cas_hash = excluded.cas_hash,
			provenance_hash = excluded.provenance_hash,
			assignments_json = excluded.assignments_json,
			use_same_voice_for_all = excluded.use_same_voice_for_all
	`
	_, err := s.db.ExecContext(ctx, query,
		idx.ID,
		idx.AssetID,
		idx.RunID,
		idx.JobID,
		idx.TargetLanguage,
		idx.CASHash,
		idx.ProvenanceHash,
		idx.AssignmentsJSON,
		sameVoiceInt,
		idx.CreatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save voice_assignment index: %w", err)
	}
	return nil
}

// GetVoiceAssignmentIndexByRun retrieves the index row for a specific asset, run, and target language.
func (s *DB) GetVoiceAssignmentIndexByRun(ctx context.Context, assetID, runID, targetLang string) (*VoiceAssignmentIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx VoiceAssignmentIndex
	var createdStr string
	var sameVoiceInt int
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
		assignments_json, use_same_voice_for_all, created_at
		FROM voice_assignments WHERE asset_id = ? AND run_id = ? AND target_language = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, assetID, runID, targetLang).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.AssignmentsJSON,
		&sameVoiceInt,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query voice_assignment index by run: %w", err)
	}
	idx.UseSameVoiceForAll = (sameVoiceInt == 1)
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// GetVoiceAssignmentIndex retrieves the latest index row for an asset and target language.
func (s *DB) GetVoiceAssignmentIndex(ctx context.Context, assetID string, targetLang string) (*VoiceAssignmentIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx VoiceAssignmentIndex
	var createdStr string
	var sameVoiceInt int
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
		assignments_json, use_same_voice_for_all, created_at
		FROM voice_assignments WHERE asset_id = ? AND target_language = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, assetID, targetLang).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.AssignmentsJSON,
		&sameVoiceInt,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query voice_assignment index: %w", err)
	}
	idx.UseSameVoiceForAll = (sameVoiceInt == 1)
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// GetVoiceAssignmentByProvenance retrieves the index row for a deterministic provenance identity.
func (s *DB) GetVoiceAssignmentByProvenance(ctx context.Context, provenanceHash string) (*VoiceAssignmentIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx VoiceAssignmentIndex
	var createdStr string
	var sameVoiceInt int
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
		assignments_json, use_same_voice_for_all, created_at
		FROM voice_assignments WHERE provenance_hash = ? LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, provenanceHash).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.AssignmentsJSON,
		&sameVoiceInt,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query voice_assignment by provenance: %w", err)
	}
	idx.UseSameVoiceForAll = (sameVoiceInt == 1)
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// DubSegmentsVariantIndex captures the SQLite indexing metadata for a persisted DubSegmentsVariant.
type DubSegmentsVariantIndex struct {
	ID             string
	AssetID        string
	RunID          string
	JobID          string
	TargetLanguage string
	CASHash        string
	ProvenanceHash string
	OverallStatus  string
	CreatedAt      time.Time
}

// SaveDubSegmentsVariantIndex records the index row for a CAS-stored DubSegmentsVariant.
func (s *DB) SaveDubSegmentsVariantIndex(ctx context.Context, idx DubSegmentsVariantIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		INSERT INTO dub_segments_variants (
			id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
			overall_status, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provenance_hash) DO UPDATE SET
			cas_hash = excluded.cas_hash,
			overall_status = excluded.overall_status
	`
	_, err := s.db.ExecContext(ctx, query,
		idx.ID,
		idx.AssetID,
		idx.RunID,
		idx.JobID,
		idx.TargetLanguage,
		idx.CASHash,
		idx.ProvenanceHash,
		idx.OverallStatus,
		func() string {
			if idx.CreatedAt.IsZero() {
				return time.Now().UTC().Format(time.RFC3339Nano)
			}
			return idx.CreatedAt.Format(time.RFC3339Nano)
		}(),
	)
	if err != nil {
		return fmt.Errorf("save dub_segments_variant index: %w", err)
	}
	return nil
}

// GetDubSegmentsVariantIndex retrieves the latest index row for an asset and target language.
func (s *DB) GetDubSegmentsVariantIndex(ctx context.Context, assetID string, targetLang string) (*DubSegmentsVariantIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx DubSegmentsVariantIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
		overall_status, created_at
		FROM dub_segments_variants WHERE asset_id = ? AND target_language = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, assetID, targetLang).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.OverallStatus,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query dub_segments_variant index: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// GetDubSegmentsVariantByProvenance retrieves the index row for a deterministic provenance identity.
func (s *DB) GetDubSegmentsVariantByProvenance(ctx context.Context, provenanceHash string) (*DubSegmentsVariantIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx DubSegmentsVariantIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
		overall_status, created_at
		FROM dub_segments_variants WHERE provenance_hash = ? LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, provenanceHash).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.OverallStatus,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query dub_segments_variant by provenance: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// AudioStemsArtifactIndex captures the SQLite indexing metadata for a persisted AudioStemArtifacts.
type AudioStemsArtifactIndex struct {
	ID             string
	AssetID        string
	ProviderID     string
	ModelName      string
	ModelVersion   string
	CASHash        string
	ProvenanceHash string
	CreatedAt      time.Time
}

// SaveAudioStemsArtifactIndex records the index row for a CAS-stored AudioStemArtifacts.
func (s *DB) SaveAudioStemsArtifactIndex(ctx context.Context, idx AudioStemsArtifactIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		INSERT INTO audio_stems_artifacts (
			id, asset_id, provider_id, model_name, model_version, cas_hash, provenance_hash, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provenance_hash) DO UPDATE SET
			cas_hash = excluded.cas_hash
	`
	_, err := s.db.ExecContext(ctx, query,
		idx.ID,
		idx.AssetID,
		idx.ProviderID,
		idx.ModelName,
		idx.ModelVersion,
		idx.CASHash,
		idx.ProvenanceHash,
		func() string {
			if idx.CreatedAt.IsZero() {
				return time.Now().UTC().Format(time.RFC3339Nano)
			}
			return idx.CreatedAt.Format(time.RFC3339Nano)
		}(),
	)
	if err != nil {
		return fmt.Errorf("save audio_stems_artifacts index: %w", err)
	}
	return nil
}

// GetAudioStemsArtifactIndex retrieves the latest index row for an asset.
func (s *DB) GetAudioStemsArtifactIndex(ctx context.Context, assetID string) (*AudioStemsArtifactIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx AudioStemsArtifactIndex
	var createdStr string
	query := `SELECT id, asset_id, provider_id, model_name, model_version, cas_hash, provenance_hash, created_at
		FROM audio_stems_artifacts WHERE asset_id = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, assetID).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.ProviderID,
		&idx.ModelName,
		&idx.ModelVersion,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query audio_stems_artifacts index: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// GetAudioStemsArtifactByProvenance retrieves the index row by provenance hash.
func (s *DB) GetAudioStemsArtifactByProvenance(ctx context.Context, provenanceHash string) (*AudioStemsArtifactIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx AudioStemsArtifactIndex
	var createdStr string
	query := `SELECT id, asset_id, provider_id, model_name, model_version, cas_hash, provenance_hash, created_at
		FROM audio_stems_artifacts WHERE provenance_hash = ? LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, provenanceHash).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.ProviderID,
		&idx.ModelName,
		&idx.ModelVersion,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query audio_stems_artifacts by provenance: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// DubMixArtifactIndex captures the SQLite indexing metadata for a persisted DubMixArtifact.
type DubMixArtifactIndex struct {
	ID             string
	AssetID        string
	RunID          string
	JobID          string
	TargetLanguage string
	CASHash        string
	ProvenanceHash string
	OverallStatus  string
	RefusalReason  string
	CreatedAt      time.Time
}

// SaveDubMixArtifactIndex records the index row for a CAS-stored DubMixArtifact.
func (s *DB) SaveDubMixArtifactIndex(ctx context.Context, idx DubMixArtifactIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		INSERT INTO dub_mix_artifacts (
			id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
			overall_status, refusal_reason, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provenance_hash) DO UPDATE SET
			cas_hash = excluded.cas_hash,
			overall_status = excluded.overall_status,
			refusal_reason = excluded.refusal_reason
	`
	_, err := s.db.ExecContext(ctx, query,
		idx.ID,
		idx.AssetID,
		idx.RunID,
		idx.JobID,
		idx.TargetLanguage,
		idx.CASHash,
		idx.ProvenanceHash,
		idx.OverallStatus,
		idx.RefusalReason,
		func() string {
			if idx.CreatedAt.IsZero() {
				return time.Now().UTC().Format(time.RFC3339Nano)
			}
			return idx.CreatedAt.Format(time.RFC3339Nano)
		}(),
	)
	if err != nil {
		return fmt.Errorf("save dub_mix_artifacts index: %w", err)
	}
	return nil
}

// GetDubMixArtifactIndex retrieves the latest index row for an asset and target language.
func (s *DB) GetDubMixArtifactIndex(ctx context.Context, assetID string, targetLang string) (*DubMixArtifactIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx DubMixArtifactIndex
	var createdStr string
	var refusalReason sql.NullString
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
		overall_status, refusal_reason, created_at
		FROM dub_mix_artifacts WHERE asset_id = ? AND target_language = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, assetID, targetLang).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.OverallStatus,
		&refusalReason,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query dub_mix_artifacts index: %w", err)
	}
	if refusalReason.Valid {
		idx.RefusalReason = refusalReason.String
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// GetDubMixArtifactByProvenance retrieves the index row by provenance hash.
func (s *DB) GetDubMixArtifactByProvenance(ctx context.Context, provenanceHash string) (*DubMixArtifactIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx DubMixArtifactIndex
	var createdStr string
	var refusalReason sql.NullString
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash,
		overall_status, refusal_reason, created_at
		FROM dub_mix_artifacts WHERE provenance_hash = ? LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, provenanceHash).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.OverallStatus,
		&refusalReason,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query dub_mix_artifacts by provenance: %w", err)
	}
	if refusalReason.Valid {
		idx.RefusalReason = refusalReason.String
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// TextRegionPlanIndex captures the SQLite indexing metadata for a persisted TextRegionPlan.
type TextRegionPlanIndex struct {
	ID             string
	AssetID        string
	ProviderID     string
	ModelName      string
	ModelVersion   string
	CASHash        string
	ProvenanceHash string
	CreatedAt      time.Time
}

// SaveTextRegionPlanIndex records the index row for a CAS-stored TextRegionPlan.
func (s *DB) SaveTextRegionPlanIndex(ctx context.Context, idx TextRegionPlanIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		INSERT INTO text_region_plans (
			id, asset_id, provider_id, model_name, model_version, cas_hash, provenance_hash, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provenance_hash) DO UPDATE SET
			cas_hash = excluded.cas_hash
	`
	_, err := s.db.ExecContext(ctx, query,
		idx.ID,
		idx.AssetID,
		idx.ProviderID,
		idx.ModelName,
		idx.ModelVersion,
		idx.CASHash,
		idx.ProvenanceHash,
		func() string {
			if idx.CreatedAt.IsZero() {
				return time.Now().UTC().Format(time.RFC3339Nano)
			}
			return idx.CreatedAt.Format(time.RFC3339Nano)
		}(),
	)
	if err != nil {
		return fmt.Errorf("save text_region_plans index: %w", err)
	}
	return nil
}

// GetTextRegionPlanIndex retrieves the latest index row for an asset.
func (s *DB) GetTextRegionPlanIndex(ctx context.Context, assetID string) (*TextRegionPlanIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx TextRegionPlanIndex
	var createdStr string
	query := `SELECT id, asset_id, provider_id, model_name, model_version, cas_hash, provenance_hash, created_at
		FROM text_region_plans WHERE asset_id = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, assetID).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.ProviderID,
		&idx.ModelName,
		&idx.ModelVersion,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query text_region_plans index: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// GetTextRegionPlanByProvenance retrieves the index row by provenance hash.
func (s *DB) GetTextRegionPlanByProvenance(ctx context.Context, provenanceHash string) (*TextRegionPlanIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx TextRegionPlanIndex
	var createdStr string
	query := `SELECT id, asset_id, provider_id, model_name, model_version, cas_hash, provenance_hash, created_at
		FROM text_region_plans WHERE provenance_hash = ? LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, provenanceHash).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.ProviderID,
		&idx.ModelName,
		&idx.ModelVersion,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query text_region_plans by provenance: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// RenderPlanIndex captures SQLite indexing metadata for a persisted RenderPlan.
type RenderPlanIndex struct {
	ID             string
	AssetID        string
	RunID          string
	JobID          string
	TargetLanguage string
	CASHash        string
	ProvenanceHash string
	CreatedAt      time.Time
}

// SaveRenderPlanIndex records the index row for a CAS-stored RenderPlan.
func (s *DB) SaveRenderPlanIndex(ctx context.Context, idx RenderPlanIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		INSERT INTO render_plans (
			id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provenance_hash) DO UPDATE SET
			cas_hash = excluded.cas_hash
	`
	_, err := s.db.ExecContext(ctx, query,
		idx.ID,
		idx.AssetID,
		idx.RunID,
		idx.JobID,
		idx.TargetLanguage,
		idx.CASHash,
		idx.ProvenanceHash,
		func() string {
			if idx.CreatedAt.IsZero() {
				return time.Now().UTC().Format(time.RFC3339Nano)
			}
			return idx.CreatedAt.Format(time.RFC3339Nano)
		}(),
	)
	if err != nil {
		return fmt.Errorf("save render_plans index: %w", err)
	}
	return nil
}

// GetRenderPlanIndex retrieves the latest index row for an asset and target language.
func (s *DB) GetRenderPlanIndex(ctx context.Context, assetID string, targetLang string) (*RenderPlanIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx RenderPlanIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash, created_at
		FROM render_plans WHERE asset_id = ? AND target_language = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, assetID, targetLang).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query render_plans index: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// GetRenderPlanByProvenance retrieves the index row by provenance hash.
func (s *DB) GetRenderPlanByProvenance(ctx context.Context, provenanceHash string) (*RenderPlanIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx RenderPlanIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash, created_at
		FROM render_plans WHERE provenance_hash = ? LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, provenanceHash).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query render_plans by provenance: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// RenderArtifactIndex captures SQLite indexing metadata for a persisted preview or final render artifact.
type RenderArtifactIndex struct {
	ID             string
	AssetID        string
	RunID          string
	JobID          string
	TargetLanguage string
	Kind           string // "preview" | "final"
	PlanProvenance string
	PlanCASHash    string
	OutputCASHash  string
	CASHash        string
	ProvenanceHash string
	OverallStatus  string
	CreatedAt      time.Time
}

// SaveRenderArtifactIndex records the index row for a CAS-stored render artifact.
func (s *DB) SaveRenderArtifactIndex(ctx context.Context, idx RenderArtifactIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		INSERT INTO render_artifacts (
			id, asset_id, run_id, job_id, target_language, kind,
			plan_provenance, plan_cas_hash, output_cas_hash, cas_hash, provenance_hash,
			overall_status, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provenance_hash) DO UPDATE SET
			output_cas_hash = excluded.output_cas_hash,
			cas_hash = excluded.cas_hash,
			overall_status = excluded.overall_status
	`
	_, err := s.db.ExecContext(ctx, query,
		idx.ID,
		idx.AssetID,
		idx.RunID,
		idx.JobID,
		idx.TargetLanguage,
		idx.Kind,
		idx.PlanProvenance,
		idx.PlanCASHash,
		idx.OutputCASHash,
		idx.CASHash,
		idx.ProvenanceHash,
		idx.OverallStatus,
		func() string {
			if idx.CreatedAt.IsZero() {
				return time.Now().UTC().Format(time.RFC3339Nano)
			}
			return idx.CreatedAt.Format(time.RFC3339Nano)
		}(),
	)
	if err != nil {
		return fmt.Errorf("save render_artifacts index: %w", err)
	}
	return nil
}

// GetLatestRenderArtifactIndex retrieves the latest index row for an asset, target language, and kind.
func (s *DB) GetLatestRenderArtifactIndex(ctx context.Context, assetID string, targetLang string, kind string) (*RenderArtifactIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx RenderArtifactIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, job_id, target_language, kind,
		plan_provenance, plan_cas_hash, output_cas_hash, cas_hash, provenance_hash, overall_status, created_at
		FROM render_artifacts WHERE asset_id = ? AND target_language = ? AND kind = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, assetID, targetLang, kind).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.Kind,
		&idx.PlanProvenance,
		&idx.PlanCASHash,
		&idx.OutputCASHash,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.OverallStatus,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query render_artifacts index: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// GetRenderArtifactByProvenance retrieves the index row by provenance hash.
func (s *DB) GetRenderArtifactByProvenance(ctx context.Context, provenanceHash string) (*RenderArtifactIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx RenderArtifactIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, job_id, target_language, kind,
		plan_provenance, plan_cas_hash, output_cas_hash, cas_hash, provenance_hash, overall_status, created_at
		FROM render_artifacts WHERE provenance_hash = ? LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, provenanceHash).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.Kind,
		&idx.PlanProvenance,
		&idx.PlanCASHash,
		&idx.OutputCASHash,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.OverallStatus,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query render_artifacts by provenance: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// LocalizedSubtitleTrackIndex captures SQLite indexing metadata for a persisted LocalizedSubtitleTrack.
type LocalizedSubtitleTrackIndex struct {
	ID             string
	AssetID        string
	RunID          string
	JobID          string
	TargetLanguage string
	CASHash        string
	ProvenanceHash string
	CueCount       int
	CreatedAt      time.Time
}

// SaveLocalizedSubtitleTrackIndex records the index row for a CAS-stored LocalizedSubtitleTrack.
func (s *DB) SaveLocalizedSubtitleTrackIndex(ctx context.Context, idx LocalizedSubtitleTrackIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		INSERT INTO localized_subtitle_tracks (
			id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash, cue_count, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provenance_hash) DO UPDATE SET
			cas_hash = excluded.cas_hash,
			cue_count = excluded.cue_count
	`
	_, err := s.db.ExecContext(ctx, query,
		idx.ID,
		idx.AssetID,
		idx.RunID,
		idx.JobID,
		idx.TargetLanguage,
		idx.CASHash,
		idx.ProvenanceHash,
		idx.CueCount,
		func() string {
			if idx.CreatedAt.IsZero() {
				return time.Now().UTC().Format(time.RFC3339Nano)
			}
			return idx.CreatedAt.Format(time.RFC3339Nano)
		}(),
	)
	if err != nil {
		return fmt.Errorf("save localized_subtitle_tracks index: %w", err)
	}
	return nil
}

// GetLocalizedSubtitleTrackIndex retrieves the latest index row for an asset and target language.
func (s *DB) GetLocalizedSubtitleTrackIndex(ctx context.Context, assetID, targetLang string) (*LocalizedSubtitleTrackIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx LocalizedSubtitleTrackIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash, cue_count, created_at
		FROM localized_subtitle_tracks WHERE asset_id = ? AND target_language = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, assetID, targetLang).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.CueCount,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query localized_subtitle_tracks index: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// GetLocalizedSubtitleTrackByProvenance retrieves the index row by provenance hash.
func (s *DB) GetLocalizedSubtitleTrackByProvenance(ctx context.Context, provenanceHash string) (*LocalizedSubtitleTrackIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx LocalizedSubtitleTrackIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, job_id, target_language, cas_hash, provenance_hash, cue_count, created_at
		FROM localized_subtitle_tracks WHERE provenance_hash = ? LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, provenanceHash).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&idx.CueCount,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query localized_subtitle_tracks by provenance: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// LocalizedVisualTrackIndex captures SQLite indexing metadata for a persisted LocalizedVisualTrack.
type LocalizedVisualTrackIndex struct {
	ID                string
	AssetID           string
	RunID             string
	JobID             string
	TargetLanguage    string
	TextRegionPlanCAS string
	CASHash           string
	ProvenanceHash    string
	CreatedAt         time.Time
}

// SaveLocalizedVisualTrackIndex records the index row for a CAS-stored LocalizedVisualTrack.
func (s *DB) SaveLocalizedVisualTrackIndex(ctx context.Context, idx LocalizedVisualTrackIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		INSERT INTO localized_visual_tracks (
			id, asset_id, run_id, job_id, target_language, text_region_plan_cas, cas_hash, provenance_hash, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provenance_hash) DO UPDATE SET
			cas_hash = excluded.cas_hash,
			text_region_plan_cas = excluded.text_region_plan_cas
	`
	_, err := s.db.ExecContext(ctx, query,
		idx.ID,
		idx.AssetID,
		idx.RunID,
		idx.JobID,
		idx.TargetLanguage,
		idx.TextRegionPlanCAS,
		idx.CASHash,
		idx.ProvenanceHash,
		func() string {
			if idx.CreatedAt.IsZero() {
				return time.Now().UTC().Format(time.RFC3339Nano)
			}
			return idx.CreatedAt.Format(time.RFC3339Nano)
		}(),
	)
	if err != nil {
		return fmt.Errorf("save localized_visual_tracks index: %w", err)
	}
	return nil
}

// GetLocalizedVisualTrackIndex retrieves the latest index row for an asset and target language.
func (s *DB) GetLocalizedVisualTrackIndex(ctx context.Context, assetID, targetLang string) (*LocalizedVisualTrackIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx LocalizedVisualTrackIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, job_id, target_language, text_region_plan_cas, cas_hash, provenance_hash, created_at
		FROM localized_visual_tracks WHERE asset_id = ? AND target_language = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, assetID, targetLang).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.TextRegionPlanCAS,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query localized_visual_tracks index: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// GetLocalizedVisualTrackByProvenance retrieves the index row by provenance hash.
func (s *DB) GetLocalizedVisualTrackByProvenance(ctx context.Context, provenanceHash string) (*LocalizedVisualTrackIndex, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var idx LocalizedVisualTrackIndex
	var createdStr string
	query := `SELECT id, asset_id, run_id, job_id, target_language, text_region_plan_cas, cas_hash, provenance_hash, created_at
		FROM localized_visual_tracks WHERE provenance_hash = ? LIMIT 1`

	err := s.db.QueryRowContext(ctx, query, provenanceHash).Scan(
		&idx.ID,
		&idx.AssetID,
		&idx.RunID,
		&idx.JobID,
		&idx.TargetLanguage,
		&idx.TextRegionPlanCAS,
		&idx.CASHash,
		&idx.ProvenanceHash,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query localized_visual_tracks by provenance: %w", err)
	}
	t, _ := time.Parse(time.RFC3339Nano, createdStr)
	idx.CreatedAt = t
	return &idx, nil
}

// SaveReviewOverride records an auditable operator acceptance/override of a flagged exception item.
func (s *DB) SaveReviewOverride(ctx context.Context, ro domain.ReviewOverride) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ro.ID == "" {
		ro.ID = uuid.NewString()
	}
	if ro.CreatedAt.IsZero() {
		ro.CreatedAt = time.Now().UTC()
	}
	if ro.Action == "" {
		ro.Action = "manual_override"
	}

	query := `INSERT INTO review_overrides (id, run_id, job_id, asset_id, target_language, review_item_id, item_type, stage, item_index, segment_id, region_id, action, reason, operator, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, query,
		ro.ID,
		ro.RunID,
		ro.JobID,
		ro.AssetID,
		ro.TargetLanguage,
		ro.ReviewItemID,
		string(ro.ItemType),
		ro.Stage,
		ro.ItemIndex,
		ro.SegmentID,
		ro.RegionID,
		ro.Action,
		ro.Reason,
		ro.Operator,
		ro.CreatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert review_override: %w", err)
	}
	return nil
}

// GetReviewOverrides returns all recorded overrides for an asset and target language.
func (s *DB) GetReviewOverrides(ctx context.Context, assetID, targetLang string) ([]domain.ReviewOverride, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, run_id, job_id, asset_id, target_language, review_item_id, item_type, stage, item_index, segment_id, region_id, action, reason, operator, created_at
		FROM review_overrides WHERE asset_id = ? AND target_language = ? ORDER BY created_at ASC`

	rows, err := s.db.QueryContext(ctx, query, assetID, targetLang)
	if err != nil {
		return nil, fmt.Errorf("query review_overrides: %w", err)
	}
	defer rows.Close()

	var overrides []domain.ReviewOverride
	for rows.Next() {
		var ro domain.ReviewOverride
		var itemTypeStr, createdStr string
		var segID, regID sql.NullString
		err := rows.Scan(
			&ro.ID,
			&ro.RunID,
			&ro.JobID,
			&ro.AssetID,
			&ro.TargetLanguage,
			&ro.ReviewItemID,
			&itemTypeStr,
			&ro.Stage,
			&ro.ItemIndex,
			&segID,
			&regID,
			&ro.Action,
			&ro.Reason,
			&ro.Operator,
			&createdStr,
		)
		if err != nil {
			return nil, fmt.Errorf("scan review_override: %w", err)
		}
		ro.ItemType = domain.ReviewItemType(itemTypeStr)
		if segID.Valid {
			ro.SegmentID = segID.String
		}
		if regID.Valid {
			ro.RegionID = regID.String
		}
		ro.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		overrides = append(overrides, ro)
	}
	return overrides, nil
}

// GetReviewOverridesByRun returns all recorded overrides for a specific run ID.
func (s *DB) GetReviewOverridesByRun(ctx context.Context, runID string) ([]domain.ReviewOverride, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, run_id, job_id, asset_id, target_language, review_item_id, item_type, stage, item_index, segment_id, region_id, action, reason, operator, created_at
		FROM review_overrides WHERE run_id = ? ORDER BY created_at ASC`

	rows, err := s.db.QueryContext(ctx, query, runID)
	if err != nil {
		return nil, fmt.Errorf("query review_overrides by run: %w", err)
	}
	defer rows.Close()

	var overrides []domain.ReviewOverride
	for rows.Next() {
		var ro domain.ReviewOverride
		var itemTypeStr, createdStr string
		var segID, regID sql.NullString
		err := rows.Scan(
			&ro.ID,
			&ro.RunID,
			&ro.JobID,
			&ro.AssetID,
			&ro.TargetLanguage,
			&ro.ReviewItemID,
			&itemTypeStr,
			&ro.Stage,
			&ro.ItemIndex,
			&segID,
			&regID,
			&ro.Action,
			&ro.Reason,
			&ro.Operator,
			&createdStr,
		)
		if err != nil {
			return nil, fmt.Errorf("scan review_override: %w", err)
		}
		ro.ItemType = domain.ReviewItemType(itemTypeStr)
		if segID.Valid {
			ro.SegmentID = segID.String
		}
		if regID.Valid {
			ro.RegionID = regID.String
		}
		ro.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		overrides = append(overrides, ro)
	}
	return overrides, nil
}

// SaveQualityResult records an append-only multimodal quality evaluation result.
// Invariant: Strictly append-only. Never mutates existing rows or overwrites historical execution state.
func (s *DB) SaveQualityResult(ctx context.Context, qr domain.QualityResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if qr.ID == "" {
		qr.ID = uuid.NewString()
	}
	if qr.CreatedAt.IsZero() {
		qr.CreatedAt = time.Now().UTC()
	}
	if qr.OverallStatus == "" {
		qr.OverallStatus = domain.QualityStatusPass
	}

	metricsJSON, err := json.Marshal(qr.Metrics)
	if err != nil {
		return fmt.Errorf("marshal quality metrics: %w", err)
	}
	issuesJSON, err := json.Marshal(qr.Issues)
	if err != nil {
		return fmt.Errorf("marshal quality issues: %w", err)
	}
	detailsJSON, err := json.Marshal(qr.Details)
	if err != nil {
		return fmt.Errorf("marshal quality details: %w", err)
	}

	query := `INSERT INTO quality_results (id, run_id, job_id, asset_id, target_language, stage, overall_status, metrics_json, issues_json, details_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err = s.db.ExecContext(ctx, query,
		qr.ID,
		qr.RunID,
		qr.JobID,
		qr.AssetID,
		qr.TargetLanguage,
		qr.Stage,
		string(qr.OverallStatus),
		string(metricsJSON),
		string(issuesJSON),
		string(detailsJSON),
		qr.CreatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert quality_result: %w", err)
	}
	return nil
}

// GetQualityResults retrieves append-only quality results for an asset, target language, and optional stage.
func (s *DB) GetQualityResults(ctx context.Context, assetID, targetLang, stage string) ([]domain.QualityResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var query string
	var args []any
	if stage != "" {
		query = `SELECT id, run_id, job_id, asset_id, target_language, stage, overall_status, metrics_json, issues_json, details_json, created_at
			FROM quality_results WHERE asset_id = ? AND target_language = ? AND stage = ? ORDER BY created_at ASC`
		args = []any{assetID, targetLang, stage}
	} else {
		query = `SELECT id, run_id, job_id, asset_id, target_language, stage, overall_status, metrics_json, issues_json, details_json, created_at
			FROM quality_results WHERE asset_id = ? AND target_language = ? ORDER BY created_at ASC`
		args = []any{assetID, targetLang}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query quality_results: %w", err)
	}
	defer rows.Close()

	var results []domain.QualityResult
	for rows.Next() {
		var qr domain.QualityResult
		var statusStr, metricsStr, issuesStr, detailsStr, createdStr string
		err := rows.Scan(
			&qr.ID,
			&qr.RunID,
			&qr.JobID,
			&qr.AssetID,
			&qr.TargetLanguage,
			&qr.Stage,
			&statusStr,
			&metricsStr,
			&issuesStr,
			&detailsStr,
			&createdStr,
		)
		if err != nil {
			return nil, fmt.Errorf("scan quality_result: %w", err)
		}
		qr.OverallStatus = domain.QualityStatus(statusStr)
		_ = json.Unmarshal([]byte(metricsStr), &qr.Metrics)
		_ = json.Unmarshal([]byte(issuesStr), &qr.Issues)
		_ = json.Unmarshal([]byte(detailsStr), &qr.Details)
		qr.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		results = append(results, qr)
	}
	return results, nil
}

// GetLatestQualityResult retrieves the most recent quality result for an asset, target language, and stage.
func (s *DB) GetLatestQualityResult(ctx context.Context, assetID, targetLang, stage string) (*domain.QualityResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, run_id, job_id, asset_id, target_language, stage, overall_status, metrics_json, issues_json, details_json, created_at
		FROM quality_results WHERE asset_id = ? AND target_language = ? AND stage = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`

	row := s.db.QueryRowContext(ctx, query, assetID, targetLang, stage)
	var qr domain.QualityResult
	var statusStr, metricsStr, issuesStr, detailsStr, createdStr string
	err := row.Scan(
		&qr.ID,
		&qr.RunID,
		&qr.JobID,
		&qr.AssetID,
		&qr.TargetLanguage,
		&qr.Stage,
		&statusStr,
		&metricsStr,
		&issuesStr,
		&detailsStr,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query latest quality_result: %w", err)
	}
	qr.OverallStatus = domain.QualityStatus(statusStr)
	_ = json.Unmarshal([]byte(metricsStr), &qr.Metrics)
	_ = json.Unmarshal([]byte(issuesStr), &qr.Issues)
	_ = json.Unmarshal([]byte(detailsStr), &qr.Details)
	qr.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return &qr, nil
}

// GetQualityResultsByRun retrieves all append-only quality results for a run ID.
func (s *DB) GetQualityResultsByRun(ctx context.Context, runID string) ([]domain.QualityResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, run_id, job_id, asset_id, target_language, stage, overall_status, metrics_json, issues_json, details_json, created_at
		FROM quality_results WHERE run_id = ? ORDER BY created_at ASC`

	rows, err := s.db.QueryContext(ctx, query, runID)
	if err != nil {
		return nil, fmt.Errorf("query quality_results by run: %w", err)
	}
	defer rows.Close()

	var results []domain.QualityResult
	for rows.Next() {
		var qr domain.QualityResult
		var statusStr, metricsStr, issuesStr, detailsStr, createdStr string
		err := rows.Scan(
			&qr.ID,
			&qr.RunID,
			&qr.JobID,
			&qr.AssetID,
			&qr.TargetLanguage,
			&qr.Stage,
			&statusStr,
			&metricsStr,
			&issuesStr,
			&detailsStr,
			&createdStr,
		)
		if err != nil {
			return nil, fmt.Errorf("scan quality_result: %w", err)
		}
		qr.OverallStatus = domain.QualityStatus(statusStr)
		_ = json.Unmarshal([]byte(metricsStr), &qr.Metrics)
		_ = json.Unmarshal([]byte(issuesStr), &qr.Issues)
		_ = json.Unmarshal([]byte(detailsStr), &qr.Details)
		qr.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		results = append(results, qr)
	}
	return results, nil
}
