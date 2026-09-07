#!/bin/bash
#
# filmstock daily — fetch the day's adds-changes dump, apply it, publish the
# build. Normally invoked by scripts/run.sh, which is the single cron entry;
# safe to run by hand.
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
# The full dump set this intermediate was built from, read from the store
# rather than pinned: the monthly promotes a new intermediate built from a new
# dump, and a hardcoded path here would quietly go on resolving against the
# superseded one. The intermediate records its own source; that is the fact.
FULL_DUMPS=${FILMSTOCK_FULL_DUMPS:-}
INCR_DIR=${FILMSTOCK_INCR:-$HOME_DIR/dump/incr}
ROOT=${FILMSTOCK_ROOT:-$HOME_DIR/bucket}
STAGE=${FILMSTOCK_STAGE:-$HOME_DIR/stage}
LOGDIR=${FILMSTOCK_LOGDIR:-$HOME_DIR/logs}
LOCK=${FILMSTOCK_LOCK:-$HOME_DIR/.daily.lock}
WORKERS=${FILMSTOCK_WORKERS:-18}
MAX_DAYS=${FILMSTOCK_MAX_DAYS:-0}          # 0 = every day available
# How long WE keep the adds-changes dumps. Wikimedia's ~42 days is their
# budget, not ours: once a day is downloaded it is ours to keep, and these
# files are the only thing that can carry a monthly rebuild forward onto the
# published tip.
#
# That matters because we do not control when a dump is finished. A dump is
# named for the day its content was snapshotted, not the day it is published,
# so an enwiki-20261001 dump that only finishes on 10/30 still has to replay
# every daily from 10/02 to reach the tip. If those days have aged off the
# server AND out of here, the rebuild simply cannot be published — the full
# would be older than the chain, which is the one thing publish refuses.
#
# Keeping 45 days made our margin the same as the server's, which is to say
# none. At ~850 MB/day, half a year is ~155 GB, and the disk holds 37 TB.
# Retention here is the cheapest insurance in the pipeline.
KEEP_INCR_DAYS=${FILMSTOCK_KEEP_INCR:-180}
MIN_FREE_GB=${FILMSTOCK_MIN_FREE_GB:-25}   # a day stages ~1.3 GB; leave room

# One run at a time. The intermediate is mutated in place, and two runs
# applying different days to it concurrently would interleave into a store that
# matches no day at all.
exec 9>"$LOCK"
if ! flock -n 9; then
  echo "filmstock daily: another run holds $LOCK; exiting" >&2
  exit 0
fi

# The binary must come from committed code. A build published from a dirty
# tree carries whatever was half-finished in it: on 2026-09-03 an uncommitted
# schema change (a new episodes column) reached a daily this way, and the
# resulting build was self-consistent but unverifiable by any consumer running
# released code, because its manifest's content hash was computed with a spec
# that only existed in the working tree. Set FILMSTOCK_ALLOW_DIRTY=1 to
# override deliberately.
if command -v git >/dev/null && git -C "$REPO" rev-parse --git-dir >/dev/null 2>&1; then
  # `|| true` is load-bearing: with pipefail a CLEAN tree makes grep -v
  # match nothing and exit 1, the pipeline inherits it, and set -e kills the
  # run here — before the ERR trap exists, so it dies silently with no log.
  # The guard against a dirty tree was refusing to run on a clean one.
  DIRTY=$(git -C "$REPO" status --porcelain 2>/dev/null | grep -v '^?? ' | head -20 || true)
  if [ -n "$DIRTY" ] && [ "${FILMSTOCK_ALLOW_DIRTY:-0}" != "1" ]; then
    { echo "filmstock daily: $REPO has uncommitted changes; refusing to publish from it."
      echo "$DIRTY" | sed 's/^/  /'
      echo "commit them, or re-run with FILMSTOCK_ALLOW_DIRTY=1 to publish anyway."
    } >&2
    exit 1
  fi
  COMMIT=$(git -C "$REPO" rev-parse --short HEAD 2>/dev/null || echo unknown)
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
intersource() { sqlite3 -readonly "$INTER" "select * from meta" | sed -n 's/^source|//p'; }
catlatest() {
  python3 - "$ROOT/builds.json" <<'PY'
import json,sys
print(json.load(open(sys.argv[1]))["latest"])
PY
}
# The tip's CONTENT DAY, which is what compares against the intermediate's
# incr_through. Not its id: ids are labels and no longer always dates, so
# comparing one against a day reports a mismatch every time.
catlatestthrough() {
  python3 - "$ROOT/builds.json" <<'PYQ'
import json,sys
c=json.load(open(sys.argv[1]))
print(next(b["through"] for b in c["builds"] if b["id"]==c["latest"]))
PYQ
}

