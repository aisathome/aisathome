// SPDX-License-Identifier: Apache-2.0
package bridge

import (
	"database/sql"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// The Store's SQL against a real MariaDB or MySQL: AISATHOME_BRIDGE_TEST_MYSQL is the DSN of an
// empty database the test may create tables in (CI starts one). The tables have the columns of
// BOINC's db/schema.sql that the bridge reads, with BOINC's names and types.
const testSchema = `
CREATE TABLE app (id integer NOT NULL AUTO_INCREMENT, name varchar(254) NOT NULL, PRIMARY KEY (id), UNIQUE KEY (name));
CREATE TABLE workunit (id integer NOT NULL AUTO_INCREMENT, appid integer NOT NULL, name varchar(254) NOT NULL,
  error_mask integer NOT NULL DEFAULT 0, batch integer NOT NULL DEFAULT 0, PRIMARY KEY (id), UNIQUE KEY (name));
CREATE TABLE result (id integer NOT NULL AUTO_INCREMENT, workunitid integer NOT NULL, server_state integer NOT NULL,
  outcome integer NOT NULL, validate_state integer NOT NULL, hostid integer NOT NULL, userid integer NOT NULL,
  teamid integer NOT NULL, appid integer NOT NULL, name varchar(254) NOT NULL, PRIMARY KEY (id),
  KEY res_app_state (appid, server_state), KEY res_wuid (workunitid));
`

func testDB(t *testing.T) *sql.DB {
	dsn := os.Getenv("AISATHOME_BRIDGE_TEST_MYSQL")
	if dsn == "" {
		t.Skip("AISATHOME_BRIDGE_TEST_MYSQL not set")
	}
	mc, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	mc.MultiStatements = true
	db, err := sql.Open("mysql", mc.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for i := 0; ; i++ { // a database that has just started
		if err = db.Ping(); err == nil || i == 60 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP TABLE IF EXISTS result, workunit, app;" + testSchema); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMySQLStore(t *testing.T) {
	db := testDB(t)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := db.Exec(q, a...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO app (id, name) VALUES (1, 'other'), (2, 'pilot')")
	wu := func(id int, name string, batch, mask int) {
		exec("INSERT INTO workunit (id, appid, name, error_mask, batch) VALUES (?, 2, ?, ?, ?)", id, name, mask, batch)
	}
	res := func(wuid, state, outcome, vstate, host, user, team int) {
		exec(`INSERT INTO result (workunitid, server_state, outcome, validate_state, hostid, userid, teamid, appid, name)
			VALUES (?, ?, ?, ?, ?, ?, ?, 2, CONCAT('r', ?))`, wuid, state, outcome, vstate, host, user, team, wuid)
	}
	wu(1, "p-000000000001", 1, 0) // no result yet
	wu(2, "p-000000000002", 1, 0)
	res(2, ServerStateUnsent, 0, 0, 0, 0, 0)
	wu(3, "p-000000000003", 1, 0)
	res(3, ServerStateInProgress, 0, 0, 9, 7, 3)
	wu(4, "p-000000000004", 1, 0)
	res(4, ServerStateOver, OutcomeSuccess, ValidateStateInit, 9, 7, 0)
	wu(5, "p-000000000005", 1, WUErrorCancelled)
	res(5, ServerStateOver, OutcomeDidntNeed, 0, 0, 0, 0)
	wu(6, "p-000000000006", 2, 0) // another bridge's batch
	res(6, ServerStateOver, OutcomeSuccess, ValidateStateInit, 1, 1, 0)
	wu(7, "p-test-20261118", 0, 0) // a test work unit made by hand
	res(7, ServerStateOver, OutcomeSuccess, ValidateStateInit, 1, 1, 0)
	wu(8, "p-000000000008", 1, 0)
	res(8, ServerStateOver, OutcomeSuccess, ValidateStateValid, 9, 7, 0)
	wu(9, "p-000000000009", 1, 0) // the validator looked before the credit file existed
	res(9, ServerStateOver, OutcomeSuccess, ValidateStateInconclusive, 9, 7, 0)

	s := NewMySQL(db, 1, "pilot")
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	names := []string{"p-000000000001", "p-000000000002", "p-000000000003", "p-000000000004", "p-000000000005",
		"p-000000000006", "p-test-20261118", "p-000000000099"}
	// Many names at once (more than one chunk of 200).
	for i := 100; i < 450; i++ {
		names = append(names, fmt.Sprintf("p-%012d", i))
	}
	jobs, err := s.Jobs(ctx, names)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Job{
		"p-000000000001": {},
		"p-000000000002": {HasResult: true, ServerState: ServerStateUnsent},
		"p-000000000003": {HasResult: true, ServerState: ServerStateInProgress, Host: 9, User: 7, Team: 3},
		"p-000000000004": {HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeSuccess, Host: 9, User: 7},
		"p-000000000005": {ErrorMask: WUErrorCancelled, HasResult: true, ServerState: ServerStateOver, Outcome: OutcomeDidntNeed},
	}
	if len(jobs) != len(want) {
		t.Errorf("jobs: %v", jobs)
	}
	for n, j := range want {
		if jobs[n] != j {
			t.Errorf("%s: %+v, want %+v", n, jobs[n], j)
		}
	}
	got, err := s.AwaitingCredit(ctx, 0, 10)
	if err != nil || len(got) != 2 || got[0].Name != "p-000000000004" || got[1].Name != "p-000000000009" ||
		got[0].ResultID >= got[1].ResultID || got[0].WorkunitID != 4 || got[1].WorkunitID != 9 {
		t.Fatalf("awaiting credit: %v %v", got, err)
	}
	// Paging: after the first one's result id, only the second; one per page.
	page, err := s.AwaitingCredit(ctx, got[0].ResultID, 1)
	if err != nil || len(page) != 1 || page[0] != got[1] {
		t.Errorf("second page: %v %v", page, err)
	}
	if first, _ := s.AwaitingCredit(ctx, 0, 1); !slices.Equal(first, got[:1]) {
		t.Errorf("first page of one: %v", first)
	}
	if err := NewMySQL(db, 1, "nope").Ping(ctx); err == nil {
		t.Error("no error for a missing app")
	}
}
