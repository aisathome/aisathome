// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func tables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type IN ('table', 'index') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		out = append(out, n)
	}
	return out
}

var wantTables = []string{"campaign_items", "campaigns", "pilot_credit_days", "pilots", "pilots_by_account", "pilots_by_state",
	"results", "results_by_campaign", "results_by_time"}

// A fresh database gets every migration; reopening applies none and keeps the data; a database
// from before the store (user_version 0) is migrated; one from a newer gateway, or a damaged file,
// stops the start.
func TestScienceDBMigrations(t *testing.T) {
	dir := t.TempDir()
	s, err := openScienceDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.schemaVersion(); err != nil || v != len(scienceMigrations) {
		t.Fatalf("fresh: version %d %v, want %d", v, err, len(scienceMigrations))
	}
	if got := tables(t, s.db); !slices.Equal(got, wantTables) {
		t.Fatalf("tables %v", got)
	}
	var mode string
	if s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); mode != "wal" {
		t.Errorf("journal_mode %q", mode)
	}
	var fk int
	if s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); fk != 1 {
		t.Error("foreign keys off")
	}
	if st, _ := os.Stat(filepath.Join(dir, ScienceDBFile)); st == nil || st.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", st)
	}
	c := &Campaign{Project: "p", ID: "t1", Status: CampaignRunning}
	if _, _, err := s.putCampaign(c, nil, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	s.close()

	s, err = openScienceDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := s.campaign("p", "t1"); err != nil || c == nil || c.Status != CampaignRunning {
		t.Fatalf("reopened: %v %v", c, err)
	}
	// a newer gateway's database
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.close()
	if _, err := openScienceDB(dir); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer schema: %v", err)
	}

	// an empty SQLite file (user_version 0) is migrated
	dir2 := t.TempDir()
	raw, err := sql.Open("sqlite", filepath.Join(dir2, ScienceDBFile))
	if err != nil {
		t.Fatal(err)
	}
	raw.Exec("CREATE TABLE unrelated (x)")
	raw.Close()
	if s, err := openScienceDB(dir2); err != nil {
		t.Fatal(err)
	} else if v, _ := s.schemaVersion(); v != len(scienceMigrations) {
		t.Fatalf("migrated to %d", v)
	} else {
		s.close()
	}

	// not a database
	dir3 := t.TempDir()
	os.WriteFile(filepath.Join(dir3, ScienceDBFile), []byte(strings.Repeat("not sqlite ", 1000)), 0o600)
	if _, err := openScienceDB(dir3); err == nil {
		t.Fatal("a damaged file opened")
	}

	// in memory: works, has no file and no copies
	m, err := openScienceDB("")
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	if got := tables(t, m.db); !slices.Equal(got, wantTables) {
		t.Fatalf("memory tables %v", got)
	}
	if wrote, err := m.backup(time.Now()); wrote || err != nil {
		t.Fatalf("memory backup %v %v", wrote, err)
	}
}

// A failed migration leaves the database as it was: its version and no half-made tables.
func TestScienceDBMigrationAtomic(t *testing.T) {
	defer func(m []string) { scienceMigrations = m }(scienceMigrations)
	dir := t.TempDir()
	s, err := openScienceDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.close()
	n := len(scienceMigrations)
	scienceMigrations = append(slices.Clone(scienceMigrations), `CREATE TABLE half (x); CREATE TABLE broken (`)
	if _, err := openScienceDB(dir); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("migration %d", n+1)) {
		t.Fatalf("broken migration: %v", err)
	}
	scienceMigrations = scienceMigrations[:n]
	s, err = openScienceDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if v, _ := s.schemaVersion(); v != n {
		t.Fatalf("version %d after a failed migration", v)
	}
	if got := tables(t, s.db); slices.Contains(got, "half") {
		t.Fatalf("half a migration stayed: %v", got)
	}
}

// Once a UTC day a VACUUM INTO copy, which opens on its own and holds the data; the newest
// scienceBackupsKept are kept; a half-written copy from a crash does not stop the next.
func TestScienceDBBackup(t *testing.T) {
	dir := t.TempDir()
	s, err := openScienceDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if _, _, err := s.putCampaign(&Campaign{Project: "p", ID: "t1", Status: CampaignRunning, Title: "One"}, nil, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 17, 0, 30, 0, 0, time.UTC)
	if wrote, err := s.backup(day); !wrote || err != nil {
		t.Fatalf("first copy %v %v", wrote, err)
	}
	if wrote, err := s.backup(day.Add(20 * time.Hour)); wrote || err != nil {
		t.Fatalf("second copy the same day %v %v", wrote, err)
	}
	copyPath := filepath.Join(dir, "science-2026-11-17.db")
	if st, err := os.Stat(copyPath); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("copy %v %v", st, err)
	}
	// the copy is a database of its own with the data
	c, err := sql.Open("sqlite", copyPath+"?_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	var title string
	if err := c.QueryRow(`SELECT title FROM campaigns WHERE id = 't1'`).Scan(&title); err != nil || title != "One" {
		t.Fatalf("copy holds %q %v", title, err)
	}
	var v int
	if c.QueryRow("PRAGMA user_version").Scan(&v); v != len(scienceMigrations) {
		t.Fatalf("copy version %d", v)
	}
	c.Close()

	os.WriteFile(filepath.Join(dir, "science-2026-11-18.db.tmp"), []byte("half"), 0o600) // a crash mid-copy
	for i := 1; i <= 3; i++ {
		if wrote, err := s.backup(day.AddDate(0, 0, i)); !wrote || err != nil {
			t.Fatalf("day %d: %v %v", i, wrote, err)
		}
	}
	all, _ := s.backups()
	if !slices.Equal(all, []string{"science-2026-11-19.db", "science-2026-11-20.db"}) {
		t.Fatalf("kept %v", all)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("left %s", e.Name())
		}
	}
	// other files in the state dir are never touched
	os.WriteFile(filepath.Join(dir, "science-notes.db"), []byte("x"), 0o600)
	s.backup(day.AddDate(0, 0, 4))
	if _, err := os.Stat(filepath.Join(dir, "science-notes.db")); err != nil {
		t.Error("removed a file that is not a copy")
	}
}

// Campaigns and results survive a gateway restart.
func TestScienceRestart(t *testing.T) {
	dir := t.TempDir()
	r := sciRig(t, dir)
	r.want(201, "PUT", campPath+"/t8", testProjectKey, trialBody())
	r.want(201, "POST", resPath, testProjectKey, resultBody("accepted"))
	if err := r.g.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := sciRig(t, dir)
	c := r2.want(200, "GET", campPath+"/t8", "", nil)["campaign"].(map[string]any)
	if c["title"] != "Trial 8" || len(c["items"].([]any)) != 2 || c["results"].(map[string]any)["accepted"] != 1.0 {
		t.Fatalf("after restart %v", c)
	}
	if out := r2.want(200, "POST", resPath, testProjectKey, resultBody("accepted")); out["created"] != false {
		t.Fatalf("idempotency lost on restart: %v", out)
	}
}
