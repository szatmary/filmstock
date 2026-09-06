#!/bin/bash
#
# filmstock monthly — rebuild from a fresh Wikimedia dump and supersede the
# chain. Normally invoked by scripts/run.sh, which is the single cron entry and
# runs the day's dailies first; safe to run by hand for a one-off rebuild.
#
# It runs every day and usually does nothing. A monthly dump appears when it
# appears — the 20260901 dump finished its article jobs on 2026-09-03 — so the
# job asks the server whether a dump this build has not consumed is ready,
# rather than believing the calendar. Not ready is not a failure.
#
# Why the rebuild is needed at all: the daily adds-changes dumps carry edits but
# not deletions. Only a full dump states which pages still exist. The dailies
# are authoritative for recency, a full is authoritative for existence, and the
# chain would drift apart without a periodic re-derivation from the whole
# corpus.
#
# THE INVARIANT (docs/CHAIN.md §2). Because neither branch dominates, a full may
# only be published once its intermediate has replayed every delta the tip
# already holds. Then it is a superset of both and nothing is reverted. This
# script's job is to establish that condition before calling publish; publish
# refuses if it has not, so a bug here fails loudly instead of walking every
# consumer backwards for a month.
#
# Two phases, and only the second holds the lock:
#
#   (no lock)  wait for readiness; fetch ~27 GB; rebuild the qidmap; import
#              into a NEW intermediate. Hours, but every write goes to a file
#              nothing else reads, so the dailies keep landing throughout.
#   (lock)     replay deltas until the new intermediate reaches the tip's
#              content day; export; post-passes; publish under the removal
#              guard; promote the new intermediate. Minutes.
#
# The promotion is a rename under the lock, so there is no window in which a
# daily could apply a day to an intermediate that is about to be replaced.
#
# Every failure before the publish leaves the old intermediate, the old resolver
# and the published chain exactly as they were, and the run is repeatable from
# the start. That is what lets a timer retry this rather than a person.

set -Eeuo pipefail

HOME_DIR=${FILMSTOCK_HOME:-/tank/mediadb}
REPO=${FILMSTOCK_REPO:-$HOME_DIR/filmstock}
BIN=${FILMSTOCK_BIN:-$REPO/filmstock}
SQLDIFF=${FILMSTOCK_SQLDIFF:-$REPO/sqldiff}
INTER=${FILMSTOCK_INTER:-$HOME_DIR/intermediate-v3.db}
CACHE=${FILMSTOCK_CACHE:-$HOME_DIR/resolver-ext.db}
DUMP_DIR=${FILMSTOCK_DUMPS:-$HOME_DIR/dump}
INCR_DIR=${FILMSTOCK_INCR:-$DUMP_DIR/incr}
ROOT=${FILMSTOCK_ROOT:-$HOME_DIR/bucket}
STAGE=${FILMSTOCK_STAGE:-$HOME_DIR/stage}
LOGDIR=${FILMSTOCK_LOGDIR:-$HOME_DIR/logs}
LOCK=${FILMSTOCK_LOCK:-$HOME_DIR/.daily.lock}
WORKERS=${FILMSTOCK_WORKERS:-18}
MIN_FREE_GB=${FILMSTOCK_MONTHLY_MIN_FREE_GB:-80}   # dump ~27 GB + intermediate ~9 GB + stage
UA=${FILMSTOCK_UA:-"filmstock/1.0 (https://github.com/szatmary/filmstock; matt@szatmary.org)"}

# Wikimedia retains adds-changes dumps for about 42 days. Past that window a
# gap in the chain cannot be closed by dailies at all. This is the one failure
# here that gets harder to fix the longer it goes unnoticed, so it escalates
# well before the cliff.
STALE_WARN_DAYS=${FILMSTOCK_STALE_WARN_DAYS:-25}
KEEP_INCR_DAYS=${FILMSTOCK_KEEP_INCR:-180}

mkdir -p "$LOGDIR" "$STAGE" "$INCR_DIR"
LOG=$LOGDIR/monthly-$(date -u +%Y%m%dT%H%M%SZ).log
STARTED=$(date -u +%s)

log() { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*" >>"$LOG"; }
say() { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; }
die() {
  { printf 'filmstock monthly: %s\n' "$*"; printf 'full log: %s\n' "$LOG"; } \
    | tee -a "$LOG" >&2
  exit 1
}
on_err() {
  local rc=$? line=${1:-?}
  {
    echo
    echo "filmstock monthly FAILED (exit $rc, line $line) after $(( $(date -u +%s) - STARTED ))s"
    echo "full log: $LOG"
    echo "--- last 40 lines ---"
    tail -40 "$LOG" 2>/dev/null
  } >&2
  exit "$rc"
}
trap 'on_err $LINENO' ERR

timed() {
  local label=$1; shift
  log "--> $label: $*"
  /usr/bin/time -f "    [$label] wall %E  cpu %P  maxrss %M kB" -a -o "$LOG" \
    "$@" >>"$LOG" 2>&1
}
timing() { grep -F "[$1]" "$LOG" | tail -1 | sed 's/^ *//'; }

jsonq() { python3 -c "import json,sys; print(json.load(open(sys.argv[1]))$1)" "$2"; }
# The dump the newest full derives from — the high-water mark of what has
# actually been consumed. Read from the catalog rather than from the dump
# directory, because a dump can be on disk from a run that failed before it
# published anything, and re-consuming it is exactly right in that case.
latestFullDump() {
  python3 -c "
import json,sys
fulls=[b for b in json.load(open(sys.argv[1]))['builds'] if b['kind']=='full']
if not fulls: sys.exit('catalog holds no full build')
print(fulls[-1]['dump'])" "$1"
}
interday() { sqlite3 -readonly "$1" "select * from meta" | sed -n 's/^incr_through|//p'; }

# --- chain health ---------------------------------------------------------
# Checked first and unconditionally: it is the thing worth knowing even on the
# runs where there is no new dump and nothing else happens.
tip=$(jsonq "['latest']" "$ROOT/builds.json")
tip_through=$(jsonq "['builds'][-1]['through']" "$ROOT/builds.json")
[ -n "$tip_through" ] || die "the chain tip $tip states no 'through'; run
'$BIN builds -catalog $ROOT/builds.json -backfill-through' once to migrate the
catalog, then re-run."
age=$(( ( $(date -u +%s) - $(date -u -d "$tip_through" +%s) ) / 86400 ))
say "chain tip $tip, content day $tip_through (${age}d old)"
if [ "$age" -ge "$STALE_WARN_DAYS" ]; then
  say "WARNING: the chain is ${age} days behind. Wikimedia retains adds-changes"
  say "         dumps ~42 days; past that this gap needs a full rebuild to close."
fi

# --- is there a dump we have not consumed? --------------------------------
# The four jobs this pipeline actually reads. The history and pagelogs jobs lag
# by days and are never consumed, so waiting on overall completion would delay
# every monthly for nothing.
latest_full_dump=$(latestFullDump "$ROOT/builds.json")
say "newest full derives from dump $latest_full_dump"

ready=""
for d in $(curl -sf -A "$UA" https://dumps.wikimedia.org/enwiki/ \
           | grep -oE '2[0-9]{7}' | sort -u | tac); do
  [ "$d" -gt "$latest_full_dump" ] || break
  status=$(curl -sf -A "$UA" "https://dumps.wikimedia.org/enwiki/$d/dumpstatus.json" || echo '{}')
  if python3 -c "
import json,sys
j=json.loads(sys.argv[1]).get('jobs',{})
need=['articlesdumprecombine','articlesmultistreamdump','pagepropstable','imagetable']
sys.exit(0 if all(j.get(k,{}).get('status')=='done' for k in need) else 1)
" "$status"; then
    ready=$d
    break
  fi
  say "dump $d exists but its article jobs are not done yet"
done

if [ -z "$ready" ]; then
  say "no new dump ready; nothing to do"
  exit 0
fi
say "dump $ready is ready to consume"

D=$ready
DUMPS=$DUMP_DIR/$D
NEW_INTER=$HOME_DIR/intermediate-v3-$D.db
NEW_CACHE=$HOME_DIR/resolver-ext-$D.db
# Fulls take a prefixed id so they can never collide with a daily's YYYYMMDD,
# which is exactly the collision that made this rebuild a design problem. The
# id is never parsed for meaning — `through` orders the chain — so it only has
# to be unique, and staying legible is worth more here than being opaque when
# the reader is a cron mail at 04:00.
ID=f$D

# --- can this dump actually reach the tip? --------------------------------
# A dump is named for the day its content was snapshotted, not the day it was
# published, and we do not control the gap. An enwiki-20261001 dump finished on
# 10/30 still has to replay every daily from 10/02 to reach the published tip.
#
# Checked here, before ~27 GB and several hours of work, because the answer
# does not change afterwards: if a day in that span is gone from both this
# machine and the server, the rebuild can never satisfy through(new) >=
# through(tip), and publish will refuse it at the very end. Failing in the
# first minute with the missing days named is the same outcome, four hours
# earlier and legible.
need_missing=$(python3 - "$D" "$tip_through" "$INCR_DIR" <<'PYQ'
import sys, os, datetime
d0 = datetime.datetime.strptime(sys.argv[1], "%Y%m%d").date()
d1 = datetime.datetime.strptime(sys.argv[2], "%Y%m%d").date()
incr = sys.argv[3]
missing = []
day = d0 + datetime.timedelta(days=1)
while day <= d1:
    stamp = day.strftime("%Y%m%d")
    if not os.path.exists(os.path.join(incr, "enwiki-%s-pages-meta-hist-incr.xml.bz2" % stamp)):
        missing.append(stamp)
    day += datetime.timedelta(days=1)
print(" ".join(missing))
PYQ
)
if [ -n "$need_missing" ]; then
  say "days not held locally, checking the server: $need_missing"
  server=$(curl -sf -A "$UA" https://dumps.wikimedia.org/other/incr/enwiki/ \
           | grep -oE '2[0-9]{7}' | sort -u || true)
  gone=""
  for d in $need_missing; do
    echo "$server" | grep -qx "$d" || gone="$gone $d"
  done
  if [ -n "$gone" ]; then
    die "dump $D cannot reach the chain tip ($tip_through).
These adds-changes days are needed to carry it forward and are held neither
here nor on the server:$gone
The dump's content stops at $D, so without them the rebuild would be older than
the chain and publish would refuse it. Nothing is wrong with the chain: dailies
continue to work. Either a later dump arrives close enough to the tip to be
usable, or the gap needs a human. Raising FILMSTOCK_KEEP_INCR (now ${KEEP_INCR_DAYS:-180}d)
is what stops this recurring."
  fi
  say "all missing days are still on the server; catchup will fetch them"
fi

[ -x "$BIN" ]     || die "no filmstock binary at $BIN (make build)"
[ -x "$SQLDIFF" ] || die "no sqldiff at $SQLDIFF (make sqldiff)"
free_gb=$(df -BG --output=avail "$HOME_DIR" | tail -1 | tr -dc '0-9')
[ "$free_gb" -ge "$MIN_FREE_GB" ] || die "only ${free_gb}G free, want ${MIN_FREE_GB}G"

if [ -e "$ROOT/$ID" ]; then
  die "$ROOT/$ID already exists. A published build is immutable, so this is
either a build that is already done (the catalog is behind and wants looking
at) or a publish that died half-written. Inspect $ROOT/$ID and
$ROOT/builds.json, remove the directory if it is half-written, and re-run."
fi

# The binary must come from committed code: a build published from a dirty tree
# carries whatever was half-finished in it, and its manifest's content hash is
# computed with a spec that exists only in someone's working tree.
if command -v git >/dev/null && git -C "$REPO" rev-parse --git-dir >/dev/null 2>&1; then
  # `|| true` is load-bearing: with pipefail a CLEAN tree makes grep -v
  # match nothing and exit 1, the pipeline inherits it, and set -e kills the
  # run here — before the ERR trap exists, so it dies silently with no log.
  # The guard against a dirty tree was refusing to run on a clean one.
  DIRTY=$(git -C "$REPO" status --porcelain 2>/dev/null | grep -v '^?? ' | head -20 || true)
  if [ -n "$DIRTY" ] && [ "${FILMSTOCK_ALLOW_DIRTY:-0}" != "1" ]; then
    { echo "filmstock monthly: $REPO has uncommitted changes; refusing to publish from it."
      echo "$DIRTY" | sed 's/^/  /'
    } >&2
    exit 1
  fi
fi

# ==========================================================================
# Phase 1 — no lock. Every write below goes to a file nothing else reads.
# ==========================================================================

# --- fetch ----------------------------------------------------------------
# Sequential wget with a contact User-Agent. aria2's default 5-way split is
# rejected wall-to-wall with 429 by dumps.wikimedia.org (dump/aria2.log,
# 2026-08-04); one connection sustains ~5 MB/s and never trips the limiter.
FILES=(
  "enwiki-$D-image.sql.gz"
  "enwiki-$D-page_props.sql.gz"
  "enwiki-$D-pages-articles-multistream-index.txt.bz2"
  "enwiki-$D-pages-articles-multistream.xml.bz2"
)
mkdir -p "$DUMPS"
if [ ! -s "$DUMPS/.fetched" ]; then
  say "fetching the $D dump set (~27 GB, ~100 min at one connection)"
  curl -sf -A "$UA" -o "$DUMPS/md5sums.txt" \
    "https://dumps.wikimedia.org/enwiki/$D/enwiki-$D-md5sums.txt" \
    || die "cannot fetch md5sums for $D"
  for f in "${FILES[@]}"; do
    timed "fetch $f" wget -c -q -U "$UA" --tries=5 --waitretry=30 --timeout=60 \
      -P "$DUMPS" "https://dumps.wikimedia.org/enwiki/$D/$f"
    want=$(grep -E "  $f\$" "$DUMPS/md5sums.txt" | awk '{print $1}')
    got=$(md5sum "$DUMPS/$f" | awk '{print $1}')
    [ "$want" = "$got" ] || die "$f: md5 mismatch (want $want, got $got)"
    say "    $f verified; $(timing "fetch $f")"
  done
  # The wikidata entity dump is a separate, much larger cadence; the resolver
  # reuses the one already on disk rather than re-fetching 102 GB monthly.
  ln -sfn "$DUMP_DIR/latest-all.json.bz2" "$DUMPS/latest-all.json.bz2"
  # Written only once every file above has passed its md5: the marker means
  # "verified", not "downloaded".
  touch "$DUMPS/.fetched"
fi

# --- resolver -------------------------------------------------------------
# The P179/P4908 wikidata edges come from an entity dump this run does not
# refresh, so the previous cache is copied and only the two dump-derived tables
# are rebuilt. Both are DROP/CREATE, so no stale row from the old dump survives.
if [ ! -s "$NEW_CACHE" ]; then
  timed "cache copy" cp "$CACHE" "$NEW_CACHE"
fi
if [ ! -f "$NEW_CACHE.built" ]; then
timed "qidmap" "$BIN" build-qidmap \
  -pageprops "$DUMPS/enwiki-$D-page_props.sql.gz" \
  -index "$DUMPS/enwiki-$D-pages-articles-multistream-index.txt.bz2" \
  -db "$NEW_CACHE"
say "    $(timing qidmap)"
timed "image-list" "$BIN" build-image-list \
  -sql "$DUMPS/enwiki-$D-image.sql.gz" -db "$NEW_CACHE"
say "    $(timing image-list)"
  touch "$NEW_CACHE.built"
else
  say "resolver cache already built for $D; reusing it"
fi

# --- import ---------------------------------------------------------------
# Into a NEW intermediate, never over the live one: a full import sets `source`
# but leaves `incr_through` where it was, so importing in place would rewind
# pages to the dump's day while still claiming every later day had been
# applied — and catchup would skip those days forever.
# Resumed on an explicit marker, never on the file merely existing. A crashed
# import leaves a large, well-formed, INCOMPLETE intermediate, and "-s" cannot
# tell that from a finished one — the run would converge, export and publish
# from a truncated corpus. The marker is written only after import returns 0.
if [ ! -f "$NEW_INTER.imported" ]; then
  rm -f "$NEW_INTER"
  timed "import" "$BIN" import -dumps "$DUMPS" -inter "$NEW_INTER" -workers "$WORKERS"
  say "    $(timing import)"
  [ -s "$NEW_INTER" ] || die "import produced no intermediate at $NEW_INTER"
  touch "$NEW_INTER.imported"
else
  say "import already complete for $D (marker $NEW_INTER.imported); reusing it"
fi

# ==========================================================================
# Phase 2 — under the lock. The tip must not move while we converge on it.
# ==========================================================================
exec 9>"$LOCK"
say "waiting for $LOCK"
flock 9
say "lock held; converging"

# Re-read the tip: dailies may have landed during the hours of phase 1.
tip=$(jsonq "['latest']" "$ROOT/builds.json")
tip_through=$(jsonq "['builds'][-1]['through']" "$ROOT/builds.json")
say "tip is now $tip (content day $tip_through)"

# --- converge -------------------------------------------------------------
# Replay every delta the tip holds into the new intermediate. This is what
# makes the rebuild a superset of both branches rather than a competing one.
have=$(interday "$NEW_INTER" || true)
if [ -z "$have" ]; then
  # A freshly imported intermediate states no incr_through; its content day is
  # the dump's own day, which is what catchup falls back to internally.
  have=$D
fi
say "new intermediate holds $have; the tip holds $tip_through"

if [ "$have" \< "$tip_through" ]; then
  timed "converge" "$BIN" catchup \
    -db "$STAGE/$ID/filmstock.db" \
    -inter "$NEW_INTER" -cache "$NEW_CACHE" \
    -dumps "$INCR_DIR" -full-dumps "$DUMPS" -workers "$WORKERS" -keep
  say "    $(timing converge)"
  have=$(interday "$NEW_INTER")
fi

# The invariant, asserted here as well as in publish. Publish is the guard that
# matters; this one names the script's own bug if convergence quietly did
# nothing, which is a better diagnosis than a refusal three steps later.
if [ "$have" \< "$tip_through" ]; then
  die "convergence left the new intermediate at $have but the tip holds
$tip_through. Publishing now would bridge backwards. The adds-changes dumps for
$have..$tip_through may have aged out of Wikimedia's ~42-day retention, in which
case the tip cannot be reproduced and this needs a human."
fi
say "converged: new intermediate holds $have (tip $tip_through)"

# --- export + post-passes -------------------------------------------------
# Exported unconditionally even when catchup already wrote a build: catchup's
# output is the last day's, and the full must be a clean re-derivation of the
# whole corpus rather than whatever the final day happened to leave.
rm -rf "$STAGE/$ID"; mkdir -p "$STAGE/$ID"
timed "export" "$BIN" export \
  -inter "$NEW_INTER" -db "$STAGE/$ID/filmstock.db" \
  -cache "$NEW_CACHE" -dumps "$DUMPS" -workers "$WORKERS"
say "    $(timing export)"
[ -s "$STAGE/$ID/filmstock.db" ] || die "export produced no database"

timed "post" bash -c \
  "'$BIN' index-external-ids -db '$STAGE/$ID/filmstock.db' -cache '$NEW_CACHE' && \
   '$BIN' index-series       -db '$STAGE/$ID/filmstock.db' -cache '$NEW_CACHE'"
say "    $(timing post)"

# --- publish --------------------------------------------------------------
# -through is the assertion the whole design rests on; -dump records what the
# content derives from, which is no longer implied by the id.
#
# -page-set is the resolver cache built from THIS dump, so publish can check
# every record the bridge removes against the dump's own page set. There is no
# size threshold: a bridge may repair any number of records (this one carries
# the aged-out 20260802–20260817 window) but may only remove a record whose
# page is genuinely gone. See docs/CHAIN.md §3.
timed "publish" "$BIN" publish \
  -root "$ROOT" -id "$ID" -dump "$D" -through "$have" \
  -from "$STAGE/$ID" -full -page-set "$NEW_CACHE" -sqldiff "$SQLDIFF"
say "    $(timing publish)"

published=$(jsonq "['latest']" "$ROOT/builds.json")
[ "$published" = "$ID" ] || die "published, but the catalog's latest is $published"
grep -E 'bridge|carried' "$LOG" | tail -5 | sed 's/^/    /' | tee -a "$LOG" || true

# --- promote --------------------------------------------------------------
# The rename is what makes tomorrow's daily use the new intermediate, and it
# happens under the lock so no daily can be mid-apply on the file being
# replaced. The old one is kept beside it: it is the only way back if the new
# chain turns out to be wrong, and it costs 9 GB.
timed "promote" bash -c "
  mv -f '$INTER' '$INTER.superseded-$D' &&
  mv -f '$NEW_INTER' '$INTER' &&
  mv -f '$CACHE' '$CACHE.superseded-$D' &&
  cp -f '$NEW_CACHE' '$CACHE'"
say "promoted: $INTER now holds $D + deltas through $have"

rm -rf "$STAGE/$ID"
say "monthly complete in $(( ( $(date -u +%s) - STARTED ) / 60 )) min: published $ID"
say "  previous intermediate kept at $INTER.superseded-$D"
