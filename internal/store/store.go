package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"github.com/andrianbdn/oddk/internal/store/auth"
	"github.com/andrianbdn/oddk/internal/store/backup"
	"github.com/andrianbdn/oddk/internal/store/cron"
	"github.com/andrianbdn/oddk/internal/store/health"
	"github.com/andrianbdn/oddk/internal/store/instances"
	"github.com/andrianbdn/oddk/internal/store/kvstore"
	"github.com/andrianbdn/oddk/internal/store/notifications"
	"github.com/andrianbdn/oddk/internal/store/offsite"
	"github.com/andrianbdn/oddk/internal/store/parameters"
	"github.com/andrianbdn/oddk/internal/store/snapshot"
)

type AuthToken struct {
	ID          int64  `db:"id"`
	TokenPrefix string `db:"token_prefix"`
	TokenHash   string `db:"token_hash"`
	CreatedAt   string `db:"created_at"`
}

type Store struct {
	Sqlx          *sqlx.DB
	Auth          *auth.AuthStore
	Instances     *instances.InstanceStore
	Backup        *backup.BackupStore
	Cron          *cron.CronStore
	Notifications *notifications.NotificationStore
	Parameters    *parameters.ParameterStore
	Health        *health.HealthStore
	KV            *kvstore.KVStore
	Offsite       *offsite.OffsiteStore
	Snapshot      *snapshot.SnapshotStore
}

func NewStore(dbPath, dataDir string) (*Store, error) {
	// Enable WAL mode for better concurrent read performance
	dsn := fmt.Sprintf("%s?_journal_mode=WAL", dbPath)
	sqx, err := sqlx.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	// Configure connection pool for SQLite - single writer, multiple readers
	sqx.SetMaxOpenConns(1)    // SQLite only supports one writer at a time
	sqx.SetMaxIdleConns(1)    // Keep one connection alive
	sqx.SetConnMaxLifetime(0) // Connections never expire

	// Wait out a concurrent writer instead of failing (or MustExec-panicking)
	// immediately with SQLITE_BUSY: `oddk auth` commands write tokens from a
	// separate process (see OpenAuthOnly), and the e2e harness restarts the
	// daemon in-process while the old store connection may still be flushing
	// a health-check write.
	if _, err := sqx.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}

	store := Store{
		Sqlx:          sqx,
		Auth:          auth.NewAuthStore(sqx),
		Instances:     instances.NewInstanceStore(sqx),
		Backup:        backup.NewBackupStore(sqx, dataDir),
		Cron:          cron.NewCronStore(sqx),
		Notifications: notifications.NewNotificationStore(sqx),
		Parameters:    parameters.NewParameterStore(sqx),
		Health:        health.NewHealthStore(sqx),
		KV:            kvstore.NewKVStore(sqx),
		Offsite:       offsite.NewOffsiteStore(sqx),
		Snapshot:      snapshot.NewSnapshotStore(sqx),
	}
	err = store.migrateDB()
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	err = store.Parameters.EnsureDefaultParameterGroup()
	if err != nil {
		return nil, fmt.Errorf("ensure default parameter group: %w", err)
	}

	err = store.KV.Initialize()
	if err != nil {
		return nil, fmt.Errorf("initialize KV store: %w", err)
	}

	return &store, nil
}

// OpenAuthOnly opens an EXISTING oddk database for the narrow purpose of
// managing CLI auth tokens from a separate process (the `oddk auth` commands).
// Unlike NewStore it runs no migrations and no startup writes - those belong to
// the daemon and would race (and MustExec-panic) against a running daemon. A
// busy_timeout lets the single token INSERT wait out the daemon's writer lock
// rather than failing immediately. The caller must Close the returned *sqlx.DB.
func OpenAuthOnly(dbPath string) (*auth.AuthStore, *sqlx.DB, error) {
	dsn := fmt.Sprintf("%s?_journal_mode=WAL", dbPath)
	db, err := sqlx.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("set busy_timeout: %w", err)
	}
	return auth.NewAuthStore(db), db, nil
}

func (s *Store) migrateDB() error {
	s.Sqlx.MustExec(`CREATE TABLE IF NOT EXISTS app_migrations (name TEXT, migrated_at TEXT)`)

	s.Sqlx.MustExec(`CREATE TABLE IF NOT EXISTS app_migrations_lock (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		app_info TEXT NOT NULL,
		locked_at TEXT NOT NULL
	)`)

	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("os.Hostname: %w", err)
	}
	appInfo := fmt.Sprintf("%s:%d:%s", hostname, os.Getpid(), os.Args[0])

	_, err = s.Sqlx.Exec(`INSERT INTO app_migrations_lock (id, app_info, locked_at) VALUES (1, ?, ?)`,
		appInfo, time.Now().Format(time.RFC3339))
	if err != nil {
		var lockInfo struct {
			AppInfo  string `db:"app_info"`
			LockedAt string `db:"locked_at"`
		}
		if s.Sqlx.Get(&lockInfo, "SELECT app_info, locked_at FROM app_migrations_lock WHERE id = 1") == nil {
			return fmt.Errorf("migrations locked by %s at %s", lockInfo.AppInfo, lockInfo.LockedAt)
		}
		return fmt.Errorf("failed to acquire migration lock: %w", err)
	}

	defer func() {
		// Attempt to clean up migration lock, ignore errors
		_, _ = s.Sqlx.Exec("DELETE FROM app_migrations_lock WHERE id = 1")
	}()

	err = s.runAllMigrations()
	if err != nil {
		return fmt.Errorf("runAllMigrations: %w", err)
	}

	return nil
}

func (s *Store) runSingleMigration(name string, fn func(sqx *sqlx.DB) error) error {
	var migratedAt string
	err := s.Sqlx.Get(&migratedAt, "SELECT migrated_at FROM app_migrations WHERE name = ?", name)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("checking migration status: %w", err)
	}
	if migratedAt != "" {
		return nil
	}

	err = fn(s.Sqlx)
	if err != nil {
		return fmt.Errorf("migration %s failed: %w", name, err)
	}

	s.Sqlx.MustExec("INSERT INTO app_migrations (name, migrated_at) VALUES (?, ?)", name, time.Now().Format(time.RFC3339))
	return nil
}

// VacuumInto writes a consistent copy of the store to path using SQLite's
// VACUUM INTO. It is safe against a live database — SQLite serialises it with
// other work — so a snapshot can be taken without stopping the daemon. The
// destination must not already exist.
//
// The copy is also compacted, which matters because a snapshot embeds it.
func (s *Store) VacuumInto(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("destination already exists: %s", path)
	}
	if _, err := s.Sqlx.Exec("VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("vacuum into %s: %w", path, err)
	}
	return nil
}

// AppliedMigrations returns the names of migrations applied to this store, in
// application order. A snapshot records these so a restore can reason about
// schema compatibility more precisely than a version string allows.
func (s *Store) AppliedMigrations() ([]string, error) {
	var names []string
	// app_migrations is (name, migrated_at) with no id column; rowid is
	// insertion order, which is application order.
	if err := s.Sqlx.Select(&names, `SELECT name FROM app_migrations ORDER BY rowid`); err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	return names, nil
}