# --- preflight ------------------------------------------------------------
# Everything checked here is a thing that, missing, would otherwise fail
# halfway through a 6-minute export.
[ -x "$BIN" ]     || die "no filmstock binary at $BIN (make build)"
[ -x "$SQLDIFF" ] || die "no sqldiff at $SQLDIFF (make sqldiff)"
[ -w "$INTER" ]   || die "intermediate $INTER is missing or not writable"
[ -r "$CACHE" ]   || die "resolver cache $CACHE is missing"
if [ -z "$FULL_DUMPS" ]; then
  src=$(intersource) || die "cannot read source from $INTER"
  [ -n "$src" ] || die "$INTER states no source; cannot locate its full dump set"
  FULL_DUMPS=$(dirname "$src")
fi
[ -d "$FULL_DUMPS" ] || die "full dump set $FULL_DUMPS is missing"
[ -s "$ROOT/builds.json" ] || die "no catalog at $ROOT/builds.json"

free_gb=$(df -BG --output=avail "$STAGE" | tail -1 | tr -dc '0-9')
[ "$free_gb" -ge "$MIN_FREE_GB" ] || die "only ${free_gb}G free on $STAGE, want ${MIN_FREE_GB}G"

# The intermediate and the catalog must agree about where the chain is. They
# drift by exactly one day when a previous run imported and then failed to
# publish, which is recoverable; anything wider means someone has been moving
# files around and the next patch would be computed against the wrong base.
# How far the store has advanced, asked the way catchup's lastAppliedDay asks
# it: the last adds-changes day applied, or -- when none has been, because the
# store was just imported from a full dump -- that dump's own day. This is not
# a fallback for a missing value; it is the same question with two sources, and
# refusing here would mean a freshly rebuilt chain could never take its first
# daily.
have=$(interday) || die "cannot read incr_through from $INTER"
if [ -z "$have" ]; then
  src=$(intersource) || die "cannot read source from $INTER"
  have=$(printf '%s' "$src" | grep -oE '\b20[0-9]{6}\b' | head -1)
  [ -n "$have" ] || die "$INTER states no incr_through and its source $src names no date"
  say "no incr_through yet; the store is at its import day $have"
fi
tip=$(catlatest) || die "cannot read latest from $ROOT/builds.json"
tip_through=$(catlatestthrough) || die "cannot read the tip's through from $ROOT/builds.json"
say "full dump set $FULL_DUMPS"
say "intermediate through $have; published chain tip $tip (content day $tip_through)"
if [ "$have" != "$tip_through" ]; then
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
    -db "$dir/filmstock.db" \
    -inter "$INTER" -cache "$CACHE" \
    -dumps "$INCR_DIR" -full-dumps "$FULL_DUMPS" -workers "$WORKERS"
  say "    $(timing "$day update")"

  # catchup exits 0 when there is nothing to apply, so "did the day land" is
  # asked of the intermediate rather than of the exit status.
  now=$(interday)
  [ "$now" = "$day" ] || die "$day: catchup left the intermediate at $now; refusing to publish"
  [ -s "$dir/filmstock.db" ] || die "$day: export produced no database in $dir"

  # The post-passes. They read the resolver cache and rewrite index tables in
  # the freshly exported database. Vectors are not published at present, so
  # there is nothing here to carry forward.
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
  # -through is the day the intermediate actually reached, asserted equal to
  # $day above. It is what orders the chain and what publish compares against
  # the tip; it is passed explicitly rather than inferred from -id, because an
  # id that is inferred-from is an id that cannot ever stop being a date.
  timed "$day publish" "$BIN" publish \
    -root "$ROOT" -id "$day" -dump "$day" -through "$day" \
    -from "$dir" -sqldiff "$SQLDIFF"
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
