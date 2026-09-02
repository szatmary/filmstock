#include "zvfs_int.h"

void zhdr_encode(const ZvfsHdr *h, u8 o[ZVFS_HDR_COPY_SIZE]){
  memset(o, 0, ZVFS_HDR_COPY_SIZE);
  memcpy(o, ZVFS_MAGIC, 16);
  put32le(o+16, 1);            /* format_version */
  put32le(o+20, 1);            /* codec_id: zstd */
  put32le(o+24, h->pageSize);
  put64le(o+28, h->pageCount);
  put64le(o+36, h->txn);
  put64le(o+44, h->mapRoot);
  put64le(o+52, h->freeOff);
  put64le(o+60, h->pendOff);
  put64le(o+68, h->eof);
  put32le(o+76, h->flags);
  put32le(o+508, zcrc32(0, o, 508));
}
int zhdr_decode(const u8 i[ZVFS_HDR_COPY_SIZE], ZvfsHdr *h){
  if(memcmp(i, ZVFS_MAGIC, 16)!=0) return SQLITE_NOTADB;
  if(get32le(i+508) != zcrc32(0, i, 508)) return SQLITE_NOTADB;
  if(get32le(i+16)!=1 || get32le(i+20)!=1) return SQLITE_NOTADB;
  h->pageSize = get32le(i+24);
  h->pageCount = get64le(i+28);
  h->txn = get64le(i+36);
  h->mapRoot = get64le(i+44);
  h->freeOff = get64le(i+52);
  h->pendOff = get64le(i+60);
  h->eof = get64le(i+68);
  h->flags = get32le(i+76);
  return SQLITE_OK;
}
int zhdr_pick(const u8 *a, const u8 *b, ZvfsHdr *out, int *pWhich){
  ZvfsHdr ha, hb;
  int va = zhdr_decode(a, &ha)==SQLITE_OK;
  int vb = zhdr_decode(b, &hb)==SQLITE_OK;
  if(!va && !vb) return SQLITE_NOTADB;
  if(va && (!vb || ha.txn >= hb.txn)){ *out=ha; *pWhich=0; }
  else { *out=hb; *pWhich=1; }
  return SQLITE_OK;
}

void zrec_encode(const ZvfsRec *r, u8 o[ZVFS_REC_HDR_SIZE]){
  put16le(o, ZVFS_REC_MAGIC);
  o[2] = r->type; o[3] = r->flags;
  put32le(o+4, r->nPayload);
  put64le(o+8, r->key);
  put32le(o+16, r->crc);
  put32le(o+20, 0);
}
int zrec_decode(const u8 i[ZVFS_REC_HDR_SIZE], ZvfsRec *r){
  if(get16le(i)!=ZVFS_REC_MAGIC) return SQLITE_IOERR_READ;
  r->type=i[2]; r->flags=i[3];
  r->nPayload=get32le(i+4); r->key=get64le(i+8); r->crc=get32le(i+16);
  if(r->type<ZREC_PAGE || r->type>ZREC_PENDING) return SQLITE_IOERR_READ;
  return SQLITE_OK;
}

int zctr_write_record(const ZvfsIO *io, u64 off, const ZvfsRec *r, const u8 *payload){
  u8 h[ZVFS_REC_HDR_SIZE];
  zrec_encode(r, h);
  int rc = io->xWrite(io->ctx, h, ZVFS_REC_HDR_SIZE, off);
  if(rc==SQLITE_OK) rc = io->xWrite(io->ctx, payload, r->nPayload, off+ZVFS_REC_HDR_SIZE);
  return rc;
}
int zctr_read_rechdr(const ZvfsIO *io, u64 off, ZvfsRec *r){
  u8 h[ZVFS_REC_HDR_SIZE];
  int rc = io->xRead(io->ctx, h, ZVFS_REC_HDR_SIZE, off);
  return rc ? rc : zrec_decode(h, r);
}
int zctr_read_record(const ZvfsIO *io, u64 off, ZvfsRec *r, u8 *payload, u32 cap){
  int rc = zctr_read_rechdr(io, off, r);
  if(rc) return rc;
  if(r->nPayload > cap) return SQLITE_IOERR_READ;
  return io->xRead(io->ctx, payload, r->nPayload, off+ZVFS_REC_HDR_SIZE);
}

/* struct ZvfsContainer moved to zvfs_int.h in Task 14 (compact.c needs it). */

/* Load the free/pending extent lists named by the committed header into
   c->alloc. No-op once loaded (readers that never write never pay for it). */
static int zctr_load_alloc(ZvfsContainer *c){
  if(c->allocLoaded) return SQLITE_OK;
  u8 *fb=0, *pb=0; u32 nf=0, np=0; ZvfsRec r; int rc=SQLITE_OK;
  if(c->hdr.freeOff){
    rc = zctr_read_rechdr(&c->io, c->hdr.freeOff, &r);
    if(rc==SQLITE_OK && (r.type!=ZREC_FREELIST)) rc = SQLITE_IOERR_READ;
    if(rc==SQLITE_OK){
      nf=r.nPayload; c->freeRecBytes=ZREC_TOTAL(nf); fb=sqlite3_malloc64(nf?nf:1);
      if(!fb) rc=SQLITE_NOMEM;
      else rc = c->io.xRead(c->io.ctx, fb, nf, c->hdr.freeOff+ZVFS_REC_HDR_SIZE);
      if(rc==SQLITE_OK && r.crc!=zcrc32(0,fb,nf)) rc=SQLITE_IOERR_READ;
    }
  }
  if(rc==SQLITE_OK && c->hdr.pendOff){
    rc = zctr_read_rechdr(&c->io, c->hdr.pendOff, &r);
    if(rc==SQLITE_OK && (r.type!=ZREC_PENDING)) rc = SQLITE_IOERR_READ;
    if(rc==SQLITE_OK){
      np=r.nPayload; c->pendRecBytes=ZREC_TOTAL(np); pb=sqlite3_malloc64(np?np:1);
      if(!pb) rc=SQLITE_NOMEM;
      else rc = c->io.xRead(c->io.ctx, pb, np, c->hdr.pendOff+ZVFS_REC_HDR_SIZE);
      if(rc==SQLITE_OK && r.crc!=zcrc32(0,pb,np)) rc=SQLITE_IOERR_READ;
    }
  }
  if(rc==SQLITE_OK) rc = zalloc_load(c->alloc, fb, nf, pb, np, c->hdr.eof);
  sqlite3_free(fb); sqlite3_free(pb);
  if(rc==SQLITE_OK) c->allocLoaded=1;
  return rc;
}

/* Per-generation caches that go stale the instant c->hdr is replaced or
   reset: the page-1 cache and the pageSize-sized scratch buffers. Freed
   (not merely marked invalid) because a page-size change across
   generations would otherwise leave a stale-sized buffer as a
   heap-overflow hazard the next time it's used. Shared by zctr_revalidate
   and zctr_sync_abort so this list of fields lives in exactly one place. */
static void zctr_drop_buffers(ZvfsContainer *c){
  sqlite3_free(c->pg1); c->pg1 = NULL;
  sqlite3_free(c->pgbuf); c->pgbuf = NULL;
  sqlite3_free(c->paybuf); c->paybuf = NULL;
}

/* Ensure both c->pgbuf and c->paybuf are allocated at c->pageSize, without
** touching whichever one is already there. Shared by zctr_write/zctr_read
** (both need pgbuf as decompress destination, paybuf as compress source)
** and compact.c's zcompact_step (paybuf only, as read/rewrite scratch for
** page relocation).
**
** Fix round 3 (found via a genuine SQLite memsubsys2.test regression --
** residual unfreed memory): an earlier version of this code let each call
** site allocate independently, each gated on a DIFFERENT single field --
** container.c's own sites checked "is pgbuf null" and (re)allocated BOTH
** unconditionally; compact.c's checked "is paybuf null" and allocated
** ONLY paybuf. Whichever ran first (commonly zcompact_step, since Task
** 15's OVERWRITE rebuild commit unconditionally drops both before its own
** pack loop -- see zctr_sync_rebuild) could leave paybuf allocated alone
** with pgbuf still null; the next ordinary zctr_write/zctr_read call then
** saw pgbuf still null and allocated BOTH again, unconditionally
** overwriting -- leaking -- the paybuf compact.c had just set up. This bug
** existed since Task 14 introduced compact.c's own allocation (a
** truncate-only commit can legitimately reach compaction with neither
** buffer ever allocated), but the round-3 drop-buffers timing fix is what
** turned "rare interleaving" into "the normal case for every VACUUM,"
** finally surfacing it. One allocation helper, one guard per field,
** called from all three sites, makes the leak structurally impossible
** rather than newly-rare again. */
int zctr_ensure_scratch(ZvfsContainer *c){
  if(!c->pgbuf){ c->pgbuf = sqlite3_malloc64(c->pageSize); if(!c->pgbuf) return SQLITE_NOMEM; }
  if(!c->paybuf){ c->paybuf = sqlite3_malloc64(c->pageSize); if(!c->paybuf) return SQLITE_NOMEM; }
  return SQLITE_OK;
}

/* Reset all container state to a freshly-created, nothing-committed-yet
   allocator/map -- equivalent to zctr_create's initial state. Used whenever
   the on-disk file has gone to zero length: there is no persisted
   eof/free/pending list left to lazily load from, so the allocator is
   rebuilt from scratch (zalloc_new()) rather than left to "reload lazily"
   from a hdr with eof==0, which would let the next allocation land inside
   the reserved header block. Shared by zctr_revalidate and
   zctr_sync_abort. */
static void zctr_reset_empty(ZvfsContainer *c){
  memset(&c->hdr, 0, sizeof(c->hdr));
  c->whichHdr = -1;
  c->pageSize = 0;
  c->stagedCount = 0;
  zmap_reset(c->map, 0, 0);
  zalloc_delete(c->alloc);
  c->alloc = zalloc_new();
  c->allocLoaded = 1;
}

/* Failure-safe reset for zctr_sync: invoked on every error return once
   c->alloc/c->map already hold mutations staged toward the txn being
   committed (from zmap_commit onward -- see the goto sites below), so a
   caller that retries zctr_sync after a mid-protocol I/O failure can't
   replay the same non-idempotent free/take sequence against state that no
   longer matches what's actually on disk (double-freeing the old
   list-record extents, aliasing extents zmap_commit already reused, etc).
   Contract: a failed sync resets our in-memory bookkeeping to whatever
   generation is actually committed on disk; SQLite's own contract after a
   failed db sync is that the caller (pager) rolls back / replays every
   modified page through fresh xWrite calls, so logical page content
   converges to the committed state regardless of what we do here. */
static void zctr_sync_abort(ZvfsContainer *c){
  u8 buf[1024];
  ZvfsHdr hdr; int which;
  int rc = c->io.xRead(c->io.ctx, buf, sizeof(buf), 0);
  if(rc==SQLITE_OK) rc = zhdr_pick(buf, buf+ZVFS_HDR_COPY_SIZE, &hdr, &which);
  if(rc==SQLITE_OK){
    /* Covers failure-after-header-write: the new header may already be
       durable even though a later step (e.g. the flip barrier) failed. */
    c->hdr = hdr; c->whichHdr = which; c->pageSize = hdr.pageSize; c->stagedCount = hdr.pageCount;
    zmap_reset(c->map, c->hdr.mapRoot, c->hdr.pageCount);
    c->allocLoaded = 0;                 /* reloads from the adopted header */
    /* Task 16 fix (coordinator-ruled, sub-case A): a real (non-crash) I/O
       failure on specifically the SECOND write of a torn-flip conversion
       commit (commit_once's c->convert branch: copy B lands+syncs, then
       copy A's own write/sync fails for real, not just a simulated crash)
       reaches exactly this branch -- zhdr_pick finds copy B durably valid
       and adopts it (c->whichHdr flips away from -1). c->convert MUST
       clear here: leaving it set would make THIS SAME container's very
       next commit re-enter the convert branch and write to copy B FIRST
       AGAIN -- but B is now the ONLY valid copy on disk (copy A never
       became one), so a crash/failure during that second B-first write
       would leave NEITHER copy valid, unlike the safe case this guard
       exists for (a genuinely virgin container with no durable copy at
       all yet). Once ANY header has been durably adopted, the ordinary
       A/B alternation (commit_once's non-convert branch) is correct and
       safe on its own -- see also commit_once's own c->whichHdr==-1 gate
       on the convert branch, a second, structural line of defense for the
       same invariant. */
    c->convert = 0;
  }else{
    u64 sz = 0;
    int rc2 = c->io.xFileSize(c->io.ctx, &sz);
    if((rc2==SQLITE_OK && sz==0) || c->whichHdr==-1){
      /* Either the file is genuinely empty, or this is a virgin container
         (zctr_create, whichHdr==-1, never committed a header -- hdr.eof==0)
         whose FIRST-ever sync failed after its page/node records were
         already durable but before any header copy was ever written: the
         file can be nonzero even though there is no committed header to
         fall back to. Falling into the "keep the previous hdr" branch
         below would pair a zeroed hdr.eof==0 with allocLoaded=0 (lazy
         reload); the next zctr_load_alloc would call zalloc_load(...,
         eof=0), and the next allocation would return offset 0 -- inside
         the reserved header block, corrupted by the next record write.
         Route through the same from-scratch reset the zero-length case
         uses instead. */
      zctr_reset_empty(c);
    }else{
      /* A previously committed container (whichHdr>=0) whose header block
         is now unreadable (genuine corruption / a transient IO failure on
         the header read itself): don't invent state, keep the last known
         committed generation. */
      sqlite3_log(SQLITE_IOERR_READ,
        "zstdvfs: sync abort could not read a valid header; keeping prior committed state");
      zmap_reset(c->map, c->hdr.mapRoot, c->hdr.pageCount);
      c->stagedCount = c->hdr.pageCount;  /* undo this txn's staged advance */
      c->allocLoaded = 0;
    }
  }
  zctr_drop_buffers(c);
  c->dirty = 0;
}

