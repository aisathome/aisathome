#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Run a command in one of the host's build slots (see host.py): flock on <slot dir>/slot.<i>, the same
# lock files the host side uses, and start only while MemAvailable is at least BUILD_MIN_FREE_GB GB.
# POSIX sh with flock(1), so it runs in a minimal image.
#   sh buildslot.sh <slot dir> <command> [args...]
# BUILDSLOT_LOG=<file> appends "<asked epoch> <got epoch> slot<i>" per acquisition.
# First come, first served (host.py): the waiter takes <slot dir>/turnstile (blocking) before it polls, and lets it go
# once it holds a slot; a slot directory without one (an older host) is polled as before.
d=$1; shift
t0=$(date +%s)
n=$(ls "$d" 2>/dev/null | grep -c '^slot\.[0-9]*$')
[ "$n" -ge 1 ] || { echo "buildslot: no slot files in $d" >&2; exit 125; }
if [ -f "$d/turnstile" ]; then
  exec 8<"$d/turnstile"
  flock 8
fi
while :; do
  avail=$(awk '/MemAvailable/{print int($2/1048576)}' /proc/meminfo)
  if [ "$avail" -ge "${BUILD_MIN_FREE_GB:-10}" ]; then
    i=1
    while [ "$i" -le "$n" ]; do
      exec 9<"$d/slot.$i"      # read only: the slot directory is mounted read-only
      if flock -n 9; then
        exec 8<&-
        [ -n "${BUILDSLOT_LOG:-}" ] && echo "$t0 $(date +%s) slot$i" >> "$BUILDSLOT_LOG"
        "$@"
        rc=$?
        exec 9>&-
        exit $rc
      fi
      exec 9>&-
      i=$((i + 1))
    done
  fi
  sleep 3
done
