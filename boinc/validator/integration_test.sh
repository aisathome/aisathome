#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The pilot validator inside BOINC's own validator framework, against a real MariaDB or MySQL with
# BOINC's schema (db/schema.sql and db/constraints.sql of the same tree), run as the bridge runs it:
# on one work unit, after its credit file is there.
#
# - a pilot whose credit file is there: canonical, valid, granted exactly that (capped by
#   --max_granted_credit), credited to its host and user; no other work unit touched;
# - a run before the file is there: the program ends and the work unit is left as it was
#   (need_validate kept, not inconclusive, no extra result asked for, no error); a run once the
#   file is there grants it;
# - a result an earlier run claimed but did not finish (killed between the framework's writes):
#   nothing granted again, nothing changed.
#
#   BOINC=<built tree> MYSQL_HOST=127.0.0.1 MYSQL_PORT=3306 MYSQL_USER=root MYSQL_PASSWORD=... \
#     boinc/validator/integration_test.sh
#
# It drops and creates the database pilot_validator_test.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
: "${BOINC:?BOINC: a built BOINC source tree}" "${MYSQL_HOST:?}" "${MYSQL_USER:?}" "${MYSQL_PASSWORD:?}"
MYSQL_PORT="${MYSQL_PORT:-3306}"
db=pilot_validator_test
my() { MYSQL_PWD="$MYSQL_PASSWORD" mysql -h"$MYSQL_HOST" -P"$MYSQL_PORT" -u"$MYSQL_USER" "$@"; }
q() { my -N -B "$db" -e "$1" | tr '\t' ' '; }
fails=0
expect() { # expect <what> <want> <got>
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$3', want '$2'"; fails=$((fails + 1)); fi
}

for i in $(seq 60); do my -e 'SELECT 1' >/dev/null 2>&1 && break; sleep 1; done
my -e "DROP DATABASE IF EXISTS $db; CREATE DATABASE $db"
my "$db" < "$BOINC/db/schema.sql"
my "$db" < "$BOINC/db/constraints.sql"
# Only the columns that matter; sql_mode '' fills the rest with their implicit defaults.
my "$db" <<'SQL'
SET SESSION sql_mode = '';
INSERT INTO app (id, name, user_friendly_name, create_time) VALUES (1, 'pilot', 'pilot', 1);
INSERT INTO user (id, name, email_addr, authenticator, cross_project_id, passwd_hash, create_time)
  VALUES (1, 'u', 'u@nomail.example', 'auth1', 'cpid1', 'x', 1);
INSERT INTO host (id, userid, create_time) VALUES (1, 1, 1);
INSERT INTO workunit (id, name, appid, need_validate, min_quorum, target_nresults, max_error_results,
    max_total_results, max_success_results, transition_time, create_time, batch)
  VALUES (1, 'p-0123456789ab', 1, 1, 1, 1, 1, 1, 1, 2000000000, 1, 1),
         (2, 'p-00000000000a', 1, 1, 1, 1, 1, 1, 1, 2000000000, 1, 1),
         (3, 'p-00000000000b', 1, 1, 1, 1, 1, 1, 1, 2000000000, 1, 1),
         (4, 'p-00000000000c', 1, 1, 1, 1, 1, 1, 1, 2000000000, 1, 1);
INSERT INTO result (id, workunitid, name, server_state, outcome, validate_state, hostid, userid, appid,
    create_time, sent_time, received_time)
  VALUES (1, 1, 'p-0123456789ab_0', 5, 1, 0, 1, 1, 1, 1, 1, 1),
         (2, 2, 'p-00000000000a_0', 5, 1, 0, 1, 1, 1, 1, 1, 1),
         (3, 3, 'p-00000000000b_0', 5, 1, 0, 1, 1, 1, 1, 1, 1),
         (4, 4, 'p-00000000000c_0', 5, 1, 0, 1, 1, 1, 1, 1, 1);
-- Result 4: claimed by a run that was killed after the claim (and maybe after the totals).
UPDATE result SET opaque = 1 WHERE id = 4;
SQL

