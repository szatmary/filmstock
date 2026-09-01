#include "zvfs_int.h"

/* Quota: a small constant base plus a term proportional to the fragmentation
   ratio (free bytes / eof), capped so one commit's compaction work stays
   bounded regardless of how fragmented the file has become. Compile-time
   policy -- not configurable, deliberately conservative so zctr_sync's cost
   stays close to O(dirty pages) even on a badly fragmented container. */
static u32 quota(const ZvfsContainer *c){
  /* Task 15: zcompact_full's pack pass wants every candidate relocated in
     one commit, not the self-regulating trickle ordinary commits use. */
  if(c->compactFull) return UINT32_MAX;
  u64 eof = zalloc_eof(c->alloc);
  u64 fb = zalloc_free_bytes(c->alloc);
  u32 q = (u32)((fb*256)/(eof+1));
  return 2 + (q>64 ? 64 : q);
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

int zcompact_step(ZvfsContainer *c, u64 txn){
  /* Task 15: reset before the early-return below too, so a fully-dense
     container correctly reports "moved nothing" rather than a stale count
     from whatever call last set it. */
  c->lastCompactMoved = 0;
  ZExt last;
  if(!zalloc_last_free(c->alloc, &last)) return SQLITE_OK;  /* dense: nothing to do */
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
  u64 pos = last.off + last.len;                            /* start of the tail run */
  /* Task 19 (found via test/bigsmoke.sh -- the only real workload in this
  ** project that ever drives a container's eof past the locking-page hole,
  ** design spec S4.5/S10 item 4 -- and root-caused, not just documented;
  ** widened after Task 19's own review round added dedicated coverage --
  ** test/unit/test_lockhole.c -- and found the original fix below was too
  ** narrow): the tail walk assumes every byte from `pos` to `eof` is
  ** either a self-describing record or already-accounted-for free space,
  ** per S4.2's whole design ("the compactor can walk any allocated run
  ** forward"). That assumption breaks exactly once: zalloc_reserve_eof
  ** (alloc.c), the only place eof ever crosses the hole, turns the
  ** leftover [old_eof, HOLE0) gap into an ordinary free extent and then
  ** jumps eof straight to HOLE1 -- the hole itself, [HOLE0, HOLE1), is
  ** never a free extent, a pending extent, or a record; it simply does
  ** not exist in this container's bookkeeping, exactly like SQLite's own
  ** locking page. No real record can ever START anywhere inside
  ** [HOLE0, HOLE1) -- the allocator's own placement never straddles or
  ** enters the hole -- but `pos` can still ARRIVE there two different
  ** ways, not just one:
  **   1. If the pre-hole pad extent happens to be the highest-offset free
  **      extent zalloc_last_free returns (true the very first time eof
  **      crosses the hole with nothing above it freed yet), `pos` starts
  **      there directly: `last.off+last.len` computes to exactly HOLE0.
  **   2. If `last` is some OTHER, lower free extent instead (ordinary
  **      fragmentation churn, or ANY commit after the first crossing),
  **      `pos` starts BELOW the hole and walks forward one real record at
  **      a time (`pos += total`) -- and can land exactly on HOLE0 through
  **      perfectly ordinary iteration, the instant the cumulative record
  **      sizes between the walk's start and the pre-hole pad happen to sum
  **      to exactly that offset. This is NOT a narrower or rarer case than
  **      (1) -- confirmed via test_lockhole.c's own per-write-sync churn,
  **      which reproduces it reliably within a few hundred ordinary
  **      commits, no crash/edge-of-file timing needed.
  ** Either way the walk then tries to parse a record header out of the
  ** hole's own unwritten bytes, hitting garbage (confirmed via
  ** instrumented reproduction, both shapes: SQLITE_IOERR_READ, "bad
  ** record magic", from container.c's own zctr_read_rechdr). A ONE-TIME
  ** check before the loop (checking only `pos`'s initial value) catches
  ** shape 1 but not shape 2 -- the check must be inside the loop,
  ** re-examining `pos` after every step, to catch both: skip straight to
  ** HOLE1, the same landing point zalloc_reserve_eof itself uses,
  ** whenever `pos` -- initial or freshly stepped-to -- lands in the
  ** hole, before ever trying to parse anything there. */
  u64 eof = zalloc_eof(c->alloc);
  u32 left = quota(c);
  while(left && pos < eof){
    if(pos >= ZVFS_LOCK_HOLE_OFF && pos < (u64)ZVFS_LOCK_HOLE_OFF + ZVFS_LOCK_HOLE_LEN){
      pos = (u64)ZVFS_LOCK_HOLE_OFF + ZVFS_LOCK_HOLE_LEN;
      continue;
    }
    ZvfsRec r;
    int rc = zctr_read_rechdr(&c->io, pos, &r);
    if(rc) return rc;                       /* tail run must parse; else corrupt */
    u32 total = ZREC_TOTAL(r.nPayload);
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
          left--;
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
          left--;
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