/* Task 15: the commit body factored out of zctr_sync so the ordinary
** commit path, the OVERWRITE rebuild's own commit, and zcompact_full's pack
** passes (compact.c) all ride the identical crash-safe protocol -- one
** implementation, three callers. Declared non-static (zvfs_int.h) purely so
** compact.c can reach it, the same "internal-shared, not a public API"
** convention zcompact_step already established in Task 14. */
int commit_once(ZvfsContainer *c, int gateOk){
  u64 txn = c->hdr.txn + 1;
  int rc = zctr_load_alloc(c);                     /* no-op if loaded */
  if(rc) return rc;
  /* Task 18 carried-in mandatory item (controller rulings, Tasks 14/17):
  ** pending-generation ceiling. gateOk was computed by the caller BEFORE
  ** entering this function; under a long-lived overlapping WAL reader it
  ** can come in 0 for many commits in a row, and each such commit's own
  ** frees land in a NEW, never-released generation (see ZVFS_PENDING_CAP's
  ** own comment, zvfs_int.h, for why gen_insert's within-generation
  ** coalescing does not bound this). Once the pending payload already on
  ** the books (from PRIOR blocked commits -- this commit's own frees
  ** haven't been staged yet at this point) crosses that cap, don't just
  ** trust the caller's possibly-stale gateOk=0: force one non-blocking
  ** re-probe of the real gate. The condition the caller observed (e.g. an
  ** overlapping reader's snapshot) may have already cleared by the time
  ** this specific commit runs -- re-probing costs one shm try-lock sweep,
  ** paid only once the cap is already crossed, not on every commit.
  **
  ** If the re-probe still finds the gate shut, this is the ceiling's
  ** actual "safe degradation" decision: keep going exactly as gateOk=0
  ** already behaves below (the commit itself never fails for this reason;
  ** recycling/truncation simply stay deferred -- "prefer failing the
  ** RECYCLE, keep pending"), but make the situation visible via
  ** sqlite3_log rather than growing silently. A hard cap that forced
  ** recycling/truncation here regardless of the reader would be able to
  ** free bytes an active reader's root still references -- exactly the
  ** corruption §6.2's gate exists to prevent -- so this is deliberately a
  ** SOFT ceiling: unbounded growth remains possible in principle (an
  ** adversarial, permanently-open reader), but it is never silent past
  ** this point, and every commit past the cap keeps trying to clear it. */
  if(!gateOk && zalloc_ser_pend_size(c->alloc) >= ZVFS_PENDING_CAP){
    if(c->gateProbe) gateOk = c->gateProbe(c->gateProbeCtx);
    if(!gateOk){
      sqlite3_log(SQLITE_WARNING,
        "zstdvfs: WAL reader gate still closed with %u bytes of pending-free "
        "payload (cap %u) -- deferring extent reclaim until a quiet moment",
        zalloc_ser_pend_size(c->alloc), (unsigned)ZVFS_PENDING_CAP);
    }
  }
  /* 0: release PRIOR committed generations (txn <= c->hdr.txn) before
  ** touching anything else this commit. Safe unconditionally: c->hdr is
  ** the durable, already-flipped-to header on disk; every generation with
  ** txn <= c->hdr.txn was superseded by a commit that already landed, so
  ** by construction no root (durable or in-flight) can still reference
  ** those extents -- that's the release-gating argument, applied here to
  ** generations that are provably closed rather than to the one still
  ** being built. This is what bounds zcompact_step's/zalloc_trim's work
  ** below: it exposes the tail *previous* commits vacated, in time for
  ** THIS commit's trim/compact/truncate to reclaim it -- the one-commit
  ** lag the brief describes ("trimmed by a later commit") is intentional
  ** and converges precisely because every commit performs this release at
  ** its own start. Contrast with THIS txn's own frees (below): those stay
  ** pending until strictly after the second xSync at the bottom of this
  ** function -- see the comment there for why releasing them any earlier
  ** is a crash-safety bug, not an optimization. */
  /* Item 1: measure what THIS release call itself makes newly available --
  ** see ZvfsContainer.bytesReleasedThisCommit's own comment (zvfs_int.h)
  ** for why compact.c's byte-budget quota scales with this. Captured
  ** tightly around the release call, before zalloc_trim (which also
  ** changes freeBytes, by retracting eof over already-free extents --
  ** that's space ceasing to exist, not newly becoming available, so it
  ** must not count here). */
  u64 freeBytesBeforeRelease = zalloc_free_bytes(c->alloc);
  if(gateOk) zalloc_release(c->alloc, c->hdr.txn);
  {
    u64 freeBytesAfterRelease = zalloc_free_bytes(c->alloc);
    c->bytesReleasedThisCommit = freeBytesAfterRelease > freeBytesBeforeRelease
      ? freeBytesAfterRelease - freeBytesBeforeRelease : 0;
  }
  /* trim: pop trailing free extent(s) abutting eof, lowering eof over them,
     off THAT release -- i.e. only extents PRIOR commits released, never
     THIS txn's own pending frees below (see zalloc_trim's comment for the
     general safety argument; see step 0's comment above for why that
     boundary is drawn here specifically). Run before compaction gets a
     chance to consume any of that same space itself. */
  zalloc_trim(c->alloc);
  /* 1: pack a small quota of tail records into lower free gaps before this
     txn's own frees/COW run, so the space they vacate is itself a candidate
     for the truncate check below. Skipped for OVERWRITE-mode rebuild (Task
     15): a rebuild already rewrites the whole container densely from
     scratch, so there is nothing fragmented for it to pack. */
  if(!c->rebuild){
    rc = zcompact_step(c, txn);
    if(rc) goto abort;
  }
  /* 2: old list records become garbage at flip. Skipped for the OVERWRITE
  ** rebuild's own commit (Task 15, same c->rebuild gate as step 1's
  ** zcompact_step skip above): zalloc_reset_span already staged the
  ** ENTIRE old generation -- including wherever these two records
  ** physically lived, both offsets necessarily < the old eof -- as one
  ** pending generation in one shot. Freeing them again here would
  ** double-account the same bytes: gen[] would gain a second, overlapping
  ** entry for space already pending release under this same commit's own
  ** txn, corrupting the sorted/coalesced invariant gen_insert otherwise
  ** guarantees. */
  if(!c->rebuild){
    if(c->hdr.freeOff){ zalloc_free(c->alloc, c->hdr.freeOff, c->freeRecBytes, txn); }
    if(c->hdr.pendOff){ zalloc_free(c->alloc, c->hdr.pendOff, c->pendRecBytes, txn); }
  }
  /* 3: COW the map (frees old node extents into txn) */
  u64 newRoot;
  rc = zmap_commit(c->map, c->alloc, txn, c->stagedCount, &newRoot);
  if(rc) goto abort;
  /* 4/5: place FREELIST/PENDING records by best-fit TAKE from fr[] instead
  ** of unconditional append -- this is the piece that actually lets eof
  ** shrink: as long as fr[] has room, these records ride inside space
  ** step 0 already proved safe to reuse (prior, already-durable
  ** generations only), so no fresh eof growth is needed for them at all;
  ** zalloc_take's own extend fallback preserves today's behavior on a
  ** genuinely dense container.
  **
  ** Self-referential-sizing, resolved without iterating to a fixed point:
  ** compute an UPPER-BOUND payload size for each list (current serialized
  ** size + a fixed slack) BEFORE taking anything, take exactly that many
  ** (granule-rounded) bytes, THEN serialize the ACTUAL post-take state
  ** into a zero-padded buffer of exactly that many bytes. This is always
  ** safe because taking only ever shrinks fr[] (removes or shrinks one
  ** entry) or, via the rare extend fallback crossing the lock hole, grows
  ** it by at most one entry (12 bytes) total across both takes (the first
  ** crossing permanently moves eof past the hole, so at most one of the
  ** two takes can ever need to cross it) -- so the actual post-take size
  ** can never exceed the pre-take size + slack. 24 bytes of slack (two
  ** free-list entries' worth) comfortably covers that one-entry worst
  ** case with margin. nPayload is recorded as the padded (reserved) size,
  ** not the real content size, so the physical extent (ZREC_TOTAL of what
  ** we took) and the record's own declared size always agree -- required
  ** for the compactor's forward tail-walk (ZREC_TOTAL(r.nPayload)) to
  ** stay consistent, and for zalloc_load's terminator-based parsing
  ** (below) to know how far the padding extends. CRC covers the full
  ** padded buffer, matching what the reader re-reads and re-hashes. */
  u32 s1cap = zalloc_ser_free_size(c->alloc) + 24;
  u32 s2cap = zalloc_ser_pend_size(c->alloc) + 24;
  u64 offF = zalloc_take(c->alloc, ZREC_TOTAL(s1cap));
  u64 offP = zalloc_take(c->alloc, ZREC_TOTAL(s2cap));
  {
    u8 *b = sqlite3_malloc64(s1cap);
    if(!b){ rc = SQLITE_NOMEM; goto abort; }
    memset(b, 0, s1cap);
    zalloc_ser_free(c->alloc, b);
    ZvfsRec r = {.type=ZREC_FREELIST,.flags=0,.nPayload=s1cap,.key=0,.crc=zcrc32(0,b,s1cap)};
    rc = zctr_write_record(&c->io, offF, &r, b);
    sqlite3_free(b);
    if(rc) goto abort;
    c->freeRecBytes = ZREC_TOTAL(s1cap);
  }
  {
    u8 *b = sqlite3_malloc64(s2cap);
    if(!b){ rc = SQLITE_NOMEM; goto abort; }
    memset(b, 0, s2cap);
    zalloc_ser_pend(c->alloc, b);
    ZvfsRec r = {.type=ZREC_PENDING,.flags=0,.nPayload=s2cap,.key=0,.crc=zcrc32(0,b,s2cap)};
    rc = zctr_write_record(&c->io, offP, &r, b);
    sqlite3_free(b);
    if(rc) goto abort;
    c->pendRecBytes = ZREC_TOTAL(s2cap);
  }
  /* physical shrink: if compaction+trim dropped the logical eof below the
     file's actual physical size, reclaim that tail now, before the data
     barrier -- a crash between here and the header flip leaves the file
     physically shorter than what the OLD (still-current, not-yet-
     superseded) committed header describes, but that's safe by the same
     prior-generations-only argument as step 0's release/trim above:
     nothing this truncate removes was reachable from that old header in
     the first place, because it was never anything but a PRIOR commit's
     already-superseded, already-released extent. */
  u64 finalEof = zalloc_eof(c->alloc);
  u64 physSize;
  rc = c->io.xFileSize(c->io.ctx, &physSize);
  if(rc) goto abort;
  if(finalEof < physSize){
    rc = c->io.xTruncate(c->io.ctx, finalEof);
    if(rc) goto abort;
  }
  /* 6: data barrier, then header flip, then flip barrier */
  rc = c->io.xSync(c->io.ctx);
  if(rc) goto abort;
  ZvfsHdr nh = { .pageSize=c->pageSize, .pageCount=c->stagedCount, .txn=txn,
                 .mapRoot=newRoot, .freeOff=offF, .pendOff=offP,
                 .eof=zalloc_eof(c->alloc), .flags=0 };
  u8 hb[ZVFS_HDR_COPY_SIZE];
  zhdr_encode(&nh, hb);
  int which;
  /* Structural safety net (coordinator-ruled, belt-and-suspenders for the
  ** c->convert==0-on-adoption fix in zctr_sync_abort above): only take the
  ** torn-flip-unsafe B-first branch when this container has genuinely
  ** never adopted ANY header yet (c->whichHdr==-1). c->convert alone is
  ** the flag that SHOULD always agree with that, but this makes it
  ** impossible for a future bug -- anywhere that fails to clear
  ** c->convert once a header has been adopted -- to re-trigger the exact
  ** hazard the flag exists to prevent (re-overwriting the only currently-
  ** valid header copy with no fallback), rather than merely making that
  ** bug rare again. Once any header is adopted, ordinary A/B alternation
  ** is unconditionally correct on its own, convert flag or not. */
  if(c->convert && c->whichHdr==-1){
    /* Torn-flip-safe conversion commit (spec Sec3/Sec7.5 amendment, Task
    ** 16): this specific commit overwrites the PLAIN database's own bytes
    ** at offset 0 for the first time -- an ordinary single-copy header
    ** write here (as below) could be torn, leaving the file neither a
    ** valid plain db nor a valid container. Write copy B (offset 512)
    ** FIRST and sync, THEN copy A (offset 0) and sync (below), rather than
    ** the ordinary alternating single-copy write. Crash matrix (verified
    ** against detect_mode's container-first zhdr_pick probe, vfs_shim.c):
    **   - pre-B / torn-B: A (offset 0-511) still holds the untouched
    **     original plain page-1 prefix -- zhdr_pick finds neither copy
    **     valid, detect_mode's plain-magic fallback finds "SQLite format
    **     3" intact, and SQLite's own hot rollback journal (already
    **     written+synced before VACUUM's destructive copy-back began, per
    **     ordinary journaled-write-before-first-touch discipline) replays
    **     every page it dirtied -- including page 1 -- restoring a fully
    **     valid plain database.
    **   - B-valid + pre-A / torn-A: zhdr_pick finds the valid B copy (the
    **     new generation; nothing else about this commit -- the whole
    **     rebuild stream's own records, the COW map, the list records --
    **     is torn, only this redundant second header copy is) ->
    **     detect_mode reports CONTAINER. SQLite still sees its own VACUUM
    **     transaction as never having committed (the hot journal is still
    **     present -- its deletion is a later pager step than this sync)
    **     and replays it: through the shim, now in CONTAINER mode, each
    **     journaled original page becomes an ordinary zctr_write against
    **     the just-flipped container -- yielding a valid container holding
    **     the pre-VACUUM logical content, exactly rollback's contract. */
    rc = c->io.xWrite(c->io.ctx, hb, ZVFS_HDR_COPY_SIZE, ZVFS_HDR_COPY_SIZE);
    if(rc) goto abort;
    rc = c->io.xSync(c->io.ctx);
    if(rc) goto abort;
    which = 0;   /* copy A -- "The whichHdr after conversion = A(0)." */
  }else{
    which = (c->whichHdr==0) ? 1 : 0;
  }
  rc = c->io.xWrite(c->io.ctx, hb, ZVFS_HDR_COPY_SIZE,
                    which ? ZVFS_HDR_COPY_SIZE : 0);
  if(rc) goto abort;
  rc = c->io.xSync(c->io.ctx);
  if(rc) goto abort;
  /* 7: committed. */
  c->hdr = nh; c->whichHdr = which; c->dirty = 0; c->convert = 0;
  /* Only NOW may THIS txn's own pending generation (steps 1-3's frees)
  ** become releasable -- strictly after the second xSync above, i.e.
  ** strictly after the header flip that makes it durable. Releasing it
  ** any earlier (as an intermediate version of this fix once did, mid-
  ** function, to let zalloc_trim/the physical-truncate check above see
  ** it) is a real crash-safety bug, not just a missed optimization: the
  ** OLD header (still durable up to this exact point, and the ONLY
  ** committed state a crash before this line can fall back to) still
  ** names c->hdr.freeOff/pendOff and the map's old node/page extents at
  ** their OLD offsets. If this generation's extents were folded into
  ** fr[] before the flip lands, a same-commit zalloc_take (list-record
  ** placement, a relocated PAGE/NODE copy from zcompact_step, or the
  ** map's own COW) could hand one of those OLD offsets right back out
  ** and physically overwrite it -- while the old header a crash would
  ** revalidate to is still pointing at it. Worse, a same-offset overwrite
  ** (e.g. a FREELIST record landing exactly where the old one was) still
  ** passes the old header's own CRC/txn checks on reload: a stale-but-
  ** structurally-valid free list, silently aliasing live extents the old
  ** generation still needs. Step 0's release of PRIOR generations is
  ** exempt from this hazard specifically because THEIR header already
  ** flipped past durably -- nothing can ever fall back to it again -- but
  ** this generation's header has not yet, right up until the line above.
  ** gateOk itself only proves no *external reader* still needs this
  ** generation's superseded content (see step 0's comment); it says
  ** nothing about our OWN not-yet-durable in-progress commit, which is
  ** exactly the case this ordering protects. (Separately: under gateOk=0 --
  ** live as of Task 18's WAL checkpoint gate (vfs_shim.c's zvGateOk) -- this
  ** generation's own extents simply never get released here (zalloc_free's
  ** gen_insert keeps its pending array coalesced, so the accumulation stays
  ** compact, but stays PENDING, unusable and unreleased) until a later
  ** commit's gateOk finally goes true; see this function's own top-of-body
  ** comment for the ceiling that bounds how long that can go unnoticed.) */
  if(gateOk) zalloc_release(c->alloc, txn);
  return SQLITE_OK;

abort:
  zctr_sync_abort(c);
  return rc;
}

