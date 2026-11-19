// The gateway's science records store (decision 0021, point 7): an embedded SQLite database,
// state_dir/science.db, for the records that are not tasks: campaigns and external results
// (campaigns.go), connector pilots and their credit (pilots.go), connector requests
// (connector_requests.go), and later others of the same kind.
// Tasks stay one JSON file each (tasks.go).
//
// The driver is pure Go (modernc.org/sqlite), so the gateway still builds without cgo for any
// platform. One connection, so writes never meet SQLITE_BUSY inside the gateway; the database is in
// WAL mode with synchronous=FULL (a few writes a minute at most: every change is on disk when its
// call answers); its temporary data stays in memory (a container's root may be read-only). Without a
// state_dir the database is in memory, like the rest of the state.
//
// Schema: versioned in code (scienceMigrations), applied in order at open, each in a transaction
// with PRAGMA user_version. A database from a newer gateway (a higher user_version) is refused, so
// a rollback never writes into a schema it does not know: restore the backup copy or start the
// newer gateway again.
//
// Backup: the data volume's daily snapshots hold the database and its WAL, which SQLite recovers
// like a crash. Besides that, once a UTC day the gateway writes a clean copy beside it with VACUUM
// INTO (science-YYYY-MM-DD.db) and keeps the newest scienceBackupsKept copies, so every snapshot
// also holds a consistent file that opens on its own. To restore: stop the gateway, replace
// science.db with a copy (and delete science.db-wal and science.db-shm), start it.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the "sqlite" database/sql driver, pure Go
)

const (
	// ScienceDBFile is the database's name in state_dir.
	ScienceDBFile = "science.db"
	// scienceBackupsKept: the daily VACUUM INTO copies kept beside the database.
	scienceBackupsKept = 2
	// scienceBackupCheck: how often the gateway checks whether today's copy exists.
	scienceBackupCheck  = time.Hour
	scienceBackupPrefix = "science-"
	scienceBackupSuffix = ".db"
)

