// What the bridge reads from and does to the BOINC project: its database (read only), its tools
// (create_work, cancel_jobs, dir_hier_path) and its web RPCs (create_account.php,
// am_get_info.php). The interfaces let the tests stand in for BOINC.
//
// SPDX-License-Identifier: Apache-2.0
package bridge

import (
	"context"
	"fmt"
)

// BOINC's result and work-unit states, from db/boinc_db_types.h in BOINC's source
// (https://github.com/BOINC/boinc/blob/master/db/boinc_db_types.h).
const (
	ServerStateUnsent     = 2 // RESULT_SERVER_STATE_UNSENT
	ServerStateInProgress = 4 // RESULT_SERVER_STATE_IN_PROGRESS
	ServerStateOver       = 5 // RESULT_SERVER_STATE_OVER

	OutcomeSuccess     = 1 // RESULT_OUTCOME_SUCCESS
	OutcomeClientError = 3 // RESULT_OUTCOME_CLIENT_ERROR
	OutcomeNoReply     = 4 // RESULT_OUTCOME_NO_REPLY: the host did not report by the deadline
	OutcomeDidntNeed   = 5 // RESULT_OUTCOME_DIDNT_NEED: cancelled before it was sent

	ValidateStateInit  = 0 // VALIDATE_STATE_INIT: not validated yet
	ValidateStateValid = 1 // VALIDATE_STATE_VALID
	// VALIDATE_STATE_INCONCLUSIVE: a validator pass found no canonical result. The pilot validator
	// never leaves a result here (the bridge runs it only once the credit file is written; see
	// boinc/validator), but a result some other pass left here still gets its file.
	ValidateStateInconclusive = 4

	WUErrorCancelled = 16 // WU_ERROR_CANCELLED
)

// Job is what BOINC holds for one pilot's work unit. A pilot's work unit has exactly one result
// (max_total_results 1 in its input template), so a job is the work unit and that result.
type Job struct {
	ErrorMask int
	// HasResult: the transitioner has made the result (it does so shortly after create_work).
	HasResult     bool
	ServerState   int
	Outcome       int
	ValidateState int
	// User, Host and Team are BOINC's ids, set when the result is sent (0 before; Team 0: none).
	User, Host, Team int64
}

// awaitingValidation: the result succeeded and the validator has not decided on it yet.
func (j Job) awaitingValidation() bool {
	return j.HasResult && j.ServerState == ServerStateOver && j.Outcome == OutcomeSuccess &&
		(j.ValidateState == ValidateStateInit || j.ValidateState == ValidateStateInconclusive)
}

// sent: a host has the work unit (or had it).
func (j Job) sent() bool { return j.HasResult && j.Host != 0 }

// unsent: no host has taken the work unit yet, and it can still be cancelled as unsent.
func (j Job) unsent() bool {
	return j.ErrorMask == 0 && (!j.HasResult || j.ServerState == ServerStateUnsent)
}

// reported says whether the work unit is done for the gateway, and with which outcome
// ("success", "error", "cancelled" or "expired", the gateway's names).
func (j Job) reported() (string, bool) {
	if j.HasResult && j.ServerState == ServerStateOver {
		switch j.Outcome {
		case OutcomeSuccess:
			return "success", true
		case OutcomeNoReply:
			return "expired", true
		case OutcomeDidntNeed:
			return "cancelled", true
		default: // couldn't send, client error, validate error, client detached
			return "error", true
		}
	}
	if j.HasResult && j.ServerState == ServerStateInProgress {
		// A host has it, cancelled or not: BOINC tells the client to abort at its next scheduler
		// request, and the result's end is reported then. Until then the pilot may serve.
		return "", false
	}
	if j.ErrorMask&WUErrorCancelled != 0 {
		return "cancelled", true
	}
	if j.ErrorMask != 0 {
		return "error", true
	}
	return "", false
}

// Store reads the project's database. It never writes: work units are created and cancelled
// through BOINC's tools, and credit reaches BOINC through the validator.
type Store interface {
	// Jobs returns the bridge's work units among names (pilot ids), by name. A name with no work
	// unit of the bridge's batch is missing from the map.
	Jobs(ctx context.Context, names []string) (map[string]Job, error)
	// AwaitingCredit returns up to limit of the bridge's work units whose result succeeded and
	// waits for the validator (not validated yet, or inconclusive), in result id order, after the
	// result id afterID: a caller pages through them all.
	AwaitingCredit(ctx context.Context, afterID int64, limit int) ([]Awaiting, error)
}

// Awaiting is a result that waits for its credit: its id (for paging), its work unit's id and
// name.
type Awaiting struct {
	ResultID   int64
	WorkunitID int64
	Name       string
}

// Tools makes and cancels work units with the project's own programs.
type Tools interface {
	// Stage puts an input file into the project's download directory.
	Stage(ctx context.Context, name string, content []byte) error
	// Unstage removes a staged input file that no work unit uses.
	Unstage(ctx context.Context, name string) error
	// CreateWork makes a pilot's work unit, named after the pilot, with the staged credential file.
	CreateWork(ctx context.Context, name, credentialFile string) error
	// Cancel cancels a work unit (its unsent result is not sent).
	Cancel(ctx context.Context, name string) error
	// Validate runs the app's validator on one work unit, whose credit file is written.
	Validate(ctx context.Context, workunitID int64) error
}

// Account is what am_get_info.php says about a BOINC user.
type Account struct {
	ID       int64
	WeakAuth string // the attach-only key, "<id>_<32 hex>"
}

// RPC is the project's stock web RPCs.
type RPC interface {
	// CreateAccount makes a user (create_account.php) and returns its account key. With an address
	// that already has a user and the same password hash, BOINC returns that user's key.
	CreateAccount(ctx context.Context, email, passwdHash, userName string) (string, error)
	// AccountInfo looks a user up by its account key (am_get_info.php).
	AccountInfo(ctx context.Context, accountKey string) (Account, error)
}

// RPCError is an error BOINC answered (<error_num>), as opposed to a failed call, which is
// worth retrying.
type RPCError struct {
	Num int
	Msg string
}

func (e *RPCError) Error() string { return fmt.Sprintf("BOINC error %d: %s", e.Num, e.Msg) }

// BOINC's error numbers the bridge tells apart, from lib/error_numbers.h in BOINC's source.
// create_account.php answers -1 with a message for a closed project or a wrong invitation code.
const (
	errDBNotFound     = -136 // ERR_DB_NOT_FOUND
	errDBCantConnect  = -138 // ERR_DB_CANT_CONNECT
	errProjectDown    = -183 // ERR_PROJECT_DOWN
	errBadUserName    = -188 // ERR_BAD_USER_NAME
	errDBNotUnique    = -137 // ERR_DB_NOT_UNIQUE
	errBadEmailAddr   = -205 // ERR_BAD_EMAIL_ADDR
	errNonuniqueEmail = -207 // ERR_NONUNIQUE_EMAIL
	errRequireConsent = -242 // ERR_ACCT_REQUIRE_CONSENT
)