/* Task 15 (SQLITE_FCNTL_OVERWRITE full rebuild, Milestone 3 -- VACUUM),
** REDESIGNED in Task 19 (coordinator ruling, "REBUILD ACCEPTANCE v3") after
** the original strict-sequential acceptance model was found to silently
** degrade every VACUUM on a database large enough to exceed SQLite's own
** pager cache: SQLite's backup engine (sqlite3.c's backupOnePage, what
** VACUUM's copy-back uses) writes into the DESTINATION PAGER's cache, not
** directly to the file -- the order those dirty pages actually reach our
** xWrite is whatever the pager's own spill policy picks, not the order
** backupOnePage was called in. For a multi-GB VACUUM (test/bigsmoke.sh,
** the first workload in this project large enough to exceed the pager's
** default cache) this reliably means page 2 or later physically arrives
** before page 1 -- confirmed directly (lldb/instrumented repro: the
** rebuild's own zero-progress guard, which required page 1 first, aborted
** on literally the very first accepted write of a 3 GiB VACUUM, silently
** falling back to non-dense recompression).
**
** v3 accepts pages in ANY arrival order, staged into newMap keyed by
** position rather than policed by a watermark -- there is no more
** "sequential continuation" concept to violate, so a genuine rollback
** replay's own writes (indistinguishable from forward-copy writes once
** ordering is no longer assumed) are simply accepted the same way instead
** of being detected and specially handled. Safety instead comes entirely
** from a completeness gate at commit time (zctr_sync_rebuild): every
** expected page must be present, AND chunk 0's own current content must
** still agree with the page size learned when it first arrived (catches a
** rollback replay that re-lands on chunk 0 with ORIGINAL content after the
** forward copy already finished -- every chunk still "present," but no
** longer a consistent generation), before the staged generation is allowed
** to become durable. Either failure -> discard the staged attempt and FAIL
** the sync with a real error (CORRECTED after controller review: an
** earlier version of this gate fell back to the still-durable prior
** generation and reported SQLITE_OK, reasoning "VACUUM never changes
** logical content" made that always safe -- wrong, because by the time
** this sync call would have returned OK, SQLite already believes the
** rebuild succeeded and has moved its own in-memory state -- pager cache,
** BTS_PAGESIZE_FIXED, schema cookie -- to match the NEW layout; silently
** serving OLD-layout content back is a real divergence, not a no-op, and
** there is no way to tell "SQLite is itself about to roll back anyway"
** apart from every other incompleteness cause at this point). Failing the
** sync instead makes SQLite itself roll back: the ensuing journal replay
** writes original content over identical still-durable content (a true
** no-op, since discard-on-failure never touched anything the durable
** header references), so the VACUUM is reported as failed rather than
** silently not taking effect. See zctr_sync's own comment for where this
** gate is applied and the full corrected argument.
**
** chunkAmt/finalPageSize exist because of a wrinkle the original design's
** own pseudocode doesn't fully cover: backupOnePage chunks EVERY write to
** the main-db handle at the DESTINATION's own CURRENT (still-fixed) page
** size -- never the new one -- for the entire copy, only clearing the
** "page size fixed" flag after the whole rebuild commits. So on an
** ordinary (unchanged page size) VACUUM, chunkAmt==the one true final page
** size throughout, and no reassembly is needed -- the chunk-keyed map IS
** the final pgno-keyed map. On a page-size-CHANGING VACUUM (test_vacuum.c
** exercises this: 4096->8192 and, with PENDING_BYTE lowered, 4096<->1024
** across the locking-page boundary) chunkAmt stays at the OLD, different
** size for the whole stream; finalPageSize is learned separately, from
** chunk 0's own content bytes (chunk 0 always covers destination byte
** offset 0, i.e. page 1's own header, regardless of arrival order -- "page
** 1 geometry learning," just decoupled from being the first arrival). The
** two sizes may then differ, in which case the commit reassembles: walk
** chunks 0..count-1 in ORDER (not arrival order -- by now every one is
** known to be present, per the completeness gate), concatenate their
** decompressed bytes, and re-chunk into real finalPageSize-sized pages.
** See rebuild_reassemble (below zctr_sync_rebuild). */
struct ZvfsRebuild {
  ZvfsMap *newMap;           /* chunk-keyed during the stream; see above */
  u32 chunkAmt;               /* the stream's fixed OLD/current page size; 0 until learned */
  u32 finalPageSize;           /* the NEW page size, learned from chunk 0's own content; 0 until chunk 0 arrives */
  u32 maxChunkSeen;             /* highest chunk index (0-based) ever placed -- completeness-gate fallback when no explicit truncate arrived; meaningless unless anyPlaced */
  int anyPlaced;                 /* has ANY chunk ever been placed? (chunk 0 is a valid real index, so maxChunkSeen==0 alone can't distinguish "only chunk 0 seen" from "nothing yet") */
  int haveCount;                 /* an explicit zctr_truncate(while rebuild) arrived */
  u64 countBytes;                 /* that truncate's raw logical byte size (page-size-agnostic; divided by chunkAmt/finalPageSize as needed at commit) */
  u8 *scratch;                     /* 65536-byte fixed: sub-page-patch/reassembly decode buffer */
  u8 *paybuf;                       /* 65536-byte fixed: compress-destination scratch */
  /* Task 16: the boundary zctr_sync_rebuild's own commit describes as
  ** "the entire superseded generation, now free space" via
  ** zalloc_reset_span(alloc, ZVFS_HDR_BLOCK_SIZE, reclaimBase, ...). For an
  ** ordinary OVERWRITE rebuild on an already-converted container this is
  ** just c->hdr.eof (the real previous generation's own boundary, set
  ** below); zctr_create_for_convert overrides it to plainSize's own
  ** rounded boundary instead -- the point past which the append-only
  ** conversion stream started -- so that once the conversion commits, the
  ** plain database's own now-superseded physical footprint
  ** [ZVFS_HDR_BLOCK_SIZE, reclaimBase) becomes ordinary reclaimable free
  ** space for the pack loop (zcompact_full) to actually shrink the file
  ** back into, instead of being permanently stranded (c->hdr.eof is 0 for
  ** a virgin conversion container, which would otherwise reclaim nothing
  ** at all and only ever grow the file). */
  u64 reclaimBase;
};

int zctr_begin_overwrite(ZvfsContainer *c){
  int rc = zctr_load_alloc(c);      /* need c->alloc live before appendonly */
  if(rc) return rc;
  struct ZvfsRebuild *rb = sqlite3_malloc64(sizeof(*rb));
  if(!rb) return SQLITE_NOMEM;
  memset(rb, 0, sizeof(*rb));
  rb->newMap = zmap_new(&c->io);
  zmap_reset(rb->newMap, 0, 0);
  rb->reclaimBase = c->hdr.eof;
  /* v3 (Task 19): chunkAmt is the pager's fixed page size for this backup's
  ** whole duration -- already known in advance for an ordinary OVERWRITE on
  ** an already-converted container (c->pageSize, unchanged until this
  ** rebuild's own commit reassigns it), so placement/reads are order-
  ** independent from the very first write. For a virgin plain-db conversion
  ** (zctr_create_for_convert, c->pageSize still 0) there is no prior
  ** geometry to know in advance -- chunkAmt stays 0, learned from whichever
  ** write arrives first (zctr_rebuild_write's own bootstrap), same
  ** amt-is-the-page-size reasoning zctr_write's own virgin-container path
  ** already uses. scratch/paybuf are sized to the max valid page shape
  ** (65536) unconditionally, so no resize bookkeeping is ever needed
  ** regardless of which page size(s) this stream turns out to use. */
  rb->chunkAmt = c->pageSize;
  rb->scratch = sqlite3_malloc64(65536);
  rb->paybuf = sqlite3_malloc64(65536);
  if(!rb->scratch || !rb->paybuf){
    sqlite3_free(rb->scratch); sqlite3_free(rb->paybuf);
    zmap_delete(rb->newMap); sqlite3_free(rb);
    return SQLITE_NOMEM;
  }
  c->pRb = rb;
  c->rebuild = 1;
  /* Deliberately NOT forcing c->dirty=1 here (an earlier version of this
  ** function did): c->dirty also gates vfs_shim.c's zvUnlock "commit if
  ** dirty" hook (synchronous=OFF support), which fires on ANY write-lock
  ** release, commit or abort alike. If OVERWRITE fires but the copy fails
  ** before a single real page ever arrives (rare but real -- e.g. OOM
  ** reading the very first source page, well before backupOnePage's first
  ** call) and the transaction then rolls back, that hook must see
  ** c->dirty still false and do nothing, leaving the untouched committed
  ** database exactly as it was. Forcing it true here would instead run
  ** the full rebuild commit with an empty, never-populated pRb->newMap --
  ** silently wiping the database. Set for real (mirroring ordinary
  ** zctr_write) once zctr_rebuild_write actually accepts a page into the
  ** stream. */
  zalloc_set_appendonly(c->alloc, 1);
  return SQLITE_OK;
}

