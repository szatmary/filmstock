#include "zvfs_int.h"

/* Item 1 fix (docs/design.md Sec7.3's former OPEN BUG: a fragmentation
** BURST -- many gaps scattered by one commit's own churn -- converged over
** ~N subsequent commits instead of a handful). Two independent constraints
** combined to cause that, both explained in detail at their own call sites
** below: (1) the old zcompact_step only ever looked at the records ABOVE
** the SINGLE highest-offset free extent ("the candidate set is only the
** tail run"), and (2) the old quota() capped relocation at a small, FIXED
** RECORD COUNT (<=66) regardless of how much a single commit's churn just
** fragmented. Fix round 3 (this one) widens the candidate set to every
** gap-free run in the file, processed highest-offset-first within one
** commit, AND widens the budget to scale with how much work is actually on
** the table -- confirmed by measurement (see the report this change ships
** with) that widening only ONE of the two does not help: a bigger quota
** alone still only ever reached the single topmost run (no measurable
** difference, WIP'd and rejected -- see quota-byte-budget-WIP.patch and
** design.md's own "rejected approaches" record); a wider candidate set
** alone would still cap out at a few dozen records per commit, unable to
** fully vacate more than one or two of the newly-scattered runs per
** commit. Paired, they are what lets ONE post-burst commit fully vacate
** several runs at once, so the FOLLOWING commit's ordinary start-of-commit
** release+trim (Sec5.3, unchanged) retracts eof past the whole merged
** region in a single shot -- see zcompact_step's own comment for the run
** enumeration and why it is safe, and quota_bytes below for the budget. */

/* Byte budget: FLOOR keeps quiet, barely-fragmented databases creeping
** toward dense every commit (matching the old formula's own "+2 minimum"
** intent, just denominated in bytes instead of records since a byte budget
** is what actually correlates with a run's real relocation cost -- page and
** node records vary widely in size, so a flat record count doesn't). CAP
** bounds one commit's worst-case relocation cost so an adversarially
** fragmented container can't make a single ordinary commit unboundedly
** slow (that is what compactFull/VACUUM's own genuinely unbounded pass,
** below, is for). Between them: proportional to
** ZvfsContainer.bytesReleasedThisCommit (zvfs_int.h) -- the backlog THIS
** commit's own step-0 release just exposed, i.e. what a PRIOR commit's
** churn (most interestingly, a burst) left pending. That is deliberately
** the ONLY term: an earlier version of this formula also added
** bytesWrittenThisTxn (this commit's OWN new page bytes), reasoning by
** analogy with quota-byte-budget-WIP.patch -- measurement (RUN_BIG=1 `make
** bigsmoke`) caught this as a real regression, not just a theoretical one:
** a large bulk-INSERT commit writes many megabytes but releases nothing
** (nothing is being freed), yet bytesWrittenThisTxn alone was already
** enough to saturate CAP on its own, turning every ordinary bulk-load
** commit into a full-budget compaction pass with nothing worthwhile to
** compact. bytesReleasedThisCommit alone still gives burst recovery its
** O(1)-ish convergence (see zcompact_step's own comment): the commit right
** after a burst releases that burst's entire backlog at its own step 0,
** sizing exactly the commit that can act on it, regardless of how much or
** little that commit itself happens to write. (A second, independent
** overspending bug lived in compact_run's own budget accounting below --
** see its comment -- and mattered more in practice than this term did;
** both are fixed together, see the report for the full A/B.) Values are
** compile-time policy, chosen from measurement (see the report), not
** configurable. */
#define ZCOMPACT_BUDGET_FLOOR   (64u*1024u)
#define ZCOMPACT_BUDGET_CAP     (4u*1024u*1024u)
/* Dense-file gate: below this free/eof ratio, spend only the floor -- a
** tightly-packed file doesn't get an unbounded license to churn every
** commit just because THIS commit happened to release a lot in absolute
** terms; matches the spirit of the old quota()'s own fragmentation-ratio
** term. */
#define ZCOMPACT_DENSE_NUM      1
#define ZCOMPACT_DENSE_DEN      16