// scienceMigrations are the schema's versions: migration i takes the database from user_version i
// to i+1. Append only; never change one that has shipped.
var scienceMigrations = []string{
	// 1: campaigns, their items, external results (campaigns.go). Times are RFC 3339 UTC text with
	// seconds ("2026-11-17T20:15:00Z"), which sorts as it reads.
	`CREATE TABLE campaigns (
		project    TEXT NOT NULL,
		id         TEXT NOT NULL,
		title      TEXT NOT NULL DEFAULT '',
		status     TEXT NOT NULL CHECK (status IN ('preparing', 'running', 'finished')),
		started_at TEXT,
		ended_at   TEXT,
		labels     TEXT NOT NULL DEFAULT '{}',
		text       TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (project, id)
	) WITHOUT ROWID;
	CREATE TABLE campaign_items (
		project  TEXT NOT NULL,
		campaign TEXT NOT NULL,
		pos      INTEGER NOT NULL,
		id       TEXT NOT NULL,
		title    TEXT NOT NULL DEFAULT '',
		url      TEXT NOT NULL DEFAULT '',
		role     TEXT NOT NULL DEFAULT '',
		labels   TEXT NOT NULL DEFAULT '{}',
		PRIMARY KEY (project, campaign, id),
		FOREIGN KEY (project, campaign) REFERENCES campaigns (project, id) ON DELETE CASCADE
	) WITHOUT ROWID;
	CREATE TABLE results (
		project      TEXT NOT NULL,
		id           TEXT NOT NULL,
		source       TEXT NOT NULL,
		external_id  TEXT NOT NULL,
		campaign     TEXT,
		item         TEXT NOT NULL DEFAULT '',
		task         TEXT NOT NULL DEFAULT '',
		url          TEXT NOT NULL DEFAULT '',
		state        TEXT NOT NULL CHECK (state IN ('pending', 'accepted', 'rejected', 'error')),
		verdict      TEXT NOT NULL DEFAULT '',
		labels       TEXT NOT NULL DEFAULT '{}',
		submitted_at TEXT NOT NULL,
		verdict_at   TEXT,
		created_at   TEXT NOT NULL,
		updated_at   TEXT NOT NULL,
		PRIMARY KEY (project, id),
		UNIQUE (project, source, external_id),
		FOREIGN KEY (project, campaign) REFERENCES campaigns (project, id)
	) WITHOUT ROWID;
	CREATE INDEX results_by_time ON results (project, submitted_at, id);
	CREATE INDEX results_by_campaign ON results (project, campaign);`,
	// 2: connector pilots and their credit (pilots.go). Times are fixed-width RFC 3339 UTC text with
	// milliseconds ("2026-11-18T15:31:54.000Z"), which sorts as it reads.
	`CREATE TABLE pilots (
		id                 TEXT NOT NULL PRIMARY KEY,
		connector          TEXT NOT NULL,
		cred_sha256        TEXT NOT NULL UNIQUE,
		state              TEXT NOT NULL CHECK (state IN ('created', 'bound', 'connected', 'disconnected', 'ended')),
		deadline           TEXT NOT NULL,
		ext_user           TEXT NOT NULL DEFAULT '',
		ext_host           TEXT NOT NULL DEFAULT '',
		ext_team           TEXT NOT NULL DEFAULT '',
		account            TEXT NOT NULL DEFAULT '',
		created_at         TEXT NOT NULL,
		bound_at           TEXT,
		first_connected_at TEXT,
		connected_at       TEXT,
		disconnected_at    TEXT,
		seen_at            TEXT,
		ended_at           TEXT,
		close_reason       TEXT NOT NULL DEFAULT '',
		end_reason         TEXT NOT NULL DEFAULT '',
		connections        INTEGER NOT NULL DEFAULT 0,
		served_ms          INTEGER NOT NULL DEFAULT 0,
		requests           INTEGER NOT NULL DEFAULT 0,
		failed             INTEGER NOT NULL DEFAULT 0,
		self_served        INTEGER NOT NULL DEFAULT 0,
		prompt_tokens      INTEGER NOT NULL DEFAULT 0,
		cached_tokens      INTEGER NOT NULL DEFAULT 0,
		completion_tokens  INTEGER NOT NULL DEFAULT 0,
		metered            REAL NOT NULL DEFAULT 0,
		credit             REAL NOT NULL DEFAULT 0,
		withheld           TEXT NOT NULL DEFAULT '',
		agent              TEXT NOT NULL DEFAULT ''
	) WITHOUT ROWID;
	CREATE INDEX pilots_by_state ON pilots (connector, state, created_at);
	CREATE INDEX pilots_by_account ON pilots (account);
	CREATE TABLE pilot_credit_days (
		account TEXT NOT NULL,
		day     TEXT NOT NULL,
		credit  REAL NOT NULL DEFAULT 0,
		PRIMARY KEY (account, day)
	) WITHOUT ROWID;`,
	// 3: connector requests (connector_requests.go): what a signed-in person asked a connector to do
	// for their account. The address, password hash and key are kept only while a step needs them,
	// then blanked; finished requests are deleted after connectorRequestKeep. Times as in 2.
	`CREATE TABLE connector_requests (
		id           TEXT NOT NULL PRIMARY KEY,
		connector    TEXT NOT NULL,
		kind         TEXT NOT NULL CHECK (kind IN ('create_link', 'claim', 'set_email')),
		account      TEXT NOT NULL,
		state        TEXT NOT NULL CHECK (state IN ('pending', 'verified', 'done', 'failed')),
		ext_user     TEXT NOT NULL DEFAULT '',
		user_name    TEXT NOT NULL DEFAULT '',
		email        TEXT NOT NULL DEFAULT '',
		passwd_hash  TEXT NOT NULL DEFAULT '',
		attach_key   TEXT NOT NULL DEFAULT '',
		own_email    INTEGER NOT NULL DEFAULT 0,
		from_account TEXT NOT NULL DEFAULT '',
		error        TEXT NOT NULL DEFAULT '',
		created_at   TEXT NOT NULL,
		updated_at   TEXT NOT NULL
	) WITHOUT ROWID;
	CREATE INDEX connector_requests_by_state ON connector_requests (connector, state, created_at);
	CREATE INDEX connector_requests_by_account ON connector_requests (account, created_at);`,
}

// scienceDB is the open database.
type scienceDB struct {
	db   *sql.DB
	path string       // "" in memory
	log  *slog.Logger // for errors the client sees only as errScience; nil: slog's default
}