/* Task 16 (plain-database conversion via VACUUM). Builds a fresh container
** (zctr_create's own virgin state: whichHdr==-1, nothing committed) and
** immediately drives it into the same rebuild mode zctr_begin_overwrite
** establishes for an already-converted container's own OVERWRITE, EXCEPT
** the allocator's eof starts pre-advanced past the ENTIRE plain database
** being converted, not at the ordinary ZVFS_HDR_BLOCK_SIZE zalloc_new()
** starts from -- zalloc_reset_span with freeFrom==freeTo==startEof sets
** eof to startEof with an empty free span (nothing below it is ever
** reusable), so every record the append-only rebuild stream places lands
** strictly above every byte of the plain database, which therefore stays
** completely untouched -- still readable via ordinary passthrough
** delegation -- until this container's own commit (commit_once's
** convert-gated B-then-A header write) makes it durable. Also overrides
** rb->reclaimBase (see struct ZvfsRebuild's own comment) to startEof: once
** that commit lands, the plain database's own now-superseded physical
** footprint [ZVFS_HDR_BLOCK_SIZE, startEof) becomes ordinary reclaimable
** free space for the pack loop to shrink the file back into -- without
** this override, zctr_begin_overwrite's own default (c->hdr.eof, which is
** 0 for this virgin container) would reclaim nothing at all and the file
** would only ever grow. */
int zctr_create_for_convert(ZvfsContainer **pOut, ZvfsIO io, u64 plainSize){
  int rc = zctr_create(pOut, io);
  if(rc) return rc;
  ZvfsContainer *c = *pOut;
  rc = zctr_begin_overwrite(c);
  if(rc){ zctr_close(c); *pOut = 0; return rc; }
  u64 startEof = plainSize > ZVFS_HDR_BLOCK_SIZE ? plainSize : ZVFS_HDR_BLOCK_SIZE;
  startEof = zvfs_gran_round64(startEof);
  /* freeFrom==freeTo here (an empty span, nothing to free) -- the txn
     argument is never actually read on this call (see zalloc_reset_span's
     own comment); 0 documents "not applicable" rather than implying a
     real generation tag. */
  zalloc_reset_span(c->alloc, startEof, startEof, startEof, 0);
  c->pRb->reclaimBase = startEof;
  c->convert = 1;
  return SQLITE_OK;
}

/* Discard any staged OVERWRITE rebuild wholesale and fall back to ordinary
** write/commit handling: nothing below the old (still fully intact,
** still-committed) eof was ever touched by the append-only rebuild stream,
** so there is nothing to undo there -- just forget the staged generation
** and let the allocator reload fresh from the still-current committed
** header. Always safe to call, including when no rebuild is active
** (c->pRb==0) -- a no-op then, which is what makes this safe as an
** unconditional boundary check rather than something every caller must
** first prove is needed.
**
** Public (declared in zvfs_int.h, not just internal-shared): called from
** two kinds of places now (Task 19's "REBUILD ACCEPTANCE v3" removed the
** third, oldest kind -- a mid-write, mid-stream call from
** zctr_rebuild_write itself, back when writes were policed for sequential
** ordering; v3 accepts any geometrically well-formed write unconditionally,
** so zctr_rebuild_write never aborts on its own account any more) --
**   1. From zctr_sync's own completeness gate (rebuild_check_complete),
**      when a commit is attempted but the staged map is provably missing
**      at least one expected page -- see zctr_sync's own comment for the
**      full v3 safety argument (this subsumes the old, narrower
**      zero-progress-only guard: "nothing ever arrived" is just the
**      most extreme case of "not everything arrived").
**   2. THE STRUCTURAL FIX (fix round 4, controller-ruled): from
**      vfs_shim.c's zvUnlock, unconditionally, at every write-transaction
**      end (lock dropping from >=RESERVED toward <=SHARED), after the
**      existing commit-if-dirty step -- and from zvClose. The completeness
**      gate above only intervenes AT a commit attempt; it does nothing for
**      a stale rebuild that survives its own transaction on a connection
**      that gets reused without ever attempting a commit through this
**      container at all. A reviewer traced a concrete reactivation path
**      through exactly that gap (round 3's own report, pre-v3, had called
**      it "harmless" -- overturned by that trace): an abandoned rebuild's
**      own transaction ends (the VACUUM already committed for real via
**      xSync, or never will), the SAME connection starts a wholly
**      unrelated transaction, and if THAT transaction's first write
**      happens to target page 1 and completes it in one call (common in
**      rollback mode -- the page-1 change-counter bump is often the
**      lowest-numbered dirty page, and the pager flushes ascending), it
**      gets staged as chunk key 1 -- and under v3 that alone can be enough
**      to satisfy the completeness gate outright (see
**      test_abandoned_rebuild's third scenario, test_container.c, for the
**      full trace of why), letting the unrelated transaction's own commit
**      swap in that near-empty map and, via zalloc_reset_span, free the
**      entire actually-committed database. Enforcing "rebuild state cannot
**      outlive the transaction that started it" at the one place every
**      write transaction provably ends -- the lock drop below RESERVED,
**      whether via a successful unlock-commit or an ordinary rollback --
**      closes this structurally: no later, unrelated transaction can ever
**      observe c->rebuild!=0 from a previous one, so reactivation is
**      impossible by construction, independent of and without needing any
**      cooperation from the completeness gate above. */
int zctr_rebuild_abort(ZvfsContainer *c){
  struct ZvfsRebuild *rb = c->pRb;
  if(!rb) return SQLITE_OK;
  zmap_delete(rb->newMap);
  sqlite3_free(rb->scratch);
  sqlite3_free(rb->paybuf);
  sqlite3_free(rb);
  c->pRb = 0;
  c->rebuild = 0;
  zalloc_set_appendonly(c->alloc, 0);
  c->allocLoaded = 0;               /* reloads from the committed header */
  return SQLITE_OK;
}

/* Compress+place ONE already-complete chunkAmt-sized (during the stream) or
** finalPageSize-sized (during rebuild_reassemble's own re-chunking pass)
** page (pg) at the given 1-based key into the given map -- an explicit map
** parameter, not always rb->newMap, because rebuild_reassemble places into
** a FRESH map while consuming rb->newMap as its own read-side source. sz is
** the page's own size in bytes (chunkAmt or finalPageSize, whichever this
** call is placing). */
static int rebuild_place_page(ZvfsContainer *c, struct ZvfsRebuild *rb, const u8 *pg,
                               u32 sz, u32 key, ZvfsMap *map){
  u32 n; int raw;
  int rc = zcodec_compress(pg, sz, rb->paybuf, &n, ZVFS_LEVEL_REBUILD, &raw);
  if(rc) return rc;
  ZvfsRec r = { .type=ZREC_PAGE, .flags=raw?ZF_RAW:0, .nPayload=n, .key=key,
                .crc = raw ? zcrc32(0,rb->paybuf,n) : 0 };
  u32 nTotal = ZREC_TOTAL(n);
  u64 eoff = zalloc_take(c->alloc, nTotal);    /* append-only: pure extend */
  rc = zctr_write_record(&c->io, eoff, &r, rb->paybuf);
  if(rc){
    /* Task 19 fix (found via test_convert.c's stage-5 transient-fault
    ** sweep): a write that fails here (e.g. a real, transient I/O error --
    ** SQLite's own VACUUM/backup machinery can and does retry further
    ** pages on this SAME rebuild stream afterward, this container has no
    ** say over that) must not leave the extent zalloc_take just handed out
    ** as a permanent phantom gap -- consumed by the allocator's own
    ** bookkeeping but never a valid record, since nothing at all landed at
    ** eoff. Undo the take so a later retry of this same chunk (or any
    ** other placement) simply continues from where this one never
    ** happened; see zalloc_untake's own comment for the full hazard this
    ** avoids (a subsequent compaction pass trying to parse this gap as a
    ** record and failing for real). */
    zalloc_untake(c->alloc, eoff, nTotal);
    return rc;
  }
  ZvfsMapEntry e = { .off=eoff, .nPayload=n, .flags=r.flags };
  return zmap_set(map, key, &e);
}

/* Records that chunk index `chunkIdx` now has content (chunk 0 is a valid
** real index, so maxChunkSeen alone -- both start at 0 -- can't distinguish
** "only chunk 0 ever placed" from "nothing placed yet" without anyPlaced). */
static void rebuild_note_placed(struct ZvfsRebuild *rb, u32 chunkIdx){
  if(!rb->anyPlaced || chunkIdx > rb->maxChunkSeen) rb->maxChunkSeen = chunkIdx;
  rb->anyPlaced = 1;
}

/* v3 (Task 19, coordinator-ruled "REBUILD ACCEPTANCE v3" -- see the struct's
** own comment for the full motivation and safety argument). Accepts a write
** for ANY chunk position, in ANY arrival order: no watermark, no forward-
** gap/backward-jump classification, no rollback detection at all -- a
** rollback replay's own writes are simply chunk placements like any other,
** made safe entirely by the completeness gate at commit time
** (zctr_sync_rebuild/rebuild_check_complete) rather than by policing
** ordering here. Geometry/alignment validation is kept: amt must be a
** valid page-size shape, and either match the stream's own established
** chunkAmt (the common case -- direct placement) or be a smaller, in-chunk
** sub-page patch (sqlite3BtreeCopyFile's pgszSrc<pgszDest tail-patch,
** handled via read-modify-write instead of the old skip-region tracking --
** order-independent by construction, since it just operates on whatever
** the chunk currently holds, staged or not). */
static int zctr_rebuild_write(ZvfsContainer *c, const void *buf, int amt, i64 off){
  struct ZvfsRebuild *rb = c->pRb;
  const u8 *p = buf;
  if(amt<512 || amt>65536 || (amt&(amt-1))){
    sqlite3_log(SQLITE_IOERR_WRITE,
      "zstdvfs: bad rebuild write shape (amt=%d off=%lld)", amt, (long long)off);
    return SQLITE_IOERR_WRITE;
  }
  if(rb->chunkAmt==0){
    /* Bootstrap (plain-db conversion only -- an already-converted
    ** container's chunkAmt was pre-set from c->pageSize at
    ** zctr_begin_overwrite, so this branch never runs for it). Whichever
    ** write arrives first establishes chunkAmt, exactly like zctr_write's
    ** own virgin-container bootstrap ("amt IS the page size regardless of
    ** which page this happens to be") -- order-independent from here on. */
    if(off % (i64)amt != 0){
      sqlite3_log(SQLITE_IOERR_WRITE,
        "zstdvfs: non page-aligned rebuild write (amt=%d off=%lld)", amt, (long long)off);
      return SQLITE_IOERR_WRITE;
    }
    rb->chunkAmt = (u32)amt;
  }
  c->dirty = 1;
  if((u32)amt == rb->chunkAmt){
    if(off % (i64)rb->chunkAmt != 0){
      sqlite3_log(SQLITE_IOERR_WRITE,
        "zstdvfs: non page-aligned rebuild write (amt=%d off=%lld)", amt, (long long)off);
      return SQLITE_IOERR_WRITE;
    }
    u32 chunkIdx = (u32)((u64)off / rb->chunkAmt);
    if(chunkIdx==0 && rb->finalPageSize==0){
      /* Page-1 geometry learning, as before Task 19 -- just no longer
      ** required to happen on the very first call: the true NEW page size
      ** lives in chunk 0's own content (the canonical page-size header
      ** field), not in amt, which only ever carries the OLD/current size
      ** (see the struct's own comment) -- learned whenever chunk 0 happens
      ** to arrive. */
      u32 ps = ((u32)p[16]<<8) | p[17];
      if(ps==1) ps = 65536;
      if(ps<512 || ps>65536 || (ps&(ps-1))){
        sqlite3_log(SQLITE_IOERR_WRITE, "zstdvfs: bad page size (%u) in rebuild page 1 header", ps);
        return SQLITE_IOERR_WRITE;
      }
      rb->finalPageSize = ps;
    }
    int rc = rebuild_place_page(c, rb, p, rb->chunkAmt, chunkIdx+1, rb->newMap);
    if(rc) return rc;
    rebuild_note_placed(rb, chunkIdx);
    return SQLITE_OK;
  }
  if((u32)amt < rb->chunkAmt){
    u64 chunkStart = ((u64)off / rb->chunkAmt) * rb->chunkAmt;
    if(off + amt > (i64)(chunkStart + rb->chunkAmt)){
      sqlite3_log(SQLITE_IOERR_WRITE,
        "zstdvfs: rebuild sub-page write crosses a chunk boundary (amt=%d off=%lld chunkAmt=%u)",
        amt, (long long)off, rb->chunkAmt);
      return SQLITE_IOERR_WRITE;
    }
    u32 chunkIdx = (u32)(chunkStart / rb->chunkAmt);
    /* Base content: whatever this chunk already holds (a previous forward-
    ** copy write, or an earlier patch to the same chunk), else zero-fill --
    ** NOT a fall back to committed/old content: this chunk's own slot
    ** (the pending-byte page) is deliberately never touched by the
    ** ordinary per-page copy, so there is no "old" content for it to begin
    ** from either; SQLite never references it as real content any way. */
    ZvfsMapEntry e;
    int rc = zmap_get(rb->newMap, chunkIdx+1, &e);
    if(rc) return rc;
    if(e.off){
      ZvfsRec r;
      rc = zctr_read_record(&c->io, e.off, &r, rb->paybuf, rb->chunkAmt);
      if(rc) return rc;
      if((r.flags & ZF_RAW) && r.crc != zcrc32(0, rb->paybuf, r.nPayload)){
        sqlite3_log(SQLITE_IOERR_READ, "zstdvfs: crc mismatch re-reading rebuild chunk %u", chunkIdx);
        return SQLITE_IOERR_READ;
      }
      rc = zcodec_decompress(rb->paybuf, r.nPayload, (r.flags&ZF_RAW)?1:0, rb->scratch, rb->chunkAmt);
      if(rc) return rc;
    }else{
      memset(rb->scratch, 0, rb->chunkAmt);
    }
    memcpy(rb->scratch + (u32)(off - (i64)chunkStart), p, (size_t)amt);
    rc = rebuild_place_page(c, rb, rb->scratch, rb->chunkAmt, chunkIdx+1, rb->newMap);
    if(rc) return rc;
    rebuild_note_placed(rb, chunkIdx);
    return SQLITE_OK;
  }
  /* amt > rb->chunkAmt: not a shape backupOnePage (or its own sub-page
  ** patch path) ever produces -- reject rather than guess. */
  sqlite3_log(SQLITE_IOERR_WRITE,
    "zstdvfs: rebuild write larger than the stream's own page size (amt=%d chunkAmt=%u)",
    amt, rb->chunkAmt);
  return SQLITE_IOERR_WRITE;
}