static u64 quota_bytes(const ZvfsContainer *c){
  /* Task 15: zcompact_full's pack pass wants every candidate relocated in
     one commit, not the self-regulating trickle ordinary commits use. */
  if(c->compactFull) return UINT64_MAX;
  u64 eof = zalloc_eof(c->alloc);
  u64 fb = zalloc_free_bytes(c->alloc);
  if(fb * ZCOMPACT_DENSE_DEN < eof * ZCOMPACT_DENSE_NUM) return ZCOMPACT_BUDGET_FLOOR;
  u64 budget = c->bytesReleasedThisCommit;
  if(budget < ZCOMPACT_BUDGET_FLOOR) budget = ZCOMPACT_BUDGET_FLOOR;
  if(budget > ZCOMPACT_BUDGET_CAP) budget = ZCOMPACT_BUDGET_CAP;
  return budget;
}

/* Is the node record at (level,firstPgno,off) reachable from the current
   committed tree? Walk down from the root to that level and compare the
   offset found there. A mismatch means either a stale record from a
   superseded generation, or (within this same zcompact_step call) a node
   this walk already relocated earlier -- either way, not live, skip it:
   that's what keeps a single commit from moving the same node twice. */
static int node_is_live(ZvfsContainer *c, u8 level, u32 firstPgno, u64 off, int *pLive){
  u64 treeOff;
  int rc = zmap_node_at(c->map, level, firstPgno, &treeOff);
  if(rc) return rc;
  *pLive = (treeOff == off);
  return SQLITE_OK;
}

/* Walk ONE gap-free run [pos, runEnd), relocating live records into
** best-fit gaps strictly below `pos` until either the run is exhausted or
** *pBudget hits zero. Identical record-handling logic to the pre-fix
** single-run zcompact_step, just parameterized by the run's own bounds
** instead of hardcoding pos=last.off+last.len/eof -- see zcompact_step
** below for why walking a run this way (self-describing headers, forward,
** never past a free gap) is valid for ANY gap-free run, not only the
** topmost one, and for the lock-hole skip inlined here. */
static int compact_run(ZvfsContainer *c, u64 txn, u64 pos, u64 runEnd, u64 *pBudget){
  while(*pBudget && pos < runEnd){
    if(pos >= ZVFS_LOCK_HOLE_OFF && pos < (u64)ZVFS_LOCK_HOLE_OFF + ZVFS_LOCK_HOLE_LEN){
      pos = (u64)ZVFS_LOCK_HOLE_OFF + ZVFS_LOCK_HOLE_LEN;
      continue;
    }
    ZvfsRec r;
    int rc = zctr_read_rechdr(&c->io, pos, &r);
    if(rc) return rc;                       /* run must parse; else corrupt */
    u32 total = ZREC_TOTAL(r.nPayload);
    /* Budget is spent on every record EXAMINED here, not only on ones that
    ** actually get relocated -- deliberately, and not merely a formality:
    ** an append-mostly workload can walk through a large run finding
    ** nothing but live records with nowhere lower to go (no free extent
    ** below most of them at all), which would otherwise never decrement a
    ** "bytes relocated" budget and so never stop -- multiplied across every
    ** run in the file (this function's caller processes ALL of them, not
    ** just the topmost), an earlier version of this fix that only spent on
    ** actual relocations turned that into an O(file size) scan on literally
    ** every commit, confirmed by direct instrumentation (zcompact_step's
    ** own nFree/free-bytes were far too small to explain the slowdown by
    ** themselves -- the cost was in scanning, not moving) and by a clean,
    ** isolated RUN_BIG=1 `make bigsmoke` A/B (see the report). Spending on
    ** every examined record keeps this function's own cost bounded by
    ** *pBudget regardless of how unproductive a run turns out to be,
    ** restoring the "close to O(dirty pages)" property the quota always
    ** intended (compact.c's own top-of-file comment). */
    *pBudget -= (*pBudget < total) ? *pBudget : total;
    if(r.type==ZREC_PAGE){
      ZvfsMapEntry e;
      rc = zmap_get(c->map, zrec_key_pgno(r.key), &e);
      if(rc) return rc;
      if(e.off==pos){                                       /* live page record */
        u64 dst = zalloc_peek(c->alloc, total);
        if(dst && dst < pos){
          rc = zctr_read_record(&c->io, pos, &r, c->paybuf, c->pageSize);
          if(rc) return rc;
          u64 nOff = zalloc_take(c->alloc, total);          /* == dst */
          rc = zctr_write_record(&c->io, nOff, &r, c->paybuf);
          if(rc) return rc;
          ZvfsMapEntry ne = { .off=nOff, .nPayload=e.nPayload, .flags=e.flags };
          rc = zmap_set(c->map, zrec_key_pgno(r.key), &ne);
          if(rc) return rc;
          zalloc_free(c->alloc, pos, total, txn);
          c->lastCompactMoved++;
        }
      }
    }else if(r.type==ZREC_NODE){
      int live=0;
      int rc2 = node_is_live(c, zrec_key_level(r.key), zrec_key_pgno(r.key), pos, &live);
      if(rc2) return rc2;
      if(live){
        u64 dst = zalloc_peek(c->alloc, total);
        if(dst && dst < pos){
          /* touch one page under this node: identity zmap_set forces the COW
             commit to rewrite the node (and its ancestors) into lower extents */
          u32 pg = zrec_key_pgno(r.key);
          ZvfsMapEntry e;
          rc2 = zmap_get(c->map, pg, &e);
          if(rc2) return rc2;
          rc2 = zmap_set(c->map, pg, &e);
          if(rc2) return rc2;
          c->lastCompactMoved++;
        }
      }
    }
    /* ZREC_FREELIST / ZREC_PENDING: skipped -- rewritten every commit anyway.
       Stale records (map no longer points here): skipped -- they are pending
       space; the walk just steps over them. */
    pos += total;
  }
  return SQLITE_OK;
}

