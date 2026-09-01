#!/bin/bash
#
# filmstock daily — fetch the day's adds-changes dump, apply it, publish the
# build. One entry point, meant to run unattended from cron:
#
#   17 9 * * *  /tank/mediadb/filmstock/scripts/daily.sh
#
# It is a loop over days rather than one day because a missed run — a reboot, a
# dump published late, a machine that was off for a week — has to heal itself
# without anyone noticing it happened. `filmstock catchup` already knows which
# days the intermediate is behind; this script adds what a release needs around
# each one (the post-passes, then publish) and does them a day at a time, so a
# chain four days behind comes back as four ordinary daily builds rather than
# one lump that no code path has ever been tested on.
#
# Every failure stops the run. Nothing is skipped past: days are applied on top
# of one another, so applying them out of order leaves a hole that nothing
# downstream would ever detect.
#
# Stopping is safe because each failure is re-runnable rather than repaired by
# hand:
#
#   - import stamps incr_through only once the day's stream is fully applied,
#     and page writes are replaces, so a crash mid-import re-applies cleanly.
#   - a torn export dies with its staging directory. The file is disposable by
#     design (journal_mode=OFF) and nothing here post-processes a nonzero exit.
#   - a day that imported but never published is not lost. Export re-derives
#     every record from the whole intermediate, so the next day's build carries
#     the missing day's changes; the chain simply skips a build id, which is
#     legal — patches chain parent to parent, not date to date.
#
# The one thing it will not do is publish over an existing build directory: a
# published build is immutable, and a half-written one is the single case that
# wants a human. The error says so explicitly.
#
# Console output is a short summary; the full transcript, with per-step wall
# time and peak RSS, goes to $LOGDIR. On failure the tail of that transcript is
# printed to stderr, so the cron mail is the diagnosis rather than a pointer to
# it.

set -Eeuo pipefail

HOME_DIR=${FILMSTOCK_HOME:-/tank/mediadb}
REPO=${FILMSTOCK_REPO:-$HOME_DIR/filmstock}
BIN=${FILMSTOCK_BIN:-$REPO/filmstock}
SQLDIFF=${FILMSTOCK_SQLDIFF:-$REPO/sqldiff}
INTER=${FILMSTOCK_INTER:-$HOME_DIR/intermediate-v3.db}
CACHE=${FILMSTOCK_CACHE:-$HOME_DIR/resolver-ext.db}
FULL_DUMPS=${FILMSTOCK_FULL_DUMPS:-$HOME_DIR/dump/20260801}
INCR_DIR=${FILMSTOCK_INCR:-$HOME_DIR/dump/incr}
ROOT=${FILMSTOCK_ROOT:-$HOME_DIR/bucket}
STAGE=${FILMSTOCK_STAGE:-$HOME_DIR/stage}
LOGDIR=${FILMSTOCK_LOGDIR:-$HOME_DIR/logs}
LOCK=${FILMSTOCK_LOCK:-$HOME_DIR/.daily.lock}
WORKERS=${FILMSTOCK_WORKERS:-18}
MAX_DAYS=${FILMSTOCK_MAX_DAYS:-0}          # 0 = every day available
KEEP_INCR_DAYS=${FILMSTOCK_KEEP_INCR:-45}  # past Wikimedia's ~42-day retention
MIN_FREE_GB=${FILMSTOCK_MIN_FREE_GB:-25}   # a day stages ~1.3 GB; leave room

# One run at a time. The intermediate is mutated in place, and two runs
# applying different days to it concurrently would interleave into a store that
# matches no day at all.
exec 9>"$LOCK"
if ! flock -n 9; then
  echo "filmstock daily: another run holds $LOCK; exiting" >&2
  exit 0
fi

mkdir -p "$LOGDIR" "$STAGE" "$INCR_DIR"
LOG=$LOGDIR/daily-$(date -u +%Y%m%dT%H%M%SZ).log
STARTED=$(date -u +%s)