/* The current PENDING_BYTE value, queried rather than hardcoded: it is
** 0x40000000 (1 GiB) in every real deployment, but every test in SQLite's
** OWN suite (tester.tcl, unconditionally, at startup) and in this
** project's own test_vacuum.c lowers it via
** sqlite3_test_control(SQLITE_TESTCTRL_PENDING_BYTE, ...) so ordinary
** test-sized databases exercise the boundary too -- hardcoding either
** value would silently stop matching whichever one is not hardcoded.
** sqlite3.c's own internal `sqlite3PendingByte` global holds the real
** answer, but it is declared SQLITE_PRIVATE (expands to `static` in the
** standard amalgamation build this project links against), invisible
** outside that one translation unit -- not reachable via `extern`, in
** either this project's statically-linked test binaries or the loadable
** extension's dynamic-lookup host process. sqlite3_test_control's own
** documented contract for this opcode is the sanctioned way around that:
** "Set the PENDING byte to the value in the argument, if X>0. Make no
** changes if X==0. Return the value of the pending byte as it existed
** before this routine was called" -- X==0 is a pure, side-effect-free
** query. */
static int zvfs_pending_byte(void){
  return sqlite3_test_control(SQLITE_TESTCTRL_PENDING_BYTE, 0);
}

/* Task 19 fix (found via SQLite's own vacuum.test, not this project's
** narrower coverage -- root-caused rather than left as a gap): the byte
** range [PENDING_BYTE, PENDING_BYTE+pageSize) is SQLite's own reserved
** "locking page," covering exactly one page number
** (PENDING_BYTE/pageSize + 1) in whatever page size is in play -- real
** content is never allocated there, by construction, in EVERY SQLite
** database, compressed or not (see backup.c's own PENDING_BYTE_PAGE
** checks in backupOnePage and sqlite3_backup_step's tail). A VACUUM whose
** own page size does not INCREASE relative to the still-current
** destination (same size, or shrinking without a sub-page-patch-eligible
** direction -- see zctr_rebuild_write's own sub-page-patch branch for the
** one direction that DOES get an explicit raw-write patch there instead)
** simply never writes the chunk(s) covering this region at all: not a
** truncated/abandoned stream, not evidence of incompleteness, just SQLite
** declining to copy a page that has no logical content to copy. Treating
** this as "incomplete" (the completeness gate's default, correct, for
** every OTHER kind of gap) would silently discard an entire correctly-
** completed VACUUM -- and worse, per this project's own test_convert.c/
** test_vacuum.c investigation, a spuriously-triggered fallback here can
** itself be corrupting: SQLite's OWN post-backup steps (e.g. a page-count
** truncate sized for the NEW generation) still arrive on this SAME
** connection afterward, believing the rebuild succeeded, and land on
** whatever this fallback silently reverted to instead.
**
** Safe to tolerate unconditionally, with no ordering/count requirement:
** zctr_read already zero-fills any chunk absent from a container's own
** map within its valid range (matching what a real, uncompressed SQLite
** file provides for this exact region -- unspecified bytes SQLite itself
** never reads), so a permanently-absent pending-byte chunk in the
** COMMITTED map is exactly as correct as a present-but-never-referenced
** one would be. The tolerance window covers
** [PENDING_BYTE, PENDING_BYTE+max(chunkAmt,finalPageSize)) rather than
** just one chunkAmt-wide slice: a page-size-INCREASING rebuild (chunkAmt
** the OLD, smaller size) can leave more than one chunkAmt-sized
** destination sub-write skipped for the SAME single final (larger) page
** (confirmed empirically: a 4096->8192 VACUUM crossing this boundary
** leaves TWO consecutive 4096-byte chunks unwritten, not one) -- bounding
** by the larger of the two sizes comfortably covers that without
** tolerating an unrelated, genuinely-missing chunk elsewhere: every
** chunk outside this one narrow window still has to be present, or the
** gate still reports incomplete exactly as before. pendingByte is queried
** ONCE by the caller (rebuild_check_complete) and passed in here, rather
** than each call re-querying sqlite3_test_control itself -- it cannot
** change mid-check (its own doc comment: changing it with any connection
** open is undefined), so re-querying per candidate chunk is pure waste. */
static int pending_gap_tolerable(struct ZvfsRebuild *rb, u64 chunkIdx, u64 pendingByte){
  if(rb->chunkAmt==0) return 0;
  u32 span = rb->finalPageSize > rb->chunkAmt ? rb->finalPageSize : rb->chunkAmt;
  u64 lo = pendingByte;
  u64 hi = lo + span;
  u64 chunkLo = chunkIdx * (u64)rb->chunkAmt;
  u64 chunkHi = chunkLo + rb->chunkAmt;
  return chunkLo < hi && chunkHi > lo;
}

/* Task 19 completeness gate (coordinator-ruled "REBUILD ACCEPTANCE v3",
** CORRECTED after review: the original ruling's "incomplete -> silently
** fall back, return SQLITE_OK" safety argument was wrong -- see zctr_sync's
** own comment for the corrected argument and what this function's caller
** now does with a !*pComplete result. This function's own job is
** unchanged: decide complete or not, and say why via sqlite3_log so a
** failed VACUUM's cause is diagnosable). Does the staged chunk-keyed map
** hold every chunk 0..chunkCount-1, or -- the one deliberate exception,
** see pending_gap_tolerable below -- every chunk EXCEPT ones covering
** SQLite's own reserved "pending byte" region, AND does chunk 0's own
** CURRENT staged content still agree with the finalPageSize learned when
** it first arrived? chunkCount is derived from an explicit
** zctr_truncate(while rebuild) if one arrived (rb->haveCount), else from
** the highest chunk index actually placed (rb->maxChunkSeen+1) -- "if none
** arrived, the max staged pgno" per the ruling. Also returns chunkCount
** itself (the caller needs it again, for rebuild_reassemble/the final
** page-count computation, so it is not re-derived twice). A truncate size
** that isn't a whole multiple of the stream's own chunk size is treated as
** incomplete rather than a hard error here -- reported the same
** discard-and-fail way as every other incompleteness this gate catches,
** just diagnosed at this specific point instead. */
static int rebuild_check_complete(ZvfsContainer *c, struct ZvfsRebuild *rb, u64 *pChunkCount, int *pComplete){
  *pComplete = 0;
  *pChunkCount = 0;
  if(!rb->anyPlaced) return SQLITE_OK;    /* zero-progress: nothing to check */
  u64 chunkCount;
  if(rb->haveCount){
    if(rb->chunkAmt==0 || rb->countBytes % rb->chunkAmt != 0){
      sqlite3_log(SQLITE_IOERR_TRUNCATE,
        "zstdvfs: rebuild truncate size (%llu) not a multiple of the stream's page size (%u) -- failing the VACUUM",
        (unsigned long long)rb->countBytes, rb->chunkAmt);
      return SQLITE_OK;                   /* incomplete */
    }
    chunkCount = rb->countBytes / rb->chunkAmt;
  }else{
    chunkCount = (u64)rb->maxChunkSeen + 1;
  }
  *pChunkCount = chunkCount;
  u64 pendingByte = (u64)(unsigned int)zvfs_pending_byte();  /* queried once, not per chunk */
  for(u64 i=0; i<chunkCount; i++){
    ZvfsMapEntry e;
    int rc = zmap_get(rb->newMap, (u32)(i+1), &e);
    if(rc) return rc;
    if(e.off==0 && !pending_gap_tolerable(rb, i, pendingByte)){
      sqlite3_log(SQLITE_IOERR_WRITE,
        "zstdvfs: incomplete rebuild -- chunk %llu of %llu never arrived -- failing the VACUUM",
        (unsigned long long)i, (unsigned long long)chunkCount);
      return SQLITE_OK;                   /* genuinely missing -- *pComplete stays 0 */
    }
  }
  if(rb->finalPageSize==0) return SQLITE_OK;  /* defensive: chunk 0 present should already imply this */
  /* Controller-ruled Critical #2 fix: every chunk being PRESENT is not
  ** enough -- a rollback replay can legitimately re-arrive after the
  ** forward copy already finished, rewriting chunk 0 with the ORIGINAL
  ** (pre-VACUUM) page-1 content, still satisfying every check above (the
  ** map entry is merely replaced, not removed) while silently
  ** invalidating rb->finalPageSize, which was learned once, from whatever
  ** chunk 0's content happened to be at that earlier moment. Re-derive the
  ** page size from chunk 0's CURRENT staged content (bytes 16..17,
  ** big-endian, 1 meaning 65536 -- same field, same decode as the original
  ** learning site) and require it to still equal rb->finalPageSize. A
  ** genuine forward copy's own chunk 0 always agrees with itself; a
  ** rollback-replay-clobbered chunk 0 reads back as the OLD page size,
  ** which can only equal finalPageSize by construction if the VACUUM
  ** never actually changed the page size to begin with (chunkAmt==
  ** finalPageSize) -- exactly the one case where old and new content
  ** cannot be distinguished this way but also cannot silently corrupt
  ** anything by conflating them (same size throughout, nothing to
  ** mis-chunk). */
  {
    ZvfsMapEntry e0;
    int rc = zmap_get(rb->newMap, 1, &e0);
    if(rc) return rc;
    if(e0.off){
      ZvfsRec r;
      rc = zctr_read_record(&c->io, e0.off, &r, rb->paybuf, rb->chunkAmt);
      if(rc) return rc;
      if((r.flags & ZF_RAW) && r.crc != zcrc32(0, rb->paybuf, r.nPayload)){
        sqlite3_log(SQLITE_IOERR_READ,
          "zstdvfs: crc mismatch re-reading rebuild chunk 0 for the final page-size check -- failing the VACUUM");
        return SQLITE_OK;
      }
      rc = zcodec_decompress(rb->paybuf, r.nPayload, (r.flags&ZF_RAW)?1:0, rb->scratch, rb->chunkAmt);
      if(rc) return rc;
      u32 ps = ((u32)rb->scratch[16]<<8) | rb->scratch[17];
      if(ps==1) ps = 65536;
      if(ps != rb->finalPageSize){
        sqlite3_log(SQLITE_IOERR_WRITE,
          "zstdvfs: rebuild chunk 0's current page-size field (%u) no longer matches the page size learned "
          "when chunk 0 first arrived (%u) -- most likely a rollback replay rewrote chunk 0 with original "
          "content after the forward copy finished -- failing the VACUUM", ps, rb->finalPageSize);
        return SQLITE_OK;
      }
    }
  }
  *pComplete = 1;
  return SQLITE_OK;
}