/* Item 1 fix: descending multi-run sweep, replacing the old single-
** (topmost-)run walk. The free-extent index (alloc.c's fr[]) is kept
** offset-sorted and coalesced, so its complement -- the allocated region --
** is a sequence of gap-free RUNS: [hdr-block-end, f0.off), [f0.end,
** f1.off), ..., [fLast.end, eof). The pre-fix code already relied on the
** topmost of these (the region above the highest-offset free extent) being
** internally gap-free -- that's the whole reason the self-describing
** forward-header walk (Sec4.2) is valid there at all -- it just never
** looked at any OTHER run. Every consecutive pair of fr[] entries bounds an
** equally gap-free run, by the same coalescing invariant, so the identical
** walk is valid on ALL of them.
**
** Processed HIGHEST-OFFSET RUN FIRST, descending: only relocations that
** shrink the file are useful (design.md's own conclusion from the rejected
** attempts -- prioritizing low-offset candidates repacks the middle of the
** file without ever lowering eof), and only a FULLY vacated top run turns
** into one single trim-able span once the NEXT commit's start-of-commit
** release (Sec5.3) exposes it -- relocating the top K runs in one commit
** vacates that whole top region; the frees are pending (this commit cannot
** trim what it just freed -- unchanged), but the next commit's release
** merges them with whatever gaps used to separate those runs and a single
** zalloc_trim retracts eof past the entire region in one shot. That is
** what turns "~N commits for N gaps" into "a handful of commits" -- see
** quota_bytes above for the other half (a budget wide enough to actually
** finish vacating more than one run's worth of records per commit).
**
** Safety: the run boundaries are computed ONCE, from a SNAPSHOT of fr[]
** taken before any relocation in this pass, not recomputed live between
** runs. This matters because relocating a record consumes space from
** wherever zalloc_take's best-fit lands (any free extent, not just the one
** immediately below the run being processed) -- if a lower run's own
** boundary were re-queried live mid-sweep, an earlier (higher) run's own
** relocations landing in that exact gap would move it. Snapshotting avoids
** needing to reason about that: a run's INTERIOR is a fixed set of bytes
** that was fully occupied by real records at snapshot time, and nothing
** during this whole pass can place a NEW record inside it (destinations
** only ever come from extents that were already free at snapshot time,
** and this pass's own frees stay pending -- never added to fr[] -- until
** released by a LATER commit, Sec5.3) -- so every run's own walk remains
** exactly as valid at the end of the sweep as it was at the start,
** regardless of what any other run's processing does. The only visible
** effect a shrunk-or-consumed neighboring free extent can have is that a
** later (lower) run's OWN upper bound, read from the snapshot, might no
** longer match the CURRENT live boundary -- harmless: the snapshot bound
** is still the correct edge of that run's own untouched interior, so at
** worst a stale live boundary elsewhere makes some OTHER run's candidate
** search find slightly less room to relocate INTO (conservative -- fewer
** relocations attempted, never an incorrect one), never a run being
** walked past its own true, stable end. */
int zcompact_step(ZvfsContainer *c, u64 txn){
  /* Task 15: reset before the early-return below too, so a fully-dense
     container correctly reports "moved nothing" rather than a stale count
     from whatever call last set it. */
  c->lastCompactMoved = 0;
  u32 nFree = zalloc_free_count(c->alloc);
  if(nFree==0) return SQLITE_OK;  /* dense: nothing to do */
  /* PAGE records get read whole into c->paybuf below; a txn whose only staged
     change was a truncate (zctr_truncate never touches pgbuf/paybuf) can
     reach zctr_sync -- and this compaction pass -- without paybuf ever
     having been allocated. zctr_ensure_scratch (container.c), not a bare
     paybuf-only allocation here: see its own comment for the leak an
     earlier, uncoordinated version of this guard caused. */
  {
    int rc0 = zctr_ensure_scratch(c);
    if(rc0) return rc0;
  }
  ZExt *snap = sqlite3_malloc64((u64)nFree * sizeof(ZExt));
  if(!snap) return SQLITE_NOMEM;
  for(u32 i=0; i<nFree; i++){
    int rc = zalloc_free_at(c->alloc, i, &snap[i]);
    if(rc){ sqlite3_free(snap); return rc; }
  }
  u64 eof = zalloc_eof(c->alloc);
  u64 budget = quota_bytes(c);
  int rc = SQLITE_OK;
  /* k = nFree: topmost run [snap[nFree-1].end, eof). k in [1,nFree-1]:
     [snap[k-1].end, snap[k].off). k = 0: lowest run [hdr-block-end,
     snap[0].off). Descending: k from nFree down to 0. */
  for(i64 k=(i64)nFree; k>=0 && budget; k--){
    u32 kk = (u32)k;
    u64 runStart = (kk==0) ? (u64)ZVFS_HDR_BLOCK_SIZE : snap[kk-1].off + snap[kk-1].len;
    u64 runEnd   = (kk==nFree) ? eof : snap[kk].off;
    if(runStart >= runEnd) continue;   /* empty run (adjacent frees / hole edge) */
    /* Task 19 lock-hole tail-walk hazard (test/unit/test_lockhole.c): see
       compact_run's own inlined skip -- kept per-run since any run, not
       only the topmost, can straddle the hole once eof has crossed it. */
    rc = compact_run(c, txn, runStart, runEnd, &budget);
    if(rc) break;
  }
  sqlite3_free(snap);
  return rc;
}

