#!/bin/bash
#
# filmstock — the one cron entry. Everything the pipeline does, unattended:
#
#   40 3 * * *  /tank/mediadb/filmstock/scripts/run.sh
#
# It runs the day's incremental builds, then rebuilds from a fresh Wikimedia
# dump if one has appeared. Most days the second half finds nothing to do and
# says so in a line.
#
# Why one entry and not two cron lines: the two halves are not independent. The
# monthly rebuild has to replay every delta the published tip already holds
# before it may supersede it (docs/CHAIN.md §2), so it wants the dailies for
# today to have landed first. Sequencing them here makes that ordering a
# property of the schedule instead of a race between two timers.
#
# Locking. This script takes its own non-blocking lock so two invocations never
# overlap — a monthly rebuild runs for hours, and tomorrow's cron must not start
# a second one on the same dump. The halves keep their own locking underneath:
# daily.sh takes $FILMSTOCK_LOCK per day and releases it when it exits, and
# monthly.sh takes the same lock only for its short convergence-and-publish
# phase, leaving its hours of downloading and importing lock-free. Nothing is
# nested, so nothing can deadlock.
#
# A skipped run costs nothing. `catchup` knows which days the intermediate is
# behind, so a day missed because the previous run was still going is applied by
# the next one; monthly.sh resumes from whichever phase markers it finds.
#
# The daily half failing does not stop the monthly half from being attempted,
# and vice versa: they fail for unrelated reasons — a late adds-changes dump
# against a full dump's jobs finishing — and one being broken is a poor reason
# to skip the other. Both outcomes are reported, and the exit status is nonzero
# if either failed, so cron mails once with the whole picture.

set -Euo pipefail

HOME_DIR=${FILMSTOCK_HOME:-/tank/mediadb}
REPO=${FILMSTOCK_REPO:-$HOME_DIR/filmstock}
LOGDIR=${FILMSTOCK_LOGDIR:-$HOME_DIR/logs}
RUN_LOCK=${FILMSTOCK_RUN_LOCK:-$HOME_DIR/.run.lock}

mkdir -p "$LOGDIR"
STARTED=$(date -u +%s)

exec 9>"$RUN_LOCK"
if ! flock -n 9; then
  echo "filmstock: a previous run still holds $RUN_LOCK (a monthly rebuild takes hours); exiting"
  exit 0
fi

daily_rc=0
monthly_rc=0

echo "=== daily $(date -Is) ==="
"$REPO/scripts/daily.sh" || daily_rc=$?

echo
echo "=== monthly $(date -Is) ==="
"$REPO/scripts/monthly.sh" || monthly_rc=$?

mins=$(( ( $(date -u +%s) - STARTED ) / 60 ))
echo
if [ "$daily_rc" -eq 0 ] && [ "$monthly_rc" -eq 0 ]; then
  echo "filmstock: run complete in ${mins}m"
  exit 0
fi
# Say which half failed. The transcripts under $LOGDIR carry the diagnosis; the
# point of this line is that a cron mail names the failure without being read
# to the end.
[ "$daily_rc" -eq 0 ]   || echo "filmstock: DAILY FAILED (exit $daily_rc)" >&2
[ "$monthly_rc" -eq 0 ] || echo "filmstock: MONTHLY FAILED (exit $monthly_rc)" >&2
echo "filmstock: run finished in ${mins}m with failures; logs in $LOGDIR" >&2
exit 1