/* If the stream's own chunk size differs from the true final page size (a
** genuine page-size-changing VACUUM -- test_vacuum.c exercises this),
** reassemble: walk chunks 0..chunkCount-1 in ORDER (not arrival order --
** rebuild_check_complete has already proven every one is present),
** decompress, concatenate, and re-chunk into real finalPageSize-sized
** pages placed into a FRESH map. If the sizes match (the common case, and
** the one test/bigsmoke.sh exercises), the chunk-keyed map already IS the
** final pgno-keyed map -- returned directly, no-op.
**
** The intermediate chunk-keyed PAGE records this discards (only when
** reassembly actually runs) are reclaimed by the caller (zctr_sync_rebuild):
** because the whole chunk-keyed stream and this function's own fresh
** finalPageSize-keyed placements are both append-only, in that order, with
** nothing else interspersed, the superseded records form one contiguous
** span the caller folds into its own free-span reset in one shot -- see
** its own comment. Still an accepted, much narrower limitation: the
** discarded map's own COW node bookkeeping (the internal tree structure,
** not the page records it indexed) has no reclaim path, since ZvfsMap's
** API doesn't expose enumeration for it -- negligible next to the page
** records, which dominate the footprint this discards. */
static int rebuild_reassemble(ZvfsContainer *c, struct ZvfsRebuild *rb, u64 chunkCount,
                               ZvfsMap **pFinalMap, u64 *pFinalCount){
  if(rb->chunkAmt == rb->finalPageSize){
    *pFinalMap = rb->newMap;
    *pFinalCount = chunkCount;
    return SQLITE_OK;
  }
  ZvfsMap *fresh = zmap_new(&c->io);
  zmap_reset(fresh, 0, 0);
  u64 totalBytes = chunkCount * rb->chunkAmt;
  if(totalBytes % rb->finalPageSize != 0){
    sqlite3_log(SQLITE_IOERR_WRITE,
      "zstdvfs: rebuild byte stream (%llu bytes) not a multiple of the new page size (%u)",
      (unsigned long long)totalBytes, rb->finalPageSize);
    zmap_delete(fresh);
    return SQLITE_IOERR_WRITE;
  }
  u64 finalCount = totalBytes / rb->finalPageSize;
  u8 *pageBuf = sqlite3_malloc64(rb->finalPageSize);
  if(!pageBuf){ zmap_delete(fresh); return SQLITE_NOMEM; }
  u64 bytePos = 0;
  u32 curChunkIdx = 0xFFFFFFFFu;
  int rc = SQLITE_OK;
  for(u64 finalPgno=1; finalPgno<=finalCount && rc==SQLITE_OK; finalPgno++){
    u32 filled = 0;
    while(filled < rb->finalPageSize){
      u32 chunkIdx = (u32)(bytePos / rb->chunkAmt);
      u32 inChunkOff = (u32)(bytePos % rb->chunkAmt);
      if(chunkIdx != curChunkIdx){
        ZvfsMapEntry e;
        rc = zmap_get(rb->newMap, chunkIdx+1, &e);
        if(rc) break;
        if(e.off==0){
          /* Absent chunk: the completeness gate (rebuild_check_complete)
          ** only ever let this attempt through with a gap here if it fell
          ** entirely inside the tolerated pending-byte window
          ** (pending_gap_tolerable) -- SQLite's own reserved page, no real
          ** content to reconstruct. Zero-fill, matching zctr_read's own
          ** treatment of the identical gap in an ordinary (non-reassembled)
          ** committed map. */
          memset(rb->scratch, 0, rb->chunkAmt);
        }else{
          ZvfsRec r;
          rc = zctr_read_record(&c->io, e.off, &r, rb->paybuf, rb->chunkAmt);
          if(rc) break;
          if((r.flags & ZF_RAW) && r.crc != zcrc32(0, rb->paybuf, r.nPayload)){
            sqlite3_log(SQLITE_IOERR_READ, "zstdvfs: crc mismatch reassembling rebuild chunk %u", chunkIdx);
            rc = SQLITE_IOERR_READ; break;
          }
          rc = zcodec_decompress(rb->paybuf, r.nPayload, (r.flags&ZF_RAW)?1:0, rb->scratch, rb->chunkAmt);
          if(rc) break;
        }
        curChunkIdx = chunkIdx;
      }
      u32 avail = rb->chunkAmt - inChunkOff;
      u32 need = rb->finalPageSize - filled;
      u32 take = avail < need ? avail : need;
      memcpy(pageBuf+filled, rb->scratch+inChunkOff, take);
      filled += take;
      bytePos += take;
    }
    if(rc) break;
    rc = rebuild_place_page(c, rb, pageBuf, rb->finalPageSize, (u32)finalPgno, fresh);
  }
  sqlite3_free(pageBuf);
  if(rc){ zmap_delete(fresh); return rc; }
  *pFinalMap = fresh;
  *pFinalCount = finalCount;
  return SQLITE_OK;
}

/* Step 5 of the rebuild flow: reassemble if the page size changed (see
** rebuild_reassemble, above), then the rebuild's own commit (this
** transaction's txn+1), then the pack-to-front loop, all under VACUUM's
** still-held EXCLUSIVE lock (gateOk hardcoded 1 -- see zcompact_full's own
** comment). Only ever reached once zctr_sync's own completeness gate has
** confirmed every expected chunk is present -- chunkCount is that same
** call's own computed value, passed through rather than re-derived. */
static int zctr_sync_rebuild(ZvfsContainer *c, int gateOk, u64 chunkCount){
  struct ZvfsRebuild *rb = c->pRb;
  /* The old (superseded) generation's own eof -- NOT zalloc_eof(c->alloc),
  ** which by now reflects the CURRENT (post-append-stream) eof, way past
  ** everything the append-only rebuild stream just wrote. For an ordinary
  ** OVERWRITE rebuild on an already-converted container this is c->hdr.eof
  ** (the durable header nothing during the stream ever touches); for a
  ** Task 16 plain-db conversion it is the plain database's own rounded
  ** size instead (c->hdr.eof is 0 for a virgin container, which would
  ** reclaim nothing) -- rb->reclaimBase carries whichever is correct (see
  ** struct ZvfsRebuild's own comment and zctr_create_for_convert). Either
  ** way, this is exactly "where the now-superseded generation ends," the
  ** boundary zalloc_reset_span needs below. */
  u64 oldEof = rb->reclaimBase;
  /* Task 19 fix (found via test_vacuum.c's own pending-byte shrink case,
  ** root-caused rather than left as the documented leak an earlier draft
  ** of rebuild_reassemble accepted): captured BEFORE rebuild_reassemble
  ** runs, this is exactly the boundary between "everything the chunk-keyed
  ** append-only stream itself wrote" and "the fresh, finalPageSize-keyed
  ** records rebuild_reassemble is about to append above it" -- see below. */
  u64 preReassembleEof = zalloc_eof(c->alloc);

  ZvfsMap *finalMap; u64 finalCount;
  int rc = rebuild_reassemble(c, rb, chunkCount, &finalMap, &finalCount);
  if(rc) return rc;
  if(finalMap != rb->newMap){
    /* Reassembly ran (chunkAmt != finalPageSize): the chunk-keyed records
    ** rb->newMap referenced are now entirely superseded by finalMap's own
    ** fresh finalPageSize-keyed records, placed strictly above them (both
    ** are append-only placements, in that order). Because the stream never
    ** wrote anything else in between, that whole superseded run is exactly
    ** the single contiguous span [oldEof, preReassembleEof) -- folded into
    ** the free-span reset below alongside the truly-old generation, in one
    ** shot, the same immediately-available (not txn/gateOk-gated) way
    ** zalloc_reset_span already frees the old generation: OVERWRITE only
    ** ever runs under EXCLUSIVE (spec Sec7.4), so there is never a
    ** concurrent reader to protect either span from. Only rb->newMap's own
    ** COW node bookkeeping (the map's internal tree structure, not the
    ** page records it indexed) stays an accepted, much smaller leak --
    ** ZvfsMap's API has no enumeration hook to reclaim that part too. */
    oldEof = preReassembleEof;
    zmap_delete(rb->newMap);
  }

  zmap_delete(c->map);                    /* old map discarded (brief, step 5) */
  c->map = finalMap;
  c->pageSize = rb->finalPageSize;
  c->stagedCount = finalCount;
  /* Invalidate pg1/pgbuf/paybuf HERE, not at the end of this function (an
  ** earlier version did): pgbuf/paybuf are scratch buffers sized to
  ** c->pageSize AT ALLOCATION TIME, and zcompact_step's own guard
  ** (compact.c) only allocates paybuf when it's currently NULL --
  ** otherwise reusing whatever's already there unconditionally. If a
  ** page-size-changing VACUUM leaves a stale, OLD-size paybuf sitting
  ** around from before c->pageSize just changed two lines above, every
  ** pack-loop commit's own zcompact_step call (below) would read a live
  ** PAGE record's payload into it capped at the NEW (possibly larger)
  ** c->pageSize -- a real heap-buffer-overflow write once that payload's
  ** actual compressed size exceeds the stale buffer's true (old,
  ** smaller) allocation. Caught via ASan on a page-size-INCREASING VACUUM
  ** (4096->8192) whose content was incompressible enough for a compacted
  ** page's compressed size to exceed the stale 4096-byte buffer -- with
  ** the earlier, more-compressible test content this never actually
  ** overflowed (the compressed size never got that large), which is why
  ** it went uncaught until fix round 3's own pending-byte test used
  ** genuinely random content. pg1's own staleness (reflects the old map)
  ** was already a reason to drop it somewhere in this function; dropping
  ** everything here, before pageSize is used by anything else, is both
  ** simpler and the only placement that's actually correct. */
  zctr_drop_buffers(c);

  /* Bug fix (Task 2 root-cause; docs/design.md Sec7.4's "RESOLVED --
  ** reset_span reused the still-current generation's own space" entry has
  ** the full writeup, including the field-shaped reproduction via
  ** diskfull.test's diskfull-2.298). This span describes the OLD
  ** generation, which right now is STILL the currently-durable one --
  ** this very commit (not yet committed; its own header hasn't flipped
  ** yet) is only in the process of superseding it. zalloc_reset_span now
  ** stages it as ONE PENDING generation tagged with this commit's own
  ** intended txn (matching commit_once's identical internal computation,
  ** c->hdr.txn+1) rather than handing it out as immediately-reusable free
  ** space -- the same two-generation discipline (Sec5.2 step 4/Sec5.3)
  ** every ordinary commit's own frees already follow. commit_once's own
  ** step 0 release (gated on the CURRENTLY DURABLE c->hdr.txn, read at
  ** its own start, still the OLD txn while THIS commit is in flight)
  ** correctly leaves it pending through this commit and releases it on
  ** the very next one -- once c->hdr.txn has genuinely advanced to
  ** intendedTxn, true whether that happens via this commit's ordinary
  ** success path or a torn-flip-safe conversion's sub-case A partial
  ** adoption (zctr_sync_abort), since both durably write the identical
  ** intended txn. A real (non-crash) failure partway through commit_once
  ** -- after at least one node/record had already been placed, before
  ** this fix, into a prematurely-reused span -- used to leave the still-
  ** durable OLD header (commit_once's own abort path correctly falls
  ** back to it) pointing at content that same failed commit had already
  ** overwritten: permanent, unrecoverable corruption. Now nothing this
  ** commit places can ever land in the OLD generation's own span before
  ** it is provably superseded, so that fallback is genuinely safe again. */
  u64 intendedTxn = c->hdr.txn + 1;
  zalloc_reset_span(c->alloc, ZVFS_HDR_BLOCK_SIZE, oldEof, zalloc_eof(c->alloc), intendedTxn);
  zalloc_set_appendonly(c->alloc, 0);

  sqlite3_free(rb->scratch);
  sqlite3_free(rb->paybuf);
  sqlite3_free(rb);
  c->pRb = 0;

  /* c->rebuild stays 1 through this one commit_once call: its two
  ** c->rebuild-gated skips (zcompact_step; freeing the old list records
  ** zalloc_reset_span already subsumed) both apply specifically to THIS
  ** commit and must not fire for the pack loop's own commits below --
  ** cleared immediately after, before the loop starts. */
  rc = commit_once(c, gateOk);
  c->rebuild = 0;
  if(rc) return rc;

  /* Pack loop: while(zcompact_full reports progress). zcompact_full's own
  ** "moved" signal is necessarily optimistic (see its own comment in
  ** compact.c) -- it fires whenever relocating a node LOOKS worthwhile
  ** from a best-fit peek, before the commit's own cascading COW placement
  ** (which the peek can't fully predict -- other relocations in the same
  ** commit, plus list-record placement, all compete for the same free
  ** space afterward) actually lands it. For most fragmentation shapes
  ** this converges in a handful of passes (see the task report: 6-8
  ** passes on that test's own data) because a "moved-only" pass is always
  ** followed by exactly one "trim" pass that reclaims what it vacated
  ** (§5.3's release-next-commit lag, never more than one pass, since any
  ** commit's own frees become visible to the very next commit's own
  ** start-of-commit release+trim).
  **
  ** But relocating any node unconditionally cascades into rewriting every
  ** ancestor up to the root (zmap_commit's own design: the root-level
  ** commit_node call always takes a fresh extent, whether or not anything
  ** under it actually needed to move) -- and for some layouts an interior
  ** node and one of its own ancestors can perpetually trade positions,
  ** each pass's fresh best-fit placement undoing the improvement that
  ** triggered the previous one. Found via e_vacuum.test's own churn+VACUUM
  ** sequence (heavy fragmentation across a table AND two indexes sharing
  ** one file): "moved" stayed permanently true while eof never moved
  ** again, hanging this loop forever. Verified directly that stopping
  ** early on exactly that input leaves a fully valid, integrity-check-
  ** clean container (just not maximally dense) -- every pass is an
  ** independent, complete, crash-safe commit, so stopping the loop at any
  ** point is always safe, never a partial/torn state.
  **
  ** Guard: give up once STALL_LIMIT consecutive passes fail to shrink eof
  ** at all, regardless of what "progress" claims -- set generously above
  ** the lag a genuinely-converging run can ever need (never more than one
  ** non-shrinking pass between shrinks, per the §5.3 argument above), so
  ** it only ever fires on a real non-convergence, not a slow-but-real one.
  ** PASS_CAP is a second, independent backstop. */
  #define ZCTR_PACK_STALL_LIMIT 4
  #define ZCTR_PACK_PASS_CAP    200
  int progress;
  int stall = 0, npass = 0;
  u64 lastEof = zalloc_eof(c->alloc);
  do {
    progress = zcompact_full(c);
    /* Found while validating Item 1's `make suite` gate (see
    ** .superpowers/reports/sweep-ckptdone-report.md's own "known issue"
    ** section -- this fix is real and independently correct, but it did
    ** NOT resolve that gate's own diskfull.test failure; that failure's
    ** true root cause is still open). A FAILED pack pass here must not be
    ** reported as a failure of this whole function. The rebuild's own
    ** commit, immediately above (the "rc = commit_once(...); if(rc)
    ** return rc;" a few lines up), is what SQLite's own xSync call
    ** actually corresponds to, and it has ALREADY landed durably by this
    ** point -- everything from here down is this container's OWN
    ** optional, best-effort follow-on densification (design.md Sec7.4:
    ** "a loop of ordinary, already-crash-safe commits... under VACUUM's
    ** still-held exclusive lock"), not something SQLite asked for or is
    ** waiting on. zcompact_full's own commit_once call already leaves the
    ** container in a fully valid, durable state on ANY failure (its own
    ** abort path reloads from whatever IS durable on disk -- at least the
    ** rebuild's own commit, possibly a later pack pass if some earlier
    ** ones already succeeded) -- §5.1's invariant, unaffected by this
    ** change. The OLD behavior (propagating -progress as this function's
    ** own return value) told SQLite "the xSync for your VACUUM failed" --
    ** even though the VACUUM itself unambiguously succeeded -- which
    ** makes SQLite's own VACUUM machinery believe the whole statement
    ** failed and attempt to roll back via journal replay, restoring
    ** PRE-VACUUM logical pages through ordinary zctr_write calls onto a
    ** container that is ALREADY at the NEW (different-layout, smaller
    ** page count) generation -- a genuinely hazardous mismatch this
    ** project's own design never anticipated (VACUUM's crash-safety
    ** argument, Sec7.4, is about a real process crash, where nothing
    ** reactively replays a journal against an already-committed newer
    ** generation in the SAME process). Stopping the loop but still
    ** reporting overall success is what design.md's own words already
    ** promise ("a crash mid-pack loop simply leaves the previous...
    ** commit as the durable state") -- this fix is what actually
    ** delivers that promise for a same-process I/O failure, not just a
    ** real crash, which the pre-fix `return -progress;` did not. Kept
    ** despite not closing the diskfull.test gap: it is correct on its own
    ** terms, matches this project's own documented design intent, and is
    ** very likely still a necessary (if not sufficient) part of the fix
    ** for that open issue and for the coordinator's own H2 field report,
    ** whose artifacts match this exact shape ("the VACUUM's own commit
    ** landed and then something failed after it"). */
    if(progress < 0){
      sqlite3_log(SQLITE_WARNING,
        "zstdvfs: VACUUM's post-rebuild pack loop stopped early after %d "
        "pass(es) (rc=%d) -- the rebuild itself is durably committed; "
        "density will keep improving on later ordinary commits", npass, -progress);
      break;
    }
    npass++;
    u64 eofNow = zalloc_eof(c->alloc);
    if(eofNow < lastEof){ stall = 0; lastEof = eofNow; }
    else { stall++; }
  } while(progress > 0 && stall < ZCTR_PACK_STALL_LIMIT && npass < ZCTR_PACK_PASS_CAP);
  #undef ZCTR_PACK_STALL_LIMIT
  #undef ZCTR_PACK_PASS_CAP

  return SQLITE_OK;
}

