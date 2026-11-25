// SPDX-License-Identifier: Apache-2.0
//
// check_set and check_pair of the pilot validator against credit files: a pilot's file makes its
// result canonical and valid with that claimed credit; no file yet, or a file that is not one
// non-negative number, is an error that changes nothing (the framework then leaves the work unit
// for its next scan); a work unit that is not a pilot's is valid with no credit; a second result
// is never valid.

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

#include "boinc_db.h"
#include "validate_util2.h"

int validate_handler_init(int argc, char** argv);

static int failures = 0;
static std::string dir;

static void put(const char* name, const char* content) {
    FILE* f = fopen((dir + "/" + name).c_str(), "w");
    fputs(content, f);
    fclose(f);
}

static RESULT result(DB_ID_TYPE id, const char* name) {
    RESULT r;
    r.clear();
    r.id = id;
    strncpy(r.name, name, sizeof(r.name) - 1);
    r.validate_state = VALIDATE_STATE_INIT;
    r.claimed_credit = -1;
    return r;
}

static void fail(const char* what, const char* name) {
    printf("FAIL %s: %s\n", name, what);
    failures++;
}

// One result: wait (error, nothing changed) or canonical and valid with the credit.
static void check(const char* name, bool wait, double credit) {
    std::vector<RESULT> rs{result(7, name)};
    WORKUNIT wu;
    wu.clear();
    DB_ID_TYPE canonical = 99;
    double dummy = 0;
    bool retry = true;
    int ret = check_set(rs, wu, canonical, dummy, retry);
    if (retry) fail("asked for a retry (the framework would mark it inconclusive)", name);
    if (wait) {
        if (ret == 0) fail("no error while waiting", name);
        if (canonical != 0 || rs[0].validate_state != VALIDATE_STATE_INIT || rs[0].claimed_credit != -1)
            fail("changed the result while waiting", name);
    } else {
        if (ret != 0) fail("an error", name);
        if (canonical != 7 || rs[0].validate_state != VALIDATE_STATE_VALID || rs[0].claimed_credit != credit) {
            printf("    canonical %lu, state %d, credit %f (want %f)\n", (unsigned long)canonical,
                rs[0].validate_state, rs[0].claimed_credit, credit);
            fail("not canonical and valid with the credit", name);
        }
    }
    if (failures == 0) printf("ok   %s\n", name);
}

int main() {
    char tmpl[] = "/tmp/pilot-validator-XXXXXX";
    if (!mkdtemp(tmpl)) return 2;
    dir = tmpl;

    const char* noarg[] = {"pilot_validator", NULL};
    if (validate_handler_init(1, (char**)noarg) == 0) fail("started without --credit_dir", "init");
    const char* args[] = {"pilot_validator", "--credit_dir", dir.c_str(), NULL};
    if (validate_handler_init(3, (char**)args) != 0) {
        printf("FAIL: --credit_dir refused\n");
        return 1;
    }

    put("p-0123456789ab", "1234.5\n");
    put("p-00000000000a", "0\n");
    put("p-00000000000b", "-3\n");
    put("p-00000000000c", "lots\n");
    put("p-00000000000d", "12 13\n");
    put("p-00000000000e", "nan\n");
    put("p-00000000000f", "");

    check("p-0123456789ab_0", false, 1234.5);
    check("p-00000000000a_0", false, 0);        // withheld or idle: valid, 0
    check("p-000000000001_0", true, 0);         // no file yet
    check("p-00000000000b_0", true, 0);         // negative
    check("p-00000000000c_0", true, 0);         // not a number
    check("p-00000000000d_0", true, 0);         // two numbers
    check("p-00000000000e_0", true, 0);         // not finite
    check("p-00000000000f_0", true, 0);         // empty
    check("p-test-20261118152746_0", false, 0); // a test work unit made by hand
    check("p-0123456789AB_0", false, 0);        // not a pilot's name

    // Two results (there should never be): the first is canonical, the second invalid.
    std::vector<RESULT> two{result(1, "p-0123456789ab_0"), result(2, "p-0123456789ab_1")};
    WORKUNIT wu;
    wu.clear();
    DB_ID_TYPE canonical = 0;
    double dummy;
    bool retry;
    if (check_set(two, wu, canonical, dummy, retry) != 0 || canonical != 1 ||
        two[0].validate_state != VALIDATE_STATE_VALID || two[1].validate_state != VALIDATE_STATE_INVALID)
        fail("the second result is not invalid", "two results");
    // A straggler after the canonical result: never valid.
    RESULT late = result(3, "p-0123456789ab_2");
    check_pair(late, two[0], retry);
    if (late.validate_state != VALIDATE_STATE_INVALID || retry) fail("a straggler was not invalid", "check_pair");

    char cmd[256];
    snprintf(cmd, sizeof(cmd), "rm -rf %s", dir.c_str());
    if (system(cmd)) {}
    if (failures) {
        printf("%d failure(s)\n", failures);
        return 1;
    }
    printf("all passed\n");
    return 0;
}