log()  { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*" >>"$LOG"; }
say()  { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; }
# die reports to the console as well as the log: a cron run that stops on a
# preflight check must say why in the mail, not only in a file nobody opened.
die()  {
  { printf 'filmstock daily: %s\n' "$*"; printf 'full log: %s\n' "$LOG"; } \
    | tee -a "$LOG" >&2
  exit 1
}

# The cron mail should be the diagnosis, not a pointer to it.
on_err() {
  local rc=$? line=${1:-?}
  {
    echo
    echo "filmstock daily FAILED (exit $rc, line $line) after $(( $(date -u +%s) - STARTED ))s"
    echo "full log: $LOG"
    echo "--- last 40 lines ---"
    tail -40 "$LOG" 2>/dev/null
  } >&2
  exit "$rc"
}
trap 'on_err $LINENO' ERR

# timed runs a step into the log with its wall time and peak RSS attached.
# Long jobs get measured rather than assumed: a day whose export suddenly takes
# twice as long is the first sign of something worth looking at.
timed() {
  local label=$1; shift
  log "--> $label: $*"
  /usr/bin/time -f "    [$label] wall %E  cpu %P  maxrss %M kB" -a -o "$LOG" \
    "$@" >>"$LOG" 2>&1
}
timing() { grep -F "[$1]" "$LOG" | tail -1 | sed 's/^ *//'; }

interday() { sqlite3 -readonly "$INTER" "select * from meta" | sed -n 's/^incr_through|//p'; }
catlatest() {
  python3 - "$ROOT/builds.json" <<'PY'
import json,sys
print(json.load(open(sys.argv[1]))["latest"])
PY
}

# --- preflight ------------------------------------------------------------
# Everything checked here is a thing that, missing, would otherwise fail
# halfway through a 6-minute export.
[ -x "$BIN" ]     || die "no filmstock binary at $BIN (make build)"
[ -x "$SQLDIFF" ] || die "no sqldiff at $SQLDIFF (make sqldiff)"
[ -w "$INTER" ]   || die "intermediate $INTER is missing or not writable"
[ -r "$CACHE" ]   || die "resolver cache $CACHE is missing"
[ -d "$FULL_DUMPS" ] || die "full dump set $FULL_DUMPS is missing"
[ -s "$ROOT/builds.json" ] || die "no catalog at $ROOT/builds.json"

free_gb=$(df -BG --output=avail "$STAGE" | tail -1 | tr -dc '0-9')
[ "$free_gb" -ge "$MIN_FREE_GB" ] || die "only ${free_gb}G free on $STAGE, want ${MIN_FREE_GB}G"

# The intermediate and the catalog must agree about where the chain is. They
# drift by exactly one day when a previous run imported and then failed to
# publish, which is recoverable; anything wider means someone has been moving
# files around and the next patch would be computed against the wrong base.
have=$(interday) || die "cannot read incr_through from $INTER"
tip=$(catlatest) || die "cannot read latest from $ROOT/builds.json"
[ -n "$have" ] || die "$INTER states no incr_through"
say "intermediate through $have; published chain tip $tip"
if [ "$have" != "$tip" ]; then
  say "note: intermediate is ahead of the chain (a previous run imported but did"
  say "      not publish); the next build will carry both days' changes"
fi

# --- what is there to do --------------------------------------------------
plan=$("$BIN" catchup -dry-run \
  -db "$STAGE/.plan.db" -inter "$INTER" -cache "$CACHE" \
  -dumps "$INCR_DIR" -full-dumps "$FULL_DUMPS" 2>&1 | tee -a "$LOG" \
  | sed -n 's/^ *would apply //p') || die "catchup -dry-run failed (see $LOG)"

if [ -z "$plan" ]; then
  say "already up to date at $have; nothing to do"
  exit 0
fi
days=$(echo "$plan" | wc -l | tr -d ' ')
if [ "$MAX_DAYS" -gt 0 ] && [ "$days" -gt "$MAX_DAYS" ]; then
  plan=$(echo "$plan" | head -n "$MAX_DAYS")
  days=$MAX_DAYS
fi
say "$days day(s) to apply: $(echo "$plan" | tr '\n' ' ')"

# --- one day at a time ----------------------------------------------------
n=0
for day in $plan; do
  n=$((n + 1))
  say "[$n/$days] $day: fetch + import + export"
  dir=$STAGE/$day
  rm -rf "$dir"
  mkdir -p "$dir"

  # catchup fetches the day (resuming a partial download, verifying the length
  # against the server's) and runs the one production daily job on it: the day
  # into the intermediate, then every record re-derived from the whole corpus.
  timed "$day update" "$BIN" catchup \
    -from "$day" -max 1 -keep \
    -db "$dir/filmstock.db" -text-db "$dir/filmstock-text.db" \
    -inter "$INTER" -cache "$CACHE" \
    -dumps "$INCR_DIR" -full-dumps "$FULL_DUMPS" -workers "$WORKERS"
  say "    $(timing "$day update")"

  # catchup exits 0 when there is nothing to apply, so "did the day land" is
  # asked of the intermediate rather than of the exit status.
  now=$(interday)
  [ "$now" = "$day" ] || die "$day: catchup left the intermediate at $now; refusing to publish"
  [ -s "$dir/filmstock.db" ] && [ -s "$dir/filmstock-text.db" ] \
    || die "$day: export produced no databases in $dir"

  # The post-passes. They read the resolver cache and rewrite index tables in
  # the freshly exported database; the vectors database is not rebuilt daily
  # and is carried forward from the base by publish.
  timed "$day post" bash -c \
    "'$BIN' index-external-ids -db '$dir/filmstock.db' -cache '$CACHE' && \
     '$BIN' index-series       -db '$dir/filmstock.db' -cache '$CACHE'"
  say "    $(timing "$day post")"

  if [ -e "$ROOT/$day" ]; then
    die "$ROOT/$day already exists. A published build is immutable, so this is
either a build that is already done (in which case the catalog is behind and
wants looking at) or a publish that died half-written. Neither is safe to
guess about: inspect $ROOT/$day and $ROOT/builds.json, remove the directory if
it is a half-written one, and re-run."
  fi

  # publish emits the patches, APPLIES each one to a copy of its base, and
  # refuses to record the build unless the result reproduces its content
  # hashes. The verification is the publisher's, not something bolted on here.
  timed "$day publish" "$BIN" publish \
    -root "$ROOT" -id "$day" -from "$dir" -sqldiff "$SQLDIFF"
  say "    $(timing "$day publish")"

  published=$(catlatest)
  [ "$published" = "$day" ] || die "$day: published, but the catalog's latest is $published"
  # What the publisher actually emitted, lifted out of this day's publish step.
  # Captured first, then written: the log is append-open, and reading it while
  # appending to it is how a summary ends up quoting itself.
  detail=$(awk -v k="--> $day publish" 'index($0,k){f=1} f' "$LOG" \
    | grep -E 'patch|carried|bridge' || true)
  [ -z "$detail" ] || sed 's/^/    /' <<<"$detail" | tee -a "$LOG"
  say "    published $day ($(du -sh --apparent-size "$ROOT/$day" | cut -f1))"

  rm -rf "$dir"
done

# --- housekeeping ---------------------------------------------------------
# The downloaded dailies are kept so a failed day can be re-run without asking
# Wikimedia for 900 MB again, but only until they are past the retention window
# that made them worth keeping.
pruned=$(find "$INCR_DIR" -maxdepth 1 -name 'enwiki-*-pages-meta-hist-incr.xml.bz2' \
  -mtime +"$KEEP_INCR_DAYS" -print -delete | wc -l | tr -d ' ')
[ "$pruned" = "0" ] || say "pruned $pruned incr dump(s) older than $KEEP_INCR_DAYS days"
find "$LOGDIR" -name 'daily-*.log' -mtime +90 -delete 2>/dev/null || true
rm -f "$STAGE/.plan.db"

say "done: $days day(s) through $(catlatest) in $(( ($(date -u +%s) - STARTED) / 60 )) min"
say "log: $LOG"