int zctr_sync(ZvfsContainer *c, int syncFlags, int gateOk){
  (void)syncFlags;
  if(!c->dirty) return c->io.xSync(c->io.ctx);
  if(c->rebuild){
    /* Task 19 completeness gate (coordinator-ruled "REBUILD ACCEPTANCE
    ** v3"; CORRECTED after review -- see below for what changed and why).
    ** A commit reaching this dispatch while c->rebuild is set must not run
    ** the real rebuild commit (zctr_sync_rebuild) unless every expected
    ** chunk (1..chunkCount, derived from an explicit zctr_truncate if one
    ** arrived, else the highest chunk actually placed) is genuinely
    ** present in the staged map AND chunk 0's own current content still
    ** agrees with the page size learned when it first arrived -- running
    ** it on an incomplete or inconsistent map would swap in a generation
    ** missing or misinterpreting real content and, via zalloc_reset_span,
    ** mark the ENTIRE currently-committed database free, wiping it (the
    ** exact hazard the old zero-progress guard existed for; v3 generalizes
    ** it to "any incompleteness or inconsistency," not just "literally
    ** nothing arrived," because order-independent acceptance means a
    ** genuinely incomplete forward copy and a partial rollback replay both
    ** look identical to "some chunks present, not all" -- see
    ** rebuild_check_complete, and the struct's own comment, for the full
    ** detection argument).
    **
    ** CORRECTED (controller review overturned the original ruling's own
    ** safety argument here): incomplete or inconsistent -> discard the
    ** whole staged attempt (same helper the old guard used) and FAIL this
    ** sync with a real error, for BOTH an ordinary already-converted
    ** container's OVERWRITE and a plain-db CONVERSION alike -- no more
    ** falling through to an ordinary commit_once. The original ruling
    ** argued "VACUUM never changes logical content" made a silent,
    ** same-content fallback safe; that conflates ROW content (which VACUUM
    ** does preserve) with PAGE LAYOUT (which is VACUUM's entire purpose to
    ** rewrite). By the time this sync call returns, SQLite already
    ** believes the rebuild succeeded -- its pager cache holds clean
    ** NEW-layout pages, BTS_PAGESIZE_FIXED has cleared, the schema cookie
    ** has moved -- so a silent revert to OLD-layout content is a genuine
    ** divergence between what SQLite thinks page N holds and what this
    ** container actually serves for it, not a benign no-op; distinguishing
    ** "SQLite is itself about to roll back anyway" from every other
    ** incomplete-stream cause is not possible from this call alone. Fail
    ** loudly instead: SQLite sees the sync fail, rolls back, and reports
    ** VACUUM failure to the caller -- the ensuing journal replay writes
    ** original content back over identical still-committed content (a
    ** true semantic no-op, since discard-and-fail below never touched
    ** anything the durable header references), so the database stays
    ** correct and the user learns the VACUUM did not happen instead of
    ** silently getting an undensified (but, under the old ruling,
    ** possibly-divergent) one. */
    u64 chunkCount; int complete;
    int rc = rebuild_check_complete(c, c->pRb, &chunkCount, &complete);
    if(rc) return rc;
    if(!complete){
      rc = zctr_rebuild_abort(c);
      if(rc) return rc;
      return SQLITE_IOERR_WRITE;
    }
    return zctr_sync_rebuild(c, gateOk, chunkCount);
  }
  return commit_once(c, gateOk);
}

int zctr_write(ZvfsContainer *c, const void *buf, int amt, i64 off){
  if(c->rebuild) return zctr_rebuild_write(c, buf, amt, off);
  const u8 *pg = buf;
  if(c->pageSize==0){
    /* First write ever to a brand-new container. SQLite's pager always
    ** writes exactly one full page per xWrite to the main db, so amt IS
    ** the page size regardless of which page this happens to be -- the
    ** pager's cache can spill any dirty page to disk once a transaction's
    ** working set exceeds cache capacity, so the very first physical
    ** write to a fresh db is not guaranteed to be page 1 (a large
    ** single-transaction bulk insert reliably spills some other page
    ** first). Deriving the page size from amt instead of peeking at page
    ** 1's own header bytes handles that case uniformly. */
    if(amt<512 || amt>65536 || (amt&(amt-1)) || off % (i64)amt) return SQLITE_IOERR_WRITE;
    c->pageSize = (u32)amt;
  }
  if(amt!=(int)c->pageSize || off%(i64)c->pageSize){
    sqlite3_log(SQLITE_IOERR_WRITE,
      "zstdvfs: non page-aligned write (amt=%d off=%lld ps=%u)", amt,(long long)off,c->pageSize);
    return SQLITE_IOERR_WRITE;
  }
  if(off==0){
    /* Page 1's own header carries the file format's canonical page-size
    ** field (offset 16-17, big-endian, 1 meaning 65536) -- cross-check it
    ** against the page size the container is actually running with on
    ** EVERY write of page 1, not just the write that happened to
    ** establish c->pageSize in the first place. Defense in depth: amt
    ** already had to match c->pageSize above, so this only fires if page
    ** 1's own content disagrees with the size every write is using. */
    u32 ps = ((u32)pg[16]<<8) | pg[17];
    if(ps==1) ps = 65536;
    if(ps != c->pageSize){
      sqlite3_log(SQLITE_IOERR_WRITE,
        "zstdvfs: page 1 header page-size (%u) disagrees with container page size (%u)",
        ps, c->pageSize);
      return SQLITE_IOERR_WRITE;
    }
  }
  int rc = zctr_load_alloc(c);
  if(rc) return rc;
  u32 pgno = (u32)(off/c->pageSize) + 1;
  u64 txn = c->hdr.txn + 1;
  rc = zctr_ensure_scratch(c);
  if(rc) return rc;
  u32 n; int raw;
  rc = zcodec_compress(pg, c->pageSize, c->paybuf, &n, ZVFS_LEVEL_WRITE, &raw);
  if(rc) return rc;
  ZvfsMapEntry old;
  rc = zmap_get(c->map, pgno, &old);
  if(rc) return rc;
  ZvfsRec r = { .type=ZREC_PAGE, .flags=raw?ZF_RAW:0, .nPayload=n, .key=pgno,
                .crc = raw ? zcrc32(0,c->paybuf,n) : 0 };
  u32 nTotal = ZREC_TOTAL(n);
  ZvfsMapEntry e = { .off=zalloc_take(c->alloc, nTotal), .nPayload=n,
                    .flags=r.flags };
  rc = zctr_write_record(&c->io, e.off, &r, c->paybuf);
  if(rc){
    /* Same fix as rebuild_place_page's own (container.c), same reason:
    ** nothing landed at e.off, so give the extent back rather than leave
    ** it a permanent phantom gap for a later compaction pass to trip over.
    ** (zmap_set failing below, by contrast, does NOT warrant this: the
    ** record itself is a genuine, complete, durable write at that point --
    ** only the map's own reference to it failed to land.) */
    zalloc_untake(c->alloc, e.off, nTotal);
    return rc;
  }
  rc = zmap_set(c->map, pgno, &e);
  if(rc) return rc;
  /* Only free the old extent once its replacement is durably written AND
  ** referenced by the map (both of the calls above succeeded) -- never
  ** before. Freeing old.off any earlier (e.g. right after zmap_get, before
  ** confirming the new record landed) stages it as reusable-once-this-txn-
  ** commits while the map still authoritatively points at it on any early
  ** return above; a later commit of that state (whether the normal xSync
  ** path or vfs_shim.c's zvUnlock transaction-end commit for
  ** synchronous=OFF) would then release it via zalloc_release and let a
  ** subsequent allocation in that SAME commit overwrite bytes the
  ** just-committed map still needs -- corrupting a page no write ever
  ** actually touched. Found via tkt2565.test under Task 13 Part B: a write
  ** failing mid-transaction (simulated IO error) left exactly this old.off
  ** pending-free-but-still-referenced; the transaction's OTHER already-
  ** staged writes still left the container dirty, so the zvUnlock hook
  ** later committed it, reused the prematurely-freed extent for the map's
  ** own COW node, and silently overwrote the untouched page's still-live
  ** record -- the next read of that page then failed to decode. */
  if(old.off) zalloc_free(c->alloc, old.off, ZREC_TOTAL(old.nPayload), txn);
  if(pgno==1){
    if(!c->pg1){ c->pg1=sqlite3_malloc64(c->pageSize); if(!c->pg1) return SQLITE_NOMEM; }
    memcpy(c->pg1, pg, c->pageSize);
  }
  if(pgno > c->stagedCount) c->stagedCount = pgno;
  c->dirty = 1;
  return SQLITE_OK;
}

/* Read [off, off+amt) from the logical (uncompressed) file image. Pages fully
   past the committed+staged logical size are zero-filled; if any part of the
   requested range lies past that size, whatever exists is filled and the
   call still reports SQLITE_IOERR_SHORT_READ (matching sqlite3_file
   semantics, where a short read still delivers the bytes it has). */
