// SPDX-License-Identifier: Apache-2.0
//
// The pilot app's validator: BOINC's validator framework (sched/validator.cpp in BOINC's source)
// with this file's check_set and check_pair, the framework's lower-level interface. A pilot's
// result carries no output worth comparing; its credit is what the gateway metered, which the
// bridge writes to <credit_dir>/<work unit name> once it is final (boinc/README.md, "The
// bridge"). check_set reads that file into the result's claimed credit and makes the result
// canonical and valid; with --post_assigned_credit the framework grants exactly that, capped by
// --max_granted_credit.
//
// It runs on one work unit at a time, started by the bridge right after it wrote the file, never
// as a daemon:
//
//   pilot_validator --app pilot --one_pass --one_pass_N_WU 1 --min_wu_id N --max_wu_id N
//       --post_assigned_credit --max_granted_credit 150000 --credit_dir /home/boincadm/project/credit
//
// Why not a daemon that waits: the gateway holds credit for a day after the pilot ends, and the
// framework cannot wait on a work unit with one result. If a pass finds no canonical result (the
// usual transient error of init_result), it marks the result inconclusive, raises
// target_nresults and transitions the work unit at once, which with max_total_results 1 errors
// it; the transitioner never asks again for an inconclusive result. If check_set returns an
// error instead, the work unit is left as it was, but a daemon's scan reads it again at once and
// spins. In the one-work-unit mode above an error leaves the work unit as it was and the program
// ends, so a run without the file (by hand, say) does no harm.
//
// At most once: the framework writes the credit (user, team and host totals), the result and the
// work unit in separate statements, so a run killed in between, run again, would grant again.
// check_set therefore claims the result first, in one conditional statement (opaque from 0 to 1;
// the framework later writes the result back with opaque 1). A run that finds it claimed grants
// nothing and logs it: the bridge sees a claimed result that still waits and leaves it to a
// person, who checks whether the interrupted run reached the totals.
//
// A work unit whose name is not a pilot's (a test work unit made by hand) is valid with no
// credit. A file that is not one non-negative number is an error too, logged: it is the bridge's
// to fix, and the result waits rather than losing its credit.

#include <cerrno>
#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

#include "boinc_db.h"
#include "error_numbers.h"
#include "sched_msgs.h"
#include "sched_util.h"
#include "validate_util2.h"
#include "validator.h"

static std::string credit_dir;

// claim_in_db claims a result for this run: 0 if it was not claimed before (now it is),
// ERR_DB_NOT_UNIQUE if an earlier run claimed it, or a database error. The database connection
// reports matched rows (BOINC connects with CLIENT_FOUND_ROWS).
static int claim_in_db(DB_ID_TYPE id) {
    DB_RESULT r;
    r.id = id;
    int retval = r.update_field("opaque=1", "opaque=0");
    if (retval) return retval;
    return r.affected_rows() == 1 ? 0 : ERR_DB_NOT_UNIQUE;
}

// claim_result is claim_in_db; the fixture test replaces it.
int (*claim_result)(DB_ID_TYPE) = claim_in_db;

int validate_handler_init(int argc, char** argv) {
    for (int i = 1; i < argc; i++) {
        if (is_arg(argv[i], "credit_dir")) {
            if (++i >= argc) {
                log_messages.printf(MSG_CRITICAL, "--credit_dir needs a directory\n");
                return 1;
            }
            credit_dir = argv[i];
        }
    }
    if (credit_dir.empty()) {
        log_messages.printf(MSG_CRITICAL, "--credit_dir is required\n");
        return 1;
    }
    return 0;
}

void validate_handler_usage() {
    fprintf(stderr,
        "    Custom options:\n"
        "    --credit_dir DIR    where the bridge writes each pilot's final credit (required)\n"
    );
}

