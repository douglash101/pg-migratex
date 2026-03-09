// Package migratex is a minimal, dependency-free SQL migration runner for PostgreSQL.
//
// It uses the Flyway-compatible naming convention for migration files:
//
//	V<version>__<description>.sql
//
// The caller is responsible for embedding the SQL files and passing the fs.FS to New.
// Example:
//
//	//go:embed migrations/*.sql
//	var migrationsFS embed.FS
//
//	m := migratex.New(db, migrationsFS, migratex.WithDir("migrations"))
//	if err := m.Migrate(); err != nil { ... }
package migratex

import (
	"database/sql"
	"fmt"
	"hash/crc32"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var filenamePattern = regexp.MustCompile(`^V(\d+(?:_\d+)?)__(.+)\.sql$`)

// migrationFile represents a parsed migration file.
type migrationFile struct {
	Version     int
	VersionRaw  string
	Description string
	Filename    string
	SQL         string
	Checksum    uint32
}

// MigrationRecord is a row from the history table.
type MigrationRecord struct {
	InstalledRank int
	Version       string
	Description   string
	Script        string
	Checksum      uint32
	InstalledOn   time.Time
	ExecutionTime int64
	Success       bool
}

// Migrator runs pending SQL migrations against a database.
type Migrator struct {
	db           *sql.DB
	migrationsFS fs.FS
	dir          string
	log          *slog.Logger
	historyTable string
}

// New creates a Migrator. migrationsFS must contain *.sql files following the
// V<version>__<description>.sql naming convention.
// By default the root of the FS is scanned; use WithDir to specify a subdirectory.
func New(db *sql.DB, migrationsFS fs.FS, opts ...Option) *Migrator {
	m := &Migrator{
		db:           db,
		migrationsFS: migrationsFS,
		dir:          ".",
		log:          slog.New(slog.NewTextHandler(os.Stderr, nil)),
		historyTable: "schema_migrations",
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Migrate applies all pending migrations in version order.
// Already-applied migrations are checksum-validated; a mismatch is a fatal error.
func (m *Migrator) Migrate() error {
	// 1. Ensure history table exists
	if _, err := m.db.Exec(m.createHistoryTableSQL()); err != nil {
		return fmt.Errorf("failed to create %s table: %w", m.historyTable, err)
	}

	// 2. Load migration files from FS
	files, err := m.loadMigrationFiles()
	if err != nil {
		return fmt.Errorf("failed to load migration files: %w", err)
	}

	if len(files) == 0 {
		m.log.Info("🗂️  No migration files found")
		return nil
	}

	// 3. Load already-applied migrations
	applied, err := m.loadAppliedMigrations()
	if err != nil {
		return fmt.Errorf("failed to query applied migrations: %w", err)
	}

	// 4. Validate checksums and collect pending ones
	var pending []migrationFile
	for _, f := range files {
		rec, wasApplied := applied[f.VersionRaw]
		if wasApplied {
			if !rec.Success {
				return fmt.Errorf("migration V%s (%s) previously failed — resolve it manually before proceeding",
					f.VersionRaw, f.Filename)
			}
			if rec.Checksum != f.Checksum {
				return fmt.Errorf("checksum mismatch for migration V%s (%s): expected %d, got %d — do not edit applied migrations",
					f.VersionRaw, f.Filename, rec.Checksum, f.Checksum)
			}
			continue
		}
		pending = append(pending, f)
	}

	if len(pending) == 0 {
		m.log.Info("✅ Database is up to date — no pending migrations")
		return nil
	}

	// 5. Apply each pending migration inside its own transaction
	for _, mig := range pending {
		if err := m.applyMigration(mig); err != nil {
			return err
		}
	}

	return nil
}

func (m *Migrator) createHistoryTableSQL() string {
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
    installed_rank  SERIAL         PRIMARY KEY,
    version         VARCHAR(50)    NOT NULL UNIQUE,
    description     VARCHAR(200)   NOT NULL,
    script          VARCHAR(300)   NOT NULL,
    checksum        BIGINT         NOT NULL,
    installed_on    TIMESTAMP      NOT NULL DEFAULT CURRENT_TIMESTAMP,
    execution_time  INTEGER        NOT NULL,
    success         BOOLEAN        NOT NULL
);`, m.historyTable)
}

func (m *Migrator) loadMigrationFiles() ([]migrationFile, error) {
	entries, err := fs.ReadDir(m.migrationsFS, m.dir)
	if err != nil {
		return nil, err
	}

	var files []migrationFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		mf, err := m.parseMigrationFile(entry.Name())
		if err != nil {
			return nil, err
		}
		files = append(files, mf)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Version < files[j].Version
	})

	return files, nil
}

func (m *Migrator) parseMigrationFile(filename string) (migrationFile, error) {
	matches := filenamePattern.FindStringSubmatch(filename)
	if matches == nil {
		return migrationFile{}, fmt.Errorf("invalid migration filename %q: expected V<version>__<description>.sql", filename)
	}

	versionRaw := matches[1]
	versionNorm := strings.ReplaceAll(versionRaw, "_", "")
	version, err := strconv.Atoi(versionNorm)
	if err != nil {
		return migrationFile{}, fmt.Errorf("cannot parse version %q in %q: %w", versionRaw, filename, err)
	}

	description := strings.ReplaceAll(matches[2], "_", " ")

	path := filename
	if m.dir != "." {
		path = m.dir + "/" + filename
	}

	content, err := fs.ReadFile(m.migrationsFS, path)
	if err != nil {
		return migrationFile{}, fmt.Errorf("failed to read %q: %w", filename, err)
	}

	return migrationFile{
		Version:     version,
		VersionRaw:  versionRaw,
		Description: description,
		Filename:    filename,
		SQL:         string(content),
		Checksum:    crc32.ChecksumIEEE(content),
	}, nil
}

func (m *Migrator) loadAppliedMigrations() (map[string]MigrationRecord, error) {
	rows, err := m.db.Query(fmt.Sprintf(`
		SELECT installed_rank, version, description, script, checksum, installed_on, execution_time, success
		FROM %s
		ORDER BY installed_rank`, m.historyTable))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]MigrationRecord)
	for rows.Next() {
		var r MigrationRecord
		if err := rows.Scan(
			&r.InstalledRank,
			&r.Version,
			&r.Description,
			&r.Script,
			&r.Checksum,
			&r.InstalledOn,
			&r.ExecutionTime,
			&r.Success,
		); err != nil {
			return nil, err
		}
		result[r.Version] = r
	}
	return result, rows.Err()
}

func (m *Migrator) applyMigration(mig migrationFile) (retErr error) {
	m.log.Info("🔄 Applying migration", "version", mig.VersionRaw, "description", mig.Description)

	tx, err := m.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction for V%s: %w", mig.VersionRaw, err)
	}

	start := time.Now()
	success := false

	defer func() {
		elapsed := time.Since(start).Milliseconds()
		_, recErr := m.db.Exec(fmt.Sprintf(`
			INSERT INTO %s (version, description, script, checksum, execution_time, success)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (version) DO UPDATE SET success = $6, execution_time = $5`, m.historyTable),
			mig.VersionRaw,
			mig.Description,
			mig.Filename,
			mig.Checksum,
			elapsed,
			success,
		)
		if recErr != nil {
			m.log.Error("⚠️  Failed to record migration result", "version", mig.VersionRaw, "error", recErr)
		}

		if !success {
			if rbErr := tx.Rollback(); rbErr != nil {
				m.log.Error("⚠️  Rollback failed", "version", mig.VersionRaw, "error", rbErr)
			}
		}
	}()

	if _, err = tx.Exec(mig.SQL); err != nil {
		retErr = fmt.Errorf("❌ Migration V%s (%s) failed: %w", mig.VersionRaw, mig.Filename, err)
		m.log.Error("Migration failed", "version", mig.VersionRaw, "error", err)
		return retErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit migration V%s: %w", mig.VersionRaw, err)
	}

	success = true
	m.log.Info("✅ Migration applied", "version", mig.VersionRaw, "description", mig.Description)
	return nil
}
