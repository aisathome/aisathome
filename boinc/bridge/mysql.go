// Reading the project's MariaDB/MySQL database: the workunit, result and app tables (db/schema.sql
// in BOINC's source, https://github.com/BOINC/boinc/blob/master/db/schema.sql). SELECT only.
//
// SPDX-License-Identifier: Apache-2.0
package bridge

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

// MySQL is the Store on the project's database.
type MySQL struct {
	db    *sql.DB
	batch int
	apps  []string // the bridge's BOINC apps, one per tier

	mu     sync.Mutex
	appIDs map[int64]string // app id → name, once all are found
}

// OpenMySQL opens the project's database: with the DSN in cfg.DBDSNFile, or else with the
// settings in the project's config.xml.
func OpenMySQL(cfg Config) (*MySQL, error) {
	var mc *mysql.Config
	if cfg.DBDSNFile != "" {
		dsn, err := ReadSecret(cfg.DBDSNFile)
		if err != nil {
			return nil, err
		}
		if mc, err = mysql.ParseDSN(dsn); err != nil {
			return nil, fmt.Errorf("%s: not a DSN", cfg.DBDSNFile) // the error would quote it
		}
	} else {
		d, err := projectDB(cfg.ProjectDir)
		if err != nil {
			return nil, err
		}
		mc = mysql.NewConfig()
		mc.User, mc.Passwd, mc.Net, mc.Addr, mc.DBName = d.User, d.Passwd, "tcp", d.Host, d.Name
	}
	mc.Timeout, mc.ReadTimeout, mc.WriteTimeout = 10*time.Second, 30*time.Second, 30*time.Second
	db, err := sql.Open("mysql", mc.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetConnMaxLifetime(10 * time.Minute)
	return &MySQL{db: db, batch: cfg.Batch, apps: cfg.Apps()}, nil
}

// NewMySQL wraps an open database (tests).
func NewMySQL(db *sql.DB, batch int, apps ...string) *MySQL {
	return &MySQL{db: db, batch: batch, apps: apps}
}

func (m *MySQL) Close() error { return m.db.Close() }

// Ping checks the connection and that every app exists.
func (m *MySQL) Ping(ctx context.Context) error {
	if err := m.db.PingContext(ctx); err != nil {
		return err
	}
	_, err := m.appIDFor(ctx)
	return err
}

// appIDFor returns the bridge's apps by id; an app missing from the project is an error.
func (m *MySQL) appIDFor(ctx context.Context) (map[int64]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appIDs != nil {
		return m.appIDs, nil
	}
	if len(m.apps) == 0 {
		return nil, fmt.Errorf("no BOINC app configured")
	}
	ids := map[int64]string{}
	for _, name := range m.apps {
		var id int64
		err := m.db.QueryRowContext(ctx, "SELECT id FROM app WHERE name = ?", name).Scan(&id)
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("no BOINC app %q", name)
		}
		if err != nil {
			return nil, err
		}
		ids[id] = name
	}
	m.appIDs = ids
	return ids, nil
}

func (m *MySQL) Jobs(ctx context.Context, names []string) (map[string]Job, error) {
	apps, err := m.appIDFor(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Job, len(names))
	for len(names) > 0 {
		n := min(len(names), 200)
		chunk := names[:n]
		names = names[n:]
		args := make([]any, 0, n+1)
		args = append(args, m.batch)
		for _, s := range chunk {
			args = append(args, s)
		}
		// The work unit's one result, if the transitioner made it; ordered so that a later result
		// (there should be none) wins.
		rows, err := m.db.QueryContext(ctx, `SELECT w.name, w.appid, w.error_mask, r.id, r.server_state, r.outcome,
				r.validate_state, r.userid, r.hostid, r.teamid, r.opaque
			FROM workunit w LEFT JOIN result r ON r.workunitid = w.id
			WHERE w.batch = ? AND w.name IN (?`+strings.Repeat(",?", n-1)+`)
			ORDER BY w.id, r.id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			var appID int64
			var j Job
			var rid, state, outcome, vstate, user, host, team sql.NullInt64
			var opaque sql.NullFloat64
			if err := rows.Scan(&name, &appID, &j.ErrorMask, &rid, &state, &outcome, &vstate, &user, &host, &team, &opaque); err != nil {
				rows.Close()
				return nil, err
			}
			if rid.Valid {
				j.HasResult = true
				j.ServerState, j.Outcome, j.ValidateState = int(state.Int64), int(outcome.Int64), int(vstate.Int64)
				j.User, j.Host, j.Team = user.Int64, host.Int64, team.Int64
				j.Claimed = opaque.Float64 != 0
			}
			app, ok := apps[appID]
			if !ok {
				continue // another app's work unit with this name: not the bridge's
			}
			j.App = app
			out[name] = j
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (m *MySQL) AwaitingCredit(ctx context.Context, afterID int64, limit int) ([]Awaiting, error) {
	apps, err := m.appIDFor(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]any, 0, len(apps))
	for id := range apps {
		ids = append(ids, id)
	}
	// result's (appid, server_state) index narrows it to the apps' finished results. Inconclusive
	// too, so that the bridge can report it: the pilot validator never leaves a result there (the
	// bridge runs it only once the file is written), but a pass of another validator would, and
	// BOINC does not ask again for an inconclusive result.
	args := append(ids, ServerStateOver, OutcomeSuccess, ValidateStateInit, ValidateStateInconclusive, m.batch, afterID, limit)
	rows, err := m.db.QueryContext(ctx, `SELECT r.id, w.id, w.name, r.appid, r.validate_state, r.opaque FROM result r JOIN workunit w ON w.id = r.workunitid
		WHERE r.appid IN (?`+strings.Repeat(",?", len(ids)-1)+`) AND r.server_state = ? AND r.outcome = ? AND r.validate_state IN (?, ?) AND w.batch = ?
			AND r.id > ?
		ORDER BY r.id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Awaiting
	for rows.Next() {
		var a Awaiting
		var appID int64
		var opaque float64
		if err := rows.Scan(&a.ResultID, &a.WorkunitID, &a.Name, &appID, &a.ValidateState, &opaque); err != nil {
			return nil, err
		}
		a.App = apps[appID]
		a.Claimed = opaque != 0
		out = append(out, a)
	}
	return out, rows.Err()
}