proj="$(mktemp -d)"
trap 'rm -rf "$proj"' EXIT
mkdir -p "$proj/credit" "$proj/upload" "$proj/download" "$proj/cgi-bin" # cgi-bin: BOINC's mark of a project directory
cat > "$proj/config.xml" <<XML
<boinc><config>
  <db_name>$db</db_name>
  <db_host>$MYSQL_HOST:$MYSQL_PORT</db_host>
  <db_user>$MYSQL_USER</db_user>
  <db_passwd>$MYSQL_PASSWORD</db_passwd>
  <upload_dir>$proj/upload</upload_dir>
  <download_dir>$proj/download</download_dir>
  <uldl_dir_fanout>1024</uldl_dir_fanout>
</config></boinc>
XML
echo 123.25 > "$proj/credit/p-0123456789ab"
echo 5000 > "$proj/credit/p-00000000000b" # above the cap
echo 77 > "$proj/credit/p-00000000000c"   # its run was interrupted
run() { # run <work unit id>, as the bridge does; a hang (a spinning scan) fails
  timeout 60 env BOINC_PROJECT_DIR="$proj" "$here/pilot_validator" --app pilot --one_pass --one_pass_N_WU 1 \
    --min_wu_id "$1" --max_wu_id "$1" --post_assigned_credit --max_granted_credit 1000 \
    --credit_dir "$proj/credit" -d 3 > "$proj/validator.log" 2>&1 \
    || { st=$?; cat "$proj/validator.log"; echo "FAIL the validator on work unit $1 exited $st"; exit 1; }
}

run 2 # no file yet
expect "a run before the file: the result untouched" "0 0" "$(q 'SELECT validate_state, granted_credit FROM result WHERE id = 2')"
expect "its work unit: still to validate, no extra result, no error" "1 1 0 0" \
  "$(q 'SELECT need_validate, target_nresults, error_mask, canonical_resultid FROM workunit WHERE id = 2')"
run 1
expect "no other work unit touched" "0 1" "$(q 'SELECT validate_state, need_validate FROM result r JOIN workunit w ON w.id = r.workunitid WHERE r.id = 3')"
run 3
expect "pilot with its file: valid, granted its credit, claimed" "1 123.25 1 1" \
  "$(q 'SELECT validate_state, granted_credit, (SELECT canonical_resultid FROM workunit WHERE id = 1), opaque FROM result WHERE id = 1')"
expect "its work unit: done, ready to assimilate" "0 1" "$(q 'SELECT need_validate, assimilate_state FROM workunit WHERE id = 1')"
expect "credit above the cap: granted the cap" "1 1000" "$(q 'SELECT validate_state, granted_credit FROM result WHERE id = 3')"
expect "the waiting work unit still untouched" "0 1 1 0" \
  "$(q 'SELECT r.validate_state, w.need_validate, w.target_nresults, w.error_mask FROM result r JOIN workunit w ON w.id = r.workunitid WHERE r.id = 2')"
expect "host and user credited" "1123.25 1123.25" "$(q 'SELECT h.total_credit, u.total_credit FROM host h, user u WHERE h.id = 1 AND u.id = 1')"
run 4
expect "a result an interrupted run claimed: not granted again" "0 0 1" "$(q 'SELECT validate_state, granted_credit, opaque FROM result WHERE id = 4')"
expect "its work unit untouched" "1 0 0" "$(q 'SELECT need_validate, error_mask, canonical_resultid FROM workunit WHERE id = 4')"
expect "totals unchanged by it" "1123.25 1123.25" "$(q 'SELECT h.total_credit, u.total_credit FROM host h, user u WHERE h.id = 1 AND u.id = 1')"
grep -q 'claimed by an earlier run' "$proj/validator.log" && echo "ok   and the log says so" || { echo "FAIL no log line for the claimed result"; fails=$((fails + 1)); }

echo 0 > "$proj/credit/p-00000000000a" # final at last: withheld
run 2
expect "a run once the file is there: valid, granted 0" "1 0" "$(q 'SELECT validate_state, granted_credit FROM result WHERE id = 2')"
expect "and its work unit done" "0 2" \
  "$(q 'SELECT need_validate, canonical_resultid FROM workunit WHERE id = 2')"

if (( fails )); then
  echo "--- validator log (last pass)"; cat "$proj/validator.log"
  echo "$fails failure(s)"; exit 1
fi
echo "all passed"