int zctr_read(ZvfsContainer *c, void *buf, int amt, i64 off){
  u8 *out = (u8*)buf;
  i64 L = c->pageSize ? (i64)c->stagedCount * (i64)c->pageSize : 0;
  if(off >= L){
    memset(out, 0, (size_t)amt);
    return SQLITE_IOERR_SHORT_READ;
  }
  {
    int rc0 = zctr_ensure_scratch(c);
    if(rc0) return rc0;
  }
  i64 end = off + amt;
  i64 cur = off;
  int rc;
  while(cur < end && cur < L){
    u32 pgno = (u32)(cur / (i64)c->pageSize) + 1;
    i64 pageStart = (i64)(pgno - 1) * (i64)c->pageSize;
    i64 chunkEnd = pageStart + (i64)c->pageSize;
    if(chunkEnd > end) chunkEnd = end;
    u32 inPageOff = (u32)(cur - pageStart);
    u32 n = (u32)(chunkEnd - cur);
    const u8 *src;
    /* Task 19 (v3 rebuild acceptance): c->pg1's cache reflects whatever
    ** page 1 content was last read from the COMMITTED map -- stale the
    ** instant an in-flight rebuild has (re)staged a NEW page 1 into
    ** rb->newMap but that generation hasn't committed yet. Disable the
    ** shortcut (and stop populating it) for the whole duration of a
    ** rebuild; page 1 goes through the same overlay-then-committed lookup
    ** as every other page below instead. */
    if(!c->rebuild && pgno==1 && c->pg1){
      src = c->pg1;
    }else{
      ZvfsMapEntry e = {0,0,0};
      if(c->rebuild){
        /* Serve the rebuild stream's own staged overlay first (cache-spill
        ** re-reads of a page THIS same connection already wrote earlier in
        ** the copy -- e.g. before the sub-page pending-byte patch's own
        ** read-modify-write, or simply on ordinary pager cache pressure --
        ** must see that new content, not the pre-VACUUM committed one).
        ** Falls through to the committed map below only for a page the
        ** stream hasn't touched yet. Never reached during a plain-db
        ** CONVERSION's own rebuild (reads there stay on the PASSTHROUGH
        ** plain-byte path throughout -- see vfs_shim.c), so c->pageSize
        ** (used for `pgno` above) is always the real, already-committed
        ** page size here, and always equals rb->chunkAmt (pre-set from it
        ** at zctr_begin_overwrite). */
        rc = zmap_get(c->pRb->newMap, pgno, &e);
        if(rc) return rc;
      }
      if(e.off==0){
        rc = zmap_get(c->map, pgno, &e);
        if(rc) return rc;
      }
      if(e.off==0){
        memset(out + (cur-off), 0, n);
        cur = chunkEnd;
        continue;
      }
      ZvfsRec r;
      rc = zctr_read_record(&c->io, e.off, &r, c->paybuf, c->pageSize);
      if(rc){
        return rc;
      }
      if(r.type!=ZREC_PAGE || r.key!=pgno || r.nPayload>c->pageSize){
        sqlite3_log(SQLITE_IOERR_READ,
          "zstdvfs: bad page record pgno=%u off=%llu", pgno, (unsigned long long)e.off);
        return SQLITE_IOERR_READ;
      }
      if(r.flags & ZF_RAW){
        if(r.crc != zcrc32(0, c->paybuf, r.nPayload)){
          sqlite3_log(SQLITE_IOERR_READ,
            "zstdvfs: crc mismatch pgno=%u off=%llu", pgno, (unsigned long long)e.off);
          return SQLITE_IOERR_READ;
        }
      }
      rc = zcodec_decompress(c->paybuf, r.nPayload, (r.flags&ZF_RAW)?1:0, c->pgbuf, c->pageSize);
      if(rc){
        return rc;
      }
      if(pgno==1 && !c->rebuild){
        if(!c->pg1){ c->pg1=sqlite3_malloc64(c->pageSize); if(!c->pg1) return SQLITE_NOMEM; }
        memcpy(c->pg1, c->pgbuf, c->pageSize);
      }
      src = c->pgbuf;
    }
    memcpy(out + (cur-off), src + inPageOff, n);
    cur = chunkEnd;
  }
  if(end > L){
    memset(out + (cur-off), 0, (size_t)(end-cur));
    return SQLITE_IOERR_SHORT_READ;
  }
  return SQLITE_OK;
}

int zctr_truncate(ZvfsContainer *c, i64 logicalSize){
  if(c->rebuild){
    /* Task 15, rebuild flow step 4: VACUUM truncates the destination to its
    ** final page count before its commit sync. Task 19 (v3): store the raw
    ** logical BYTE size as-is, without dividing by any page size here --
    ** under order-independent acceptance, rb->chunkAmt may still be 0 (a
    ** virgin plain-db conversion whose first write hasn't arrived yet) and
    ** rb->finalPageSize may not be known either (chunk 0, i.e. page 1,
    ** might arrive after this truncate does) at the moment this call
    ** happens. Division into a chunk count -- and its own alignment
    ** validation -- is deferred entirely to the commit-time completeness
    ** gate (rebuild_check_complete), which by then has whatever chunkAmt
    ** ended up being. The committed map/count (what reads still serve; see
    ** zctr_read) does not move until that commit lands. */
    struct ZvfsRebuild *rb = c->pRb;
    rb->haveCount = 1;
    rb->countBytes = (u64)logicalSize;
    return SQLITE_OK;
  }
  if(c->pageSize==0 || logicalSize % (i64)c->pageSize != 0){
    sqlite3_log(SQLITE_IOERR_TRUNCATE,
      "zstdvfs: non page-aligned truncate (size=%lld ps=%u)", (long long)logicalSize, c->pageSize);
    return SQLITE_IOERR_TRUNCATE;
  }
  u64 newCount = (u64)(logicalSize / (i64)c->pageSize);
  if(newCount >= c->stagedCount){
    c->stagedCount = newCount;
    return SQLITE_OK;
  }
  int rc = zctr_load_alloc(c);
  if(rc) return rc;
  u64 txn = c->hdr.txn + 1;
  for(u64 pgno = newCount+1; pgno <= c->stagedCount; pgno++){
    ZvfsMapEntry e;
    rc = zmap_get(c->map, (u32)pgno, &e);
    if(rc) return rc;
    /* Stage pgno as absent (off=0) BEFORE freeing its old extent, and only
    ** free that extent once the map has actually been updated to stop
    ** referencing it (zmap_set below succeeded) -- same
    ** free-only-after-the-replacement-is-durably-staged rule zctr_write
    ** follows now (see its own comment), for the identical reason: freeing
    ** e.off first and then having zmap_set fail (e.g. OOM) would leave the
    ** map still pointing at an extent already staged reusable, which a
    ** later commit (including vfs_shim.c's zvUnlock transaction-end commit
    ** for synchronous=OFF) could release and let a subsequent allocation
    ** overwrite out from under the still-live reference. Clearing the
    ** staged entry also keeps every zmap_get for this pgno within this
    ** transaction correctly reporting "absent" from this point on: without
    ** it, zmap_get would keep resolving this pgno to the offset just freed
    ** (the free only touches the allocator's pending list, never the map on
    ** its own) -- if the same transaction later rewrites this pgno (e.g.
    ** auto_vacuum relocating content into a lower-numbered slot before the
    ** next xSync), zctr_write's own zmap_get(pgno) would otherwise see that
    ** same stale offset and free it a second time (the double-free Task 11
    ** fixed this loop for originally). */
    ZvfsMapEntry cleared = {0};
    rc = zmap_set(c->map, (u32)pgno, &cleared);
    if(rc) return rc;
    if(e.off) zalloc_free(c->alloc, e.off, ZREC_TOTAL(e.nPayload), txn);
  }
  c->stagedCount = newCount;
  c->dirty = 1;
  return SQLITE_OK;
}

int zctr_open(ZvfsContainer **pOut, ZvfsIO io){
  u8 buf[1024];
  ZvfsHdr hdr; int which;
  int rc = io.xRead(io.ctx, buf, sizeof(buf), 0);
  if(rc==SQLITE_OK) rc = zhdr_pick(buf, buf+ZVFS_HDR_COPY_SIZE, &hdr, &which);
  if(rc!=SQLITE_OK){
    return SQLITE_NOTADB;
  }
  ZvfsContainer *c = sqlite3_malloc64(sizeof(*c));
  if(!c) return SQLITE_NOMEM;
  memset(c, 0, sizeof(*c));
  c->io = io;
  c->hdr = hdr;
  c->whichHdr = which;
  c->pageSize = hdr.pageSize;
  c->stagedCount = hdr.pageCount;
  c->map = zmap_new(&c->io);
  zmap_reset(c->map, hdr.mapRoot, hdr.pageCount);
  c->alloc = zalloc_new();
  c->allocLoaded = 0;
  *pOut = c;
  return SQLITE_OK;
}

int zctr_create(ZvfsContainer **pOut, ZvfsIO io){
  ZvfsContainer *c = sqlite3_malloc64(sizeof(*c));
  if(!c) return SQLITE_NOMEM;
  memset(c, 0, sizeof(*c));
  c->io = io;
  c->whichHdr = -1;
  c->map = zmap_new(&c->io);
  zmap_reset(c->map, 0, 0);
  c->alloc = zalloc_new();
  c->allocLoaded = 1;             /* fresh allocator: nothing to load */
  *pOut = c;
  return SQLITE_OK;
}

void zctr_close(ZvfsContainer *c){
  if(!c) return;
  /* Task 15: a connection can close with a rebuild stream still in flight
  ** (e.g. its write transaction never reaches an explicit commit/rollback
  ** through this container before the pager tears the file handle down) --
  ** free the staged rebuild state rather than leaking c->pRb and its
  ** staged newMap. Never referenced by any durable header, so
  ** discarding it here (same helper every other rebuild-abandonment path
  ** uses -- now also vfs_shim.c's zvUnlock/zvClose, see its own comment)
  ** is exactly as safe as those; the extra c->rebuild/c->pRb/c->alloc
  ** field resets it performs are harmless on a container about to be
  ** freed outright regardless. zctr_rebuild_abort is self-guarding
  ** (no-op when c->pRb is already 0, e.g. because vfs_shim.c's zvClose
  ** already called it before this), so no need to gate the call here. */
  zctr_rebuild_abort(c);
  zmap_delete(c->map);
  zalloc_delete(c->alloc);
  sqlite3_free(c->pg1);
  sqlite3_free(c->pgbuf);
  sqlite3_free(c->paybuf);
  sqlite3_free(c);
}

int zctr_revalidate(ZvfsContainer *c){
  u8 buf[1024];
  ZvfsHdr hdr; int which;
  int rc = c->io.xRead(c->io.ctx, buf, sizeof(buf), 0);
  if(rc==SQLITE_OK) rc = zhdr_pick(buf, buf+ZVFS_HDR_COPY_SIZE, &hdr, &which);
  if(rc!=SQLITE_OK){
    u64 sz = 0;
    int rc2 = c->io.xFileSize(c->io.ctx, &sz);
    if(rc2!=SQLITE_OK || sz!=0) return SQLITE_IOERR_READ;
    /* file now zero-length: reset to created-empty state */
    zctr_reset_empty(c);
    zctr_drop_buffers(c);
    c->dirty = 0;
    return SQLITE_OK;
  }
  if(hdr.txn != c->hdr.txn){
    c->hdr = hdr;
    c->pageSize = hdr.pageSize;
    c->stagedCount = hdr.pageCount;
    c->whichHdr = which;
    zmap_reset(c->map, hdr.mapRoot, hdr.pageCount);
    c->allocLoaded = 0;
    zctr_drop_buffers(c);
  }
  return SQLITE_OK;
}

i64 zctr_logical_size(ZvfsContainer *c){
  return (i64)c->stagedCount * (i64)c->pageSize;
}

/* Task 13 Part B: exposes whether this container has staged, unsynced
   changes -- i.e. whether a commit (zctr_sync) would actually do work. Used
   by vfs_shim.c's zvUnlock hook to decide whether a lock-level drop needs to
   trigger the transaction-end commit that PRAGMA synchronous=OFF would
   otherwise skip entirely (see that hook's comment for the full mechanism). */
int zctr_is_dirty(const ZvfsContainer *c){
  return c->dirty;
}

/* Task 18: bind the pending-cap re-probe hook -- see ZvfsContainer.gateProbe's
** own comment (zvfs_int.h) and commit_once's own comment (above, near the
** top of this file) for what calls this and why. */
void zctr_set_gate_probe(ZvfsContainer *c, int (*probe)(void*), void *ctx){
  c->gateProbe = probe;
  c->gateProbeCtx = ctx;
}

/* Task 16: has this container EVER durably committed a header copy --
** i.e. is c->whichHdr something other than the "virgin, never committed"
** sentinel (-1)? Used by vfs_shim.c's zvSync/zvUnlock (PASSTHROUGH mode,
** converting a plain db) to decide whether to flip p->mode to CONTAINER,
** using this instead of the sync attempt's own return code: a conversion
** commit's torn-flip-safe write order (commit_once, B then A) means a
** REAL (non-crash) I/O error specifically on the second write (copy A,
** offset 0) can leave zctr_sync reporting failure to the caller (correct
** -- the sync call genuinely didn't fully complete) even though
** zctr_sync_abort's own recovery, finding the already-durable copy B on
** disk, has already adopted it as the current committed generation
** in-memory (c->whichHdr flips away from -1). From that point on this
** container IS the authoritative representation of the file -- if
** vfs_shim.c kept routing reads through raw PASSTHROUGH delegation
** instead of through this container, it would read a torn mix of
** already-overwritten container-header bytes (offsets 512-1023) and
** still-untouched original plain bytes (offsets 0-511), rather than the
** correct, fully-assembled logical content this container already knows
** how to serve. */
int zctr_has_committed(const ZvfsContainer *c){
  return c->whichHdr != -1;
}

u32 zctr_page_size(const ZvfsContainer *c){
  return c->pageSize;
}