/* Task 15: one unbounded-quota pack pass, wrapped as one ordinary
** crash-safe commit -- see this function's declaration in zvfs_int.h for
** the full return-value contract (1 progress / 0 done / negative -rc
** error). gateOk is hardcoded 1: OVERWRITE only ever fires on
** rollback-journal commits under SQLite's own EXCLUSIVE lock, so the
** WAL-checkpoint reader gate (Task 18) never applies here.
**
** "Progress" can't be judged from eof alone: a pass that relocates records
** but doesn't yet expose a trim-able tail (this same commit's own frees
** stay pending until strictly after its own header flip, per §5.3 -- see
** commit_once/zalloc_release's comments) would look like a no-op by eof
** alone, causing the caller's loop to stop one pass too early, before the
** next pass's start-of-commit release+trim ever gets to reclaim what THIS
** pass set up. c->lastCompactMoved (set by zcompact_step, called from
** inside commit_once below since c->rebuild is already 0 by the time the
** pack loop runs) is what catches that case.
**
** NOTE (found via e_vacuum.test): this return value is a genuine "did a
** relocation look worthwhile" signal, not a guarantee that it landed
** somewhere strictly better once the commit's own cascading COW placement
** (which the best-fit peek behind "moved" can't fully predict) actually
** finishes -- relocating any node unconditionally rewrites every ancestor
** up to the root (zmap_commit's own design), and for some layouts an
** interior node and an ancestor can perpetually trade positions, each
** pass's fresh placement undoing the last one's improvement. A caller
** that loops `while(zcompact_full(c))` without its own independent
** eof-based stall guard can loop forever on such an input even though
** every individual pass here remains a fully valid, crash-safe commit on
** its own -- see container.c's zctr_sync_rebuild for that guard. */
int zcompact_full(ZvfsContainer *c){
  u64 eofBefore = zalloc_eof(c->alloc);
  c->compactFull = 1;
  int rc = commit_once(c, /*gateOk=*/1);
  c->compactFull = 0;
  if(rc) return -rc;
  int moved = c->lastCompactMoved;
  u64 eofAfter = zalloc_eof(c->alloc);
  return (moved>0 || eofAfter<eofBefore) ? 1 : 0;
}