// pilot_name: the work unit name of a result "<work unit>_<n>", if it is a pilot's
// ("p-" and 12 lower-case hex digits).
bool pilot_name(const char* result_name, std::string& wu) {
    const char* us = strrchr(result_name, '_');
    if (!us || us == result_name) return false;
    wu.assign(result_name, us - result_name);
    if (wu.size() != 14 || wu.compare(0, 2, "p-") != 0) return false;
    for (size_t i = 2; i < wu.size(); i++) {
        char c = wu[i];
        if (!((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'))) return false;
    }
    return true;
}

// read_credit reads a credit file: one finite, non-negative number, optionally followed by
// white space. Returns 0, ERR_FOPEN (no file) or ERR_XML_PARSE (anything else).
int read_credit(const std::string& path, double& credit) {
    FILE* f = fopen(path.c_str(), "r");
    if (!f) return ERR_FOPEN;
    char buf[64];
    size_t n = fread(buf, 1, sizeof(buf) - 1, f);
    bool more = !feof(f);
    fclose(f);
    if (more || n == 0) return ERR_XML_PARSE;
    buf[n] = 0;
    char* end;
    errno = 0;
    double v = strtod(buf, &end);
    if (end == buf || errno) return ERR_XML_PARSE;
    while (*end == ' ' || *end == '\n' || *end == '\r' || *end == '\t') end++;
    if (*end || !std::isfinite(v) || v < 0) return ERR_XML_PARSE;
    credit = v;
    return 0;
}

// pilot_credit: the credit for a result (0, credit set), or why to wait (ERR_NOT_FOUND: no file
// yet; ERR_XML_PARSE: a file that is not one non-negative number).
int pilot_credit(const RESULT& result, double& credit) {
    std::string wu;
    if (!pilot_name(result.name, wu)) {
        credit = 0;
        log_messages.printf(MSG_NORMAL, "[RESULT#%lu %s] not a pilot's: no credit\n", result.id, result.name);
        return 0;
    }
    int retval = read_credit(credit_dir + "/" + wu, credit);
    if (retval == ERR_FOPEN) {
        log_messages.printf(MSG_DEBUG, "[RESULT#%lu %s] no credit file yet\n", result.id, result.name);
        return ERR_NOT_FOUND;
    }
    if (retval) {
        log_messages.printf(MSG_CRITICAL, "[RESULT#%lu %s] credit file is not one non-negative number\n",
            result.id, result.name);
        return ERR_XML_PARSE;
    }
    log_messages.printf(MSG_NORMAL, "[RESULT#%lu %s] credit %f from the gateway\n", result.id, result.name, credit);
    return 0;
}

// check_set gets the work unit's viable results (over, success, not invalid): one for a pilot.
// The first is canonical and valid with the bridge's credit; any other (there should be none) is
// invalid, so a work unit never earns twice. Without the file: an error, changing nothing.
int check_set(std::vector<RESULT>& results, WORKUNIT&, DB_ID_TYPE& canonicalid, double&, bool& retry) {
    retry = false;
    canonicalid = 0;
    if (results.empty()) return 0;
    double credit;
    int retval = pilot_credit(results[0], credit);
    if (retval) return retval;
    retval = claim_result(results[0].id);
    if (retval == ERR_DB_NOT_UNIQUE) {
        log_messages.printf(MSG_CRITICAL,
            "[RESULT#%lu %s] claimed by an earlier run that did not finish: not granting again "
            "(check whether that run reached the user, team and host totals)\n",
            results[0].id, results[0].name);
        return retval;
    }
    if (retval) {
        log_messages.printf(MSG_CRITICAL, "[RESULT#%lu %s] cannot claim it: %s\n", results[0].id,
            results[0].name, boincerror(retval));
        return retval;
    }
    results[0].opaque = 1; // the framework writes the result back with it
    results[0].claimed_credit = credit;
    results[0].validate_state = VALIDATE_STATE_VALID;
    canonicalid = results[0].id;
    for (size_t i = 1; i < results.size(); i++) {
        results[i].validate_state = VALIDATE_STATE_INVALID;
    }
    return 0;
}

// check_pair: a result that arrives after the work unit has its canonical result. A pilot's work
// unit has one result, so this would be a second run of the same pilot: no credit.
void check_pair(RESULT& r, RESULT&, bool& retry) {
    retry = false;
    r.validate_state = VALIDATE_STATE_INVALID;
}

// init_result and cleanup_result are called by the framework only with --check_punitive (to
// punish hosts whose results fail); a pilot's failure is not the host's fault.
int init_result(const RESULT&, void*& data) {
    data = NULL;
    return 0;
}

int cleanup_result(RESULT const&, void*) {
    return 0;
}