// openScienceDB opens (or creates) dir/science.db and brings its schema up to date; dir "" opens
// one in memory. Nothing is left open when it fails.
func openScienceDB(dir string) (_ *scienceDB, err error) {
	// A plain name: the driver takes the query off before SQLite sees the file name.
	path, dsn := "", ":memory:?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=temp_store(MEMORY)&_txlock=immediate"
	if dir != "" {
		if strings.ContainsAny(dir, "?#") {
			return nil, fmt.Errorf("science database: state_dir %q: no '?' or '#'", dir)
		}
		path = filepath.Join(dir, ScienceDBFile)
		dsn = path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=temp_store(MEMORY)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("science database: %w", err)
	}
	defer func() {
		if err != nil {
			db.Close()
		}
	}()
	// One connection: writes are serialised here rather than retried, and an in-memory database
	// lives as long as its connection.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("science database %s: %w", path, err)
	}
	if path != "" {
		os.Chmod(path, 0o600) // the directory is the operator's (0700); the file needs no more
		var check string
		if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
			return nil, fmt.Errorf("science database %s: integrity check failed (%q, %v); restore a backup copy (%s*%s)",
				path, check, err, scienceBackupPrefix, scienceBackupSuffix)
		}
	}
	if err := migrateScience(ctx, db); err != nil {
		return nil, fmt.Errorf("science database %s: %w", path, err)
	}
	return &scienceDB{db: db, path: path}, nil
}

// migrateScience applies the migrations the database has not had yet.
func migrateScience(ctx context.Context, db *sql.DB) error {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > len(scienceMigrations) {
		return fmt.Errorf("schema version %d is newer than this gateway's (%d): run the newer gateway or restore a copy", v, len(scienceMigrations))
	}
	for i := v; i < len(scienceMigrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, scienceMigrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}

// schemaVersion is the database's user_version.
func (s *scienceDB) schemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow("PRAGMA user_version").Scan(&v)
	return v, err
}

func (s *scienceDB) close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// backupName is the copy of the UTC day of now.
func backupName(now time.Time) string {
	return scienceBackupPrefix + now.UTC().Format(time.DateOnly) + scienceBackupSuffix
}

// backups lists the copies in the database's directory, oldest first.
func (s *scienceDB) backups() ([]string, error) {
	ents, err := os.ReadDir(filepath.Dir(s.path))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(n, scienceBackupPrefix) || !strings.HasSuffix(n, scienceBackupSuffix) {
			continue
		}
		if _, err := time.Parse(time.DateOnly, strings.TrimSuffix(strings.TrimPrefix(n, scienceBackupPrefix), scienceBackupSuffix)); err == nil {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// backup writes today's copy (UTC) with VACUUM INTO unless it exists, then keeps the newest
// scienceBackupsKept copies. It reports whether it wrote one. An in-memory database has none.
func (s *scienceDB) backup(now time.Time) (bool, error) {
	if s == nil || s.path == "" {
		return false, nil
	}
	dir := filepath.Dir(s.path)
	name := backupName(now)
	final := filepath.Join(dir, name)
	wrote := false
	if _, err := os.Stat(final); errors.Is(err, os.ErrNotExist) {
		tmp := final + ".tmp"
		os.Remove(tmp) // a copy a crash left half written; VACUUM INTO refuses an existing file
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		_, err := s.db.ExecContext(ctx, "VACUUM INTO ?", tmp)
		cancel()
		if err != nil {
			os.Remove(tmp)
			return false, fmt.Errorf("science database copy: %w", err)
		}
		if err := syncFile(tmp); err != nil {
			os.Remove(tmp)
			return false, fmt.Errorf("science database copy: %w", err)
		}
		if err := os.Rename(tmp, final); err != nil {
			os.Remove(tmp)
			return false, fmt.Errorf("science database copy: %w", err)
		}
		syncDir(dir)
		wrote = true
	} else if err != nil {
		return false, fmt.Errorf("science database copy: %w", err)
	}
	all, err := s.backups()
	if err != nil {
		return wrote, fmt.Errorf("science database copies: %w", err)
	}
	for len(all) > scienceBackupsKept {
		if err := os.Remove(filepath.Join(dir, all[0])); err != nil {
			return wrote, fmt.Errorf("science database copies: %w", err)
		}
		all = all[1:]
	}
	return wrote, nil
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	os.Chmod(path, 0o600)
	err = f.Sync()
	return errors.Join(err, f.Close())
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// backupScience is the gateway's hourly check for today's copy.
func (g *Gateway) backupScience() {
	wrote, err := g.sci.backup(time.Now())
	switch {
	case err != nil:
		g.log.Error("science database copy failed", "err", err)
	case wrote:
		g.log.Info("science database copied", "file", backupName(time.Now()))
	}
}
