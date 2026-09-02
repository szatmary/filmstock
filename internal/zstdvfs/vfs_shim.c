#include "zstdvfs.h"
#include "zvfs_int.h"
#include <assert.h>
#include <stdlib.h>

typedef struct ZvfsVfs { sqlite3_vfs base; struct ZvfsVfs *pRegNext; } ZvfsVfs;
typedef struct ZvfsFile {
  sqlite3_file base;
  sqlite3_file *pReal;
  sqlite3_vfs *pBaseVfs;
  int openFlags;
  int mode;                    /* ZVFS_MODE_* */
  struct ZvfsContainer *pCtr;  /* container mode only; NULL for now */
  int lockLevel;                /* last SQLITE_LOCK_* level delegated
                                 ** successfully; tracks the real OS lock so
                                 ** zvLock can detect a NONE->(>=SHARED)
                                 ** acquisition (revalidation point, main-db
                                 ** only, see zvLock) and zvUnlock can detect
                                 ** a (>=RESERVED)->(<=SHARED) release
                                 ** (unlock-time commit point, see zvUnlock).
                                 ** Zero-initialized (== SQLITE_LOCK_NONE) by
                                 ** zvOpen's memset. */
  int converting;                /* Task 16: mode==PASSTHROUGH, but
                                 ** SQLITE_FCNTL_OVERWRITE has fired and
                                 ** pCtr holds a from-scratch conversion
                                 ** container (zctr_create_for_convert) whose
                                 ** rebuild stream is in flight. Reads still
                                 ** delegate to the untouched plain bytes
                                 ** (mode itself never changes for reads);
                                 ** writes/sync/size/truncate route to pCtr.
                                 ** Zero-initialized by zvOpen's memset. */
  /* Task 18 (WAL reader gate, spec Sec6.2): usedShm is set the moment
  ** zvShmMap is ever called on this handle -- true exactly for WAL
  ** connections (xShmMap/xShmLock are WAL-only; a rollback-journal
  ** connection never calls either), false for rollback mode, where the
  ** pager's own EXCLUSIVE lock already makes the gate trivially safe (see
  ** zvGateOk below). shmHeld[slot] tracks, per SQLITE_SHM_NLOCK==8 wal-index
  ** lock slot (WRITE=0, CKPT=1, RECOVER=2, READ(0..4)=3..7 -- wal.c's own
  ** stable, VFS-interop-documented layout, already relied on by
  ** zvShmLock's revalidation hook below), whether THIS handle currently
  ** holds a lock on that slot and WHICH KIND: 0 = unheld, 1 = SHARED, 2 =
  ** EXCLUSIVE. Set on a successful LOCK (to 1 or 2), cleared to 0 on a
  ** successful UNLOCK, both in zvShmLock.
  **
  ** The distinction matters, and is not just defensive bookkeeping: SQLite's
  ** own checkpointer (walCheckpoint, wal.c) holds WAL_READ_LOCK(0) (slot 3)
  ** EXCLUSIVELY on THIS SAME connection for the checkpoint's entire
  ** duration -- verified directly (instrumented build) that
  ** SQLITE_FCNTL_CKPT_DONE fires while that hold is still in place, released
  ** only afterward (confirmed against wal.c's own walCheckpoint: the
  ** "release the reader lock held while backfilling" walUnlockExclusive
  ** call sits AFTER both the CKPT_DONE file-control and the full-backfill
  ** truncate+xSync branch). A naive "any held slot closes the gate" rule
  ** (an EARLIER version of this comment described exactly that) makes the
  ** CKPT_DONE call site's gate UNCONDITIONALLY 0 forever -- not merely
  ** conservative, but permanently non-functional, since every checkpoint
  ** structurally holds slot 3 exclusively at that exact moment regardless
  ** of whether any external reader exists. zvGateOk (below) resolves this
  ** correctly: an EXCLUSIVE hold, by definition, already PROVES no other
  ** connection can hold that same slot in any mode -- strictly stronger
  ** than what the try-exclusive probe below would establish for an unheld
  ** slot, so it is treated as "this slot is safe," not "unknown, be
  ** conservative." A SHARED hold, by contrast, does not exclude another
  ** connection also holding it SHARED -- that case stays conservative
  ** (gate closed), matching the brief's original "someone -- possibly this
  ** connection -- holds a mark" reasoning, which remains correct for the
  ** SHARED case; it just does not generalize to EXCLUSIVE. Both fields
  ** zero-initialized by zvOpen's memset. */
  int usedShm;
  u8 shmHeld[8];
} ZvfsFile;

#define REAL(F) (((ZvfsFile*)(F))->pReal)

/* The base (wrapped) VFS is looked up via pVfs->pAppData rather than a
** trailing field on a private wrapper struct (as an earlier version of
** this shim did via a ZvfsVfs.pBaseVfs field). Test and shim VFS layers
** in the wild -- e.g. SQLite's own test_quota.c -- copy a sqlite3_vfs
** struct *by value* (`sThisVfs = *pOrigVfs`) to build a new VFS that
** reuses our method pointers with THEIR struct's address as pVfs. Any
** data hung off the end of our own wrapper struct is not part of that
** copy and reading it back through a cast is undefined behavior (in
** practice: a wild pointer dereference/segfault). pAppData is a plain
** member of sqlite3_vfs itself, present in every iVersion, so it survives
** a struct-value copy intact -- this is the sanctioned place to stash a
** shim's private "parent VFS" pointer. */
#define BASEVFS(V) ((sqlite3_vfs*)(V)->pAppData)

/* Adapts the base (real) sqlite3_file to the ZvfsIO vtable container.c
** consumes, so the container can drive physical IO without knowing
** anything about sqlite3_file/sqlite3_io_methods. xSync always uses
** SQLITE_SYNC_NORMAL: the container's own commit protocol (container.c)
** decides exactly when a barrier is needed and never needs the pager's
** FULL/DATAONLY distinction on the physical file underneath it. */
/* Field report ("SQLITE_FULL on large transactions", growth-full-report.md):
** SQLite's own os_unix.c never expects a single xRead/xWrite call larger
** than ~128 KiB (0x1ffff) -- both seekAndRead and seekAndWriteFd carry the
** identical contract marker `assert(cnt==(cnt&0x1ffff))`, an assertion
** compiled out under NDEBUG (auto-defined by sqliteInt.h whenever
** SQLITE_DEBUG is not explicitly set -- true of this project's own release
** build of third_party/sqlite/sqlite3.c; confirmed present at the exact
** same offset in the actual linked amalgamation, not just the reference
** source tree).
**
** The two sides of that contract are enforced asymmetrically once the
** assert is compiled out, which is why only the write side has an
** OBSERVED failure:
**  - WRITE: seekAndWriteFd masks the count to `nBuf & 0x1ffff` before the
**    real pwrite(). For a request >= 128 KiB this silently truncates the
**    call; unixWrite's own retry loop reissues the remainder, but because
**    128 KiB (0x20000) is an exact power of two, that remainder is ALWAYS
**    itself an exact multiple of 128 KiB -- so the next masked count is
**    exactly 0. write(fd,buf,0,...) returns 0 with no error; unixWrite
**    treats that as "nothing left, must be disk-full" and returns
**    SQLITE_FULL. Deterministic, not a race, not real disk pressure:
**    reproduced directly, confirmed on a machine with tens of GB free, and
**    exact by construction (see the report for the derivation).
**  - READ: seekAndRead does NOT mask -- it passes the full count straight
**    to pread()/read(). This works today only because a regular-file
**    pread() for a size like this ordinarily returns everything in one
**    call; unixRead has no retry loop at all for a short return (`got <
**    amt` goes straight to zero-fill + SQLITE_IOERR_SHORT_READ). So an
**    oversized read is not deterministically broken the way an oversized
**    write is, but it is silently OUT OF CONTRACT and fragile: any base
**    VFS (or kernel/filesystem combination) that legitimately short-reads
**    a large regular-file request would surface as spurious corruption
**    (SQLITE_IOERR_SHORT_READ / a bad record) with nothing to explain why.
**
** container.c's FREELIST/PENDING list records (commit_once, via
** zctr_write_record / zctr_load_alloc) are the only records in this format
** whose size is not bounded by a fixed page/node size -- a single
** transaction's worth of scattered index/WITHOUT ROWID churn can serialize
** a pending-free list past 128 KiB, in either direction (written by one
** commit, read back by the next). The fix belongs at THIS layer (the
** real-VFS adapter), not in container.c: it is a general property of
** every xRead/xWrite this shim ever issues to the base VFS, mirrors the
** same chunk-and-retry discipline os_unix.c itself uses internally for
** writes, and protects any future oversized record type without
** container.c (or any other caller of the ZvfsIO vtable) needing to know
** anything about a base-VFS limit that has nothing to do with our own
** format. Applied to both directions for the same reason, even though only
** the write side has an observed failure: symmetric hardening against the
** same documented ceiling, not a speculative fix for an unobserved bug. */
#define ZVFS_IO_MAX_XFER (64u*1024u)   /* safely under the ~128 KiB ceiling */
static int fio_read(void *ctx, void *b, u32 n, u64 off){
  sqlite3_file *f=ctx;
  u8 *p=b;
  u32 done=0;
  while(done<n){
    u32 chunk = n-done;
    if(chunk>ZVFS_IO_MAX_XFER) chunk=ZVFS_IO_MAX_XFER;
    int rc = f->pMethods->xRead(f, p+done, (int)chunk, (i64)(off+done));
    if(rc!=SQLITE_OK) return rc;
    done += chunk;
  }
  return SQLITE_OK;
}
static int fio_write(void *ctx, const void *b, u32 n, u64 off){
  sqlite3_file *f=ctx;
  const u8 *p=b;
  u32 done=0;
  while(done<n){
    u32 chunk = n-done;
    if(chunk>ZVFS_IO_MAX_XFER) chunk=ZVFS_IO_MAX_XFER;
    int rc = f->pMethods->xWrite(f, p+done, (int)chunk, (i64)(off+done));
    if(rc!=SQLITE_OK) return rc;
    done += chunk;
  }
  return SQLITE_OK;
}
static int fio_trunc(void *ctx, u64 sz){
  sqlite3_file *f=ctx; return f->pMethods->xTruncate(f,(i64)sz); }
static int fio_sync(void *ctx){
  sqlite3_file *f=ctx; return f->pMethods->xSync(f,SQLITE_SYNC_NORMAL); }
static int fio_fsize(void *ctx, u64 *p){
  sqlite3_file *f=ctx; i64 s; int rc=f->pMethods->xFileSize(f,&s); *p=(u64)s; return rc; }
static ZvfsIO file_io(sqlite3_file *pReal){
  ZvfsIO io = { pReal, fio_read, fio_write, fio_trunc, fio_sync, fio_fsize };
  return io;
}

/* Task 18: WAL reader gate (spec Sec6.2, brief's own "Mechanism" section).
** Computes whether extent recycling / tail truncation is safe to perform
** THIS commit, replacing every previously-hardcoded gateOk=1 constant (that
** constant is correct only under rollback-journal/EXCLUSIVE, where a sync
** is always the sole writer's commit point -- never true once a second WAL
** connection can hold an open read transaction against an older container
** generation).
**
** Rollback mode (p->usedShm==0, i.e. this handle has never touched shm at
** all -- xShmMap/xShmLock are WAL-only): SQLite's own pager holds EXCLUSIVE
** for the whole main-db write, so no reader can be mid-snapshot. Trivially
** safe; returns 1 without touching any lock.
**
** WAL mode: probe each of the 5 read-mark slots -- WAL_READ_LOCK(0..4) =
** shm lock indices 3..SQLITE_SHM_NLOCK-1, the same stable wal.c protocol
** zvShmLock's own revalidation hook below already relies on -- via a
** non-blocking try-exclusive xShmLock on the REAL underlying handle. This
** is the identical mechanism SQLite's own checkpointer uses (walLockExclusive
** over WAL_READ_LOCK) to prove no reader still needs the frames it's about
** to backfill; here it proves no reader still holds ANY mark at all (the
** brief's "v1 rule: recycle/truncate only when no read marks are held at
** all" -- spec Sec6.2).
**
** A slot THIS handle itself currently holds (p->shmHeld[slot], tracked by
** zvShmLock below) is never contended via a fresh try-exclusive -- but
** which kind of hold it is changes the verdict, and this is not optional
** nuance: an EXCLUSIVE hold on a slot, by definition, already proves no
** OTHER connection can hold that same slot in any mode right now -- exactly
** as strong a guarantee as a fresh successful try-exclusive would give, so
** it counts as "this slot is safe" and the loop moves on. This matters
** concretely for WAL_READ_LOCK(0) (slot 3): SQLite's own checkpointer
** (walCheckpoint, wal.c) holds it EXCLUSIVELY, on this very connection, for
** the checkpoint's entire duration, including through the
** SQLITE_FCNTL_CKPT_DONE call this gate is most often computed from
** (verified directly against wal.c: the matching walUnlockExclusive sits
** strictly after both CKPT_DONE and the full-backfill truncate+xSync
** branch) -- treating that self-hold as "unknown, be conservative" would
** make the CKPT_DONE call site's gate unconditionally 0 FOREVER, not
** merely cautious: every checkpoint holds slot 3 this way regardless of
** whether any external reader exists, so recycling could never happen at
** all. A SHARED hold, by contrast, does NOT exclude another connection also
** holding the same slot SHARED -- that case stays conservative (gate
** closed), which is the scenario shmHeld's own struct comment (above)
** documents ("someone -- possibly this connection -- holds a mark").
**
** Anything other than "we hold it exclusively" or a clean fresh
** try-exclusive success -- SQLITE_BUSY, any other error (proves nothing
** about safety either way), or a SHARED self-hold -- means "can't prove
** it's safe" -> gate closed (conservative failure, per the brief). A
** successful fresh try-exclusive is immediately unlocked, so this probe
** never disturbs SQLite's own lock state: the acquire+release pair is
** momentary and invisible to every other connection racing for the same
** slot. */
static int zvGateOk(ZvfsFile *p){
  if(!p->usedShm) return 1;
  sqlite3_file *real = p->pReal;
  for(int slot=3; slot<8; slot++){
    if(p->shmHeld[slot]==2) continue;     /* exclusive self-hold: proven safe */
    if(p->shmHeld[slot]==1) return 0;     /* shared self-hold: conservative */
    int rc = real->pMethods->xShmLock(real, slot, 1,
                SQLITE_SHM_LOCK|SQLITE_SHM_EXCLUSIVE);
    if(rc!=SQLITE_OK) return 0;
    real->pMethods->xShmLock(real, slot, 1,
                SQLITE_SHM_UNLOCK|SQLITE_SHM_EXCLUSIVE);
  }
  return 1;
}
/* Task 18: thunk adapting zvGateOk's ZvfsFile* signature to the void*-context
** callback container.c's commit_once expects (ZvfsContainer.gateProbe) --
** same ctx-cast idiom already used above for fio_read/fio_write/etc. Bound
** to each container right after it's built (zctr_set_gate_probe), so the
** pending-payload ceiling's re-probe (container.c) can call back into this
** same gate computation without container.c needing to know anything about
** ZvfsFile, shm, or locks. */
static int zvGateProbeThunk(void *ctx){ return zvGateOk((ZvfsFile*)ctx); }

/* mode detection: called from zvOpen for MAIN_DB.
**
** Task 16 amendment (spec Sec3/Sec7.5): probes for a valid CONTAINER header
** FIRST (zhdr_pick over the first 1024 bytes -- either copy, A or B, valid
** ==> CONTAINER), THEN the plain-db magic, THEN empty ==> UNDECIDED, else
** CORRUPT -- reordered from Task 11's original "magic first" probe. This
** matters specifically for recovering from a crash mid-conversion-commit
** (container.c's commit_once, when c->convert is set, writes header copy B
** at offset 512 FIRST, syncs, THEN copy A at offset 0, syncs -- see its own
** comment for the full crash-matrix argument): a crash after B lands but
** before/during A leaves the file's first 16 bytes (copy A's own region)
** still showing the ORIGINAL plain database's own "SQLite format 3" magic,
** completely untouched, even though a fully valid, newer-generation
** container header already sits durably at offset 512. Checking the plain
** magic first would misdetect that intermediate state as an ordinary (if
** subtly corrupted) plain database and silently ignore the container's own
** already-committed generation; checking zhdr_pick first correctly detects
** CONTAINER regardless of which single copy happens to be currently valid.
** For an ordinary, never-converted plain database, this reordering changes
** nothing observable: zhdr_pick requires an exact 16-byte magic match AND a
** CRC32 match over 508 bytes on the SAME probe, at either offset -- a false
** positive on genuine plain-db content is not realistically possible. */
/* Task 19 (found via full.test's ext/recover-adjacent backup_ioerr.test,
** not attributable to any established exclude category -- root-caused,
** not just documented): both header copies read back as entirely zero
** bytes -- neither a valid container header (zhdr_pick already failed)
** nor plain-db magic, yet distinguishable from ordinary corruption by
** being EXACTLY all-zero across the whole 1024-byte probe. A real header
** copy, valid or torn, always starts with either our own 16-byte magic or
** (pre-conversion) SQLite's plain one -- both non-zero -- so this exact
** shape can only arise from a virgin container whose first-ever commit
** never got as far as writing either header copy (zctr_sync could still
** have grown the physical file past the header block first, staging
** page/map records for the in-flight transaction -- ordinary and safe,
** per Sec5.1's invariant: nothing a still-unwritten header references is
** ever referenced by anything, so those bytes are inert). Confirmed via
** isolated repro (sqlite3_backup's own two-phase CommitPhaseOne(bCommit=0)
** + CommitPhaseTwo sequence, unlike an ordinary single-phase SQL COMMIT,
** can interleave a transient injected write failure between the moment
** the file physically grows and the moment either header copy is written)
** that a freshly-reopened connection over exactly this leftover shape
** used to hit ZVFS_MODE_CORRUPT permanently, even though nothing was ever
** actually committed to lose -- a plain SQLite db in the same situation
** rolls back via its journal to "nothing happened" instead. Treating this
** shape identically to the sz==0 case below (UNDECIDED -- becomes a
** container on the next write) is exactly that: xFileSize/xRead already
** report logical-empty for UNDECIDED regardless of physical bytes left
** over (see their own switch cases), and the allocator's ordinary
** best-fit/extend placement on the next real commit is free to reuse or
** grow past this dead space precisely because no live header references
** it. */
static int hdr_block_all_zero(const u8 *buf){
  for(int i=0; i<2*ZVFS_HDR_COPY_SIZE; i++) if(buf[i]) return 0;
  return 1;
}

static int detect_mode(ZvfsFile *p){
  i64 sz; int rc = p->pReal->pMethods->xFileSize(p->pReal, &sz);
  if(rc) return rc;
  if(sz==0){ p->mode = ZVFS_MODE_UNDECIDED; return SQLITE_OK; }
  u8 buf[1024];
  rc = p->pReal->pMethods->xRead(p->pReal, buf, sizeof(buf), 0);
  if(rc!=SQLITE_OK && rc!=SQLITE_IOERR_SHORT_READ) return rc;
  if(rc==SQLITE_OK){
    ZvfsHdr hdr; int which;
    if(zhdr_pick(buf, buf+ZVFS_HDR_COPY_SIZE, &hdr, &which)==SQLITE_OK){
      p->mode = ZVFS_MODE_CONTAINER;
      rc = zctr_open(&p->pCtr, file_io(p->pReal));
      /* Task 18: bind the gate-probe hook the instant a container exists,
      ** so a subsequent commit's pending-cap ceiling (container.c's
      ** commit_once) can always re-probe -- this handle may go on to be
      ** used under WAL regardless of the mode it happened to be opened
      ** under. */
      if(rc==SQLITE_OK) zctr_set_gate_probe(p->pCtr, zvGateProbeThunk, p);
      return rc;
    }
    if(hdr_block_all_zero(buf)){ p->mode = ZVFS_MODE_UNDECIDED; return SQLITE_OK; }
  }
  if(memcmp(buf, SQLITE_DB_MAGIC, 16)==0){ p->mode = ZVFS_MODE_PASSTHROUGH; return SQLITE_OK; }
  sqlite3_log(SQLITE_IOERR, "zstdvfs: unrecognized main-db file format");
  p->mode = ZVFS_MODE_CORRUPT;
  return SQLITE_OK;                    /* fail on first IO, not at open */
}

static int zvClose(sqlite3_file *f){
  ZvfsFile *p = (ZvfsFile*)f;
  if(p->pCtr){
    /* Fix round 4: close is the other place a write transaction can end
    ** (e.g. sqlite3_close() tearing down a handle without an intervening
    ** unlock through zvUnlock's own path) -- same structural guarantee as
    ** zvUnlock's own new step, explicit here too rather than relying only
    ** on zctr_close's own internal handling, so both transaction-end
    ** boundaries visibly enforce it the same way. zctr_rebuild_abort is a
    ** no-op if there is nothing to discard (including because zctr_close
    ** itself would otherwise be about to do the same thing internally).
    ** Task 16: this already covers a plain-db conversion left in flight
    ** too (p->pCtr is set whenever p->converting is, regardless of
    ** p->mode) -- zctr_close unconditionally frees the whole container
    ** object (map/alloc/scratch buffers/any staged rebuild state), which
    ** is exactly the "discard wholesale" a converting container needs
    ** (unlike an ordinary CONTAINER-mode close, it never had a committed
    ** generation to preserve). No p->converting reset needed here: the
    ** ZvfsFile itself is being torn down. */
    zctr_rebuild_abort(p->pCtr);
    zctr_close(p->pCtr);
  }
  int rc = REAL(f)->pMethods ? REAL(f)->pMethods->xClose(REAL(f)) : SQLITE_OK;
  return rc;
}
static int zvRead(sqlite3_file *f, void *b, int n, i64 off){
  ZvfsFile *p = (ZvfsFile*)f;
  switch(p->mode){
    case ZVFS_MODE_CONTAINER: return zctr_read(p->pCtr, b, n, off);
    case ZVFS_MODE_UNDECIDED: memset(b, 0, (size_t)n); return SQLITE_IOERR_SHORT_READ;
    case ZVFS_MODE_CORRUPT:   return SQLITE_IOERR_READ;
    default: return REAL(f)->pMethods->xRead(REAL(f), b, n, off);
  }
}
static int zvWrite(sqlite3_file *f, const void *b, int n, i64 off){
  ZvfsFile *p = (ZvfsFile*)f;
  switch(p->mode){
    case ZVFS_MODE_CONTAINER: return zctr_write(p->pCtr, b, n, off);
    case ZVFS_MODE_UNDECIDED: {
      /* First write ever to a fresh (zero-length) main db. SQLite's pager
      ** writes exactly one full page per xWrite call, but its cache can
      ** spill any dirty page to disk once a transaction's working set
      ** exceeds cache capacity -- a large single-transaction bulk insert
      ** routinely spills a page other than page 1 first. zctr_create
      ** doesn't need to know the offset up front, and zctr_write derives
      ** the container's page size from n (see its own comment), which
      ** works regardless of which page arrives first. */
      int rc = zctr_create(&p->pCtr, file_io(p->pReal));
      if(rc) return rc;
      zctr_set_gate_probe(p->pCtr, zvGateProbeThunk, p);  /* Task 18 */
      p->mode = ZVFS_MODE_CONTAINER;
      return zctr_write(p->pCtr, b, n, off);
    }
    case ZVFS_MODE_CORRUPT: return SQLITE_IOERR_WRITE;
    case ZVFS_MODE_PASSTHROUGH:
      if(p->converting){
        /* Task 16: route to the conversion container's rebuild path,
        ** exactly like an ordinary OVERWRITE rebuild write on an
        ** already-converted container (zctr_write dispatches to
        ** zctr_rebuild_write since c->rebuild==1, set by
        ** zctr_create_for_convert). Task 19 ("REBUILD ACCEPTANCE v3"):
        ** every geometrically well-formed write is now accepted
        ** unconditionally, in any arrival order (see container.c's own
        ** comment on struct ZvfsRebuild) -- there is no more mid-write
        ** "this doesn't belong to the stream" outcome to special-case
        ** here (the pre-v3 ZVFS_CONVERT_ABORTED sentinel this replaced).
        ** An incomplete or abandoned conversion attempt is instead caught
        ** later, at the sync/unlock transaction boundary -- zvSync/
        ** zvUnlock's own zctr_has_committed-driven discard-to-passthrough
        ** logic below, unchanged by v3 and already independent of
        ** anything happening at individual write time. */
        return zctr_write(p->pCtr, b, n, off);
      }
      return REAL(f)->pMethods->xWrite(REAL(f), b, n, off);
    default: return REAL(f)->pMethods->xWrite(REAL(f), b, n, off);
  }
}
static int zvTruncate(sqlite3_file *f, i64 sz){
  ZvfsFile *p = (ZvfsFile*)f;
  if(p->mode==ZVFS_MODE_CONTAINER) return zctr_truncate(p->pCtr, sz);
  if(p->mode==ZVFS_MODE_PASSTHROUGH && p->converting) return zctr_truncate(p->pCtr, sz);
  return REAL(f)->pMethods->xTruncate(REAL(f), sz);
}
/* Task 16: while converting (PASSTHROUGH mode, p->converting set), a
** successful commit here is what actually completes the conversion -- see
** zctr_has_committed's own comment (container.c) for why this checks that,
** rather than zctr_sync's own return code, to decide whether to switch to
** CONTAINER-mode dispatch: a torn-flip-safe conversion commit writes header
** copy B then copy A (container.c's commit_once), so a genuine I/O error
** specifically on the second write can leave zctr_sync reporting failure
** even though the container has already durably adopted copy B's committed
** generation in memory. From that point on this container, not the real
** file's raw bytes, is the authoritative representation -- staying in
** PASSTHROUGH would let a later read on this same connection see a torn
** mix of already-overwritten container-header bytes and untouched plain
** bytes instead of correct logical content. */
static int zvSync(sqlite3_file *f, int flags){
  ZvfsFile *p = (ZvfsFile*)f;
  if(p->mode==ZVFS_MODE_CONTAINER)
    /* Task 18: computed WAL reader gate, replacing the constant gateOk=1
    ** that was correct only under rollback-journal/EXCLUSIVE (where a sync
    ** is always the sole writer's commit point) and unsound under WAL, once
    ** a second connection can hold an open read transaction against an
    ** older container generation. See zvGateOk's own comment above. */
    return zctr_sync(p->pCtr, flags, /*gateOk=*/zvGateOk(p));
  if(p->mode==ZVFS_MODE_PASSTHROUGH && p->converting){
    if(!zctr_is_dirty(p->pCtr)){
      /* Nothing was ever staged into the conversion's rebuild stream (e.g.
      ** an early-failing VACUUM copy) -- there is no container commit to
      ** make, and this virgin container has no committed generation of
      ** its own to fall back to. Do NOT flip to CONTAINER mode on the
      ** strength of an ordinary, no-op physical sync: every future read
      ** must keep seeing the plain database's own still-fully-intact
      ** bytes, not an empty, uncommitted container. */
      return REAL(f)->pMethods->xSync(REAL(f), flags);
    }
    /* Task 18: computed here too, for uniformity with every other commit
    ** call site in this file, even though this specific path is reachable
    ** only via SQLITE_FCNTL_OVERWRITE on a rollback-journal commit (spec
    ** Sec7.4: WAL-mode VACUUM goes through WAL frames and never reaches
    ** here) -- p->usedShm is therefore always 0 on this path, so zvGateOk
    ** always returns 1 here, identical to the constant this replaces; using
    ** the real computation rather than a hand-proved-equal constant means
    ** this stays correct by construction rather than by an invariant
    ** enforced only by convention elsewhere in the codebase. */
    int rc = zctr_sync(p->pCtr, flags, /*gateOk=*/zvGateOk(p));
    if(zctr_has_committed(p->pCtr)){
      p->mode = ZVFS_MODE_CONTAINER; p->converting = 0;
    }else{
      /* Task 16 fix (coordinator-ruled, sub-case B): rc is necessarily
      ** non-OK here -- zctr_sync only returns SQLITE_OK once commit_once's
      ** success path has adopted a real header (zctr_has_committed would
      ** be true) -- covering both a genuine I/O failure before copy B ever
      ** became durable and the incomplete-or-inconsistent-rebuild path
      ** (container.c's zctr_sync, rebuild_check_complete's !complete
      ** case -- Task 19's "REBUILD ACCEPTANCE v3" completeness gate,
      ** corrected after review to always fail loudly here rather than
      ** silently discarding, convert or not: see zctr_sync's own comment
      ** for why a silent same-content fallback is not actually safe once
      ** SQLite itself believes the rebuild succeeded). Either way,
      ** this container never became the authoritative representation of
      ** anything: it must be discarded IMMEDIATELY, not left attached to
      ** p->pCtr/p->converting for a later transaction-end boundary
      ** (zvUnlock/zvClose) to eventually clean up. The write transaction
      ** is very likely still open at this exact point (SQLite's own
      ** rollback machinery can issue further xWrite calls -- e.g. hot-
      ** journal replay -- on this SAME connection before it ever drops the
      ** lock), and c->rebuild is already 0 by now (zctr_sync_rebuild
      ** clears it unconditionally right after calling commit_once,
      ** success or failure) -- so any such write would otherwise dispatch
      ** through zvWrite's converting branch into container.c's ORDINARY
      ** zctr_write path on a virgin, never-committed container: pageSize
      ** derives from the write's own amt, and (whichHdr still -1)
      ** zctr_sync_abort's own zctr_reset_empty branch has already reset
      ** c->alloc to a fresh zalloc_new() -- eof at ZVFS_HDR_BLOCK_SIZE, NOT
      ** the write's own logical offset -- so the next allocation lands
      ** THERE regardless of what offset the write itself asked for,
      ** INSIDE the plain database's own still-live bytes (verified
      ** directly: test/integration/test_convert.c's raw VFS-level
      ** regression), actively corrupting exactly what ordinary passthrough
      ** reads are still serving to every other reader of this same
      ** connection.
      ** Discarding here (mode stays PASSTHROUGH, p->pCtr/p->converting
      ** both cleared) routes every subsequent write on this connection to
      ** ordinary REAL(f) passthrough delegation instead -- correct,
      ** since the plain database's own bytes were never touched by the
      ** conversion's append-only stream in the first place. */
      zctr_close(p->pCtr);
      p->pCtr = 0;
      p->converting = 0;
    }
    return rc;
  }
  return REAL(f)->pMethods->xSync(REAL(f), flags);
}
static int zvFileSize(sqlite3_file *f, i64 *pSz){
  ZvfsFile *p = (ZvfsFile*)f;
  switch(p->mode){
    case ZVFS_MODE_CONTAINER: *pSz = zctr_logical_size(p->pCtr); return SQLITE_OK;
    case ZVFS_MODE_UNDECIDED: *pSz = 0; return SQLITE_OK;
    case ZVFS_MODE_PASSTHROUGH:
      if(p->converting){ *pSz = zctr_logical_size(p->pCtr); return SQLITE_OK; }
      return REAL(f)->pMethods->xFileSize(REAL(f), pSz);
    default: return REAL(f)->pMethods->xFileSize(REAL(f), pSz);
  }
}
/* Cache coherence across connections (§6.1): cached container state --
** header/txn, page map, decompressed page-1/scratch buffers -- is trustworthy
** only while this handle holds >= SHARED. Delegate the real lock first; only
** on success (the OS lock is what actually excludes/admits concurrent
** access) does a NONE->(>=SHARED) transition on a main-db handle mean "we
** may have been idle while another connection committed": a plain file can
** only have *become* a container in the meantime (never the reverse), so
** UNDECIDED/PASSTHROUGH re-run detect_mode; an existing container
** re-reads its header via zctr_revalidate, which resets the map/allocator
** caches iff the on-disk txn actually moved. lockLevel is updated to the
** newly-held level regardless of whether that re-detection/revalidation
** itself succeeded, since it mirrors the real OS lock state, not
** transaction status -- a failure here is reported to the caller (which
** will typically clean up via zvUnlock, correctly delegating from the
** lockLevel this just recorded). */
static int zvLock(sqlite3_file *f, int lk){
  ZvfsFile *p = (ZvfsFile*)f;
  int rc = REAL(f)->pMethods->xLock(REAL(f), lk);
  if(rc==SQLITE_OK){
    if((p->openFlags & SQLITE_OPEN_MAIN_DB) &&
       p->lockLevel==SQLITE_LOCK_NONE && lk>=SQLITE_LOCK_SHARED){
      if(p->mode==ZVFS_MODE_CONTAINER){
        rc = zctr_revalidate(p->pCtr);
      }else if(p->mode==ZVFS_MODE_UNDECIDED || p->mode==ZVFS_MODE_PASSTHROUGH){
        rc = detect_mode(p);
      }
    }
    p->lockLevel = lk;
  }
  return rc;
}
/* Controller ruling (Task 13 Part B, root-caused in Task 11): PRAGMA
** synchronous=OFF makes SQLite's pager skip xSync on the main db entirely at
** commit (pager_commit_phase_one guards sqlite3PagerSync() behind
** `if(!noSync)`), so the container's own commit point -- the A/B header flip
** in zctr_sync, which is what makes a write visible to any other connection
** -- never runs, and a committed transaction is lost on close. Fix: commit
** at transaction end via the unlock path instead, since xUnlock (unlike
** xSync) is never skippable. When a container-mode main-db handle's lock
** drops from >=RESERVED to <=SHARED (write transaction ending, commit or
** rollback-that-wrote-restore-content alike -- zctr_write can't tell the
** difference and doesn't need to) and the container has staged, unsynced
** changes (zctr_is_dirty), sync BEFORE delegating the unlock: the real file
** lock is still held at that point, which is what makes this connection
** still the exclusive writer and the commit safe.
**
** This is safe ONLY because zvLock (above) guarantees revalidation on every
** lock acquisition: this connection's in-memory state is provably the
** current committed generation as of when its write transaction began, so
** zctr_sync's txn=hdr.txn+1 can't silently clobber a newer commit some other
** connection made in the meantime. Task 11's own attempt at this exact hook,
** built before revalidation existed, reproduced concrete corruption
** (vacuum.test, tkt2565.test regressions -- see task-11-report.md and the
** git history of this comment in run_suite.sh) for exactly that reason.
**
** With synchronous!=OFF, xSync already ran and committed (dirty==0) by the
** time any unlock happens, so this is a pure no-op then -- never a double
** commit. zctr_sync itself always clears dirty (on success or via its own
** zctr_sync_abort on failure), so this fires at most once per write
** transaction.
**
** The real unlock is delegated unconditionally, even if the commit attempt
** failed: locks must keep working after an I/O error (e.g. so
** sqlite3_close() can still release the OS lock -- exercised directly by the
** crash harness, test/unit/test_crash.c, whose faultvfs deliberately leaves
** lock methods as unconditional passthrough post-crash for this reason).
**
** Fix round 4 (controller-ruled structural fix, closing a reactivation-wipe
** a reviewer traced through what round 3 had assessed as a harmless
** residual gap -- see container.c's zctr_rebuild_abort for the full
** mechanism): AFTER the commit-if-dirty step above, at this exact same
** write-transaction-end boundary, unconditionally discard any OVERWRITE
** rebuild state still attached to this container (zctr_rebuild_abort is a
** no-op when none is active). A rebuild that completed its own real commit
** already cleared c->rebuild as part of that commit, success or failure,
** before this point ever runs -- so this only ever does real work when a
** VACUUM's write transaction is ending WITHOUT ever having reached its own
** commit (an ordinary in-process rollback, or the copy simply never
** finishing), which is exactly the abandoned state that must never survive
** into whatever this same, possibly-reused connection does next.
**
** Task 16: the identical structural guarantee, extended to a plain-db
** CONVERSION in flight (mode STILL PASSTHROUGH -- p->converting is set
** instead, since a conversion only flips mode on its own successful commit,
** see zvSync). Unlike the CONTAINER-mode case above, a converting
** container has no committed generation of its own to leave partially
** intact -- if this write transaction is ending without ever completing
** the conversion, the whole scratch container is discardable, in full: an
** ordinary zctr_rebuild_abort here would only clear its in-flight rebuild
** sub-state while leaving a virgin, forever-uncommittable container object
** attached to p->pCtr, silently breaking every future read/write on this
** reused connection (mode wrongly stuck at PASSTHROUGH+converting with a
** container that can never be completed). Discard it wholesale --
** zctr_rebuild_abort then zctr_close, p->pCtr/p->converting both cleared --
** leaving a working, ordinary PASSTHROUGH handle over the plain database's
** own bytes, which were never touched by the conversion's append-only
** stream in the first place. As with the CONTAINER-mode commit-if-dirty
** step, a genuinely successful conversion's own commit already ran (via
** zvSync, under synchronous!=OFF) or runs right here (under
** synchronous=OFF, mirroring the container-mode case) before this boundary
** is reached -- see zctr_has_committed's own comment (container.c) for why
** the commit-completion check below is its return value, not zctr_sync's
** own rc: a torn-flip-safe conversion commit's second header write can
** fail for real even after its first (copy B) already landed durably. */
static int zvUnlock(sqlite3_file *f, int lk){
  ZvfsFile *p = (ZvfsFile*)f;
  int rc = SQLITE_OK;
  int txnEnding = (p->openFlags & SQLITE_OPEN_MAIN_DB) &&
                  p->lockLevel>=SQLITE_LOCK_RESERVED && lk<=SQLITE_LOCK_SHARED;
  int ctrEnding = txnEnding && p->mode==ZVFS_MODE_CONTAINER;
  int convEnding = txnEnding && p->mode==ZVFS_MODE_PASSTHROUGH && p->converting;
  /* Task 18: computed gate at both commit-if-dirty call sites below,
  ** replacing the constant gateOk=1 -- see zvSync's own comment for why
  ** that constant was unsound under WAL, and zvGateOk's own comment for
  ** the computation itself. */
  if(ctrEnding && zctr_is_dirty(p->pCtr)){
    rc = zctr_sync(p->pCtr, 0, /*gateOk=*/zvGateOk(p));
  }
  if(ctrEnding){
    zctr_rebuild_abort(p->pCtr);
  }
  if(convEnding){
    if(zctr_is_dirty(p->pCtr)){
      rc = zctr_sync(p->pCtr, 0, /*gateOk=*/zvGateOk(p));
      if(zctr_has_committed(p->pCtr)){ p->mode = ZVFS_MODE_CONTAINER; p->converting = 0; }
    }
    if(p->converting){
      zctr_rebuild_abort(p->pCtr);
      zctr_close(p->pCtr);
      p->pCtr = 0;
      p->converting = 0;
    }
  }
  int rcUnlock = REAL(f)->pMethods->xUnlock(REAL(f), lk);
  if(rcUnlock==SQLITE_OK) p->lockLevel = lk;
  return rc!=SQLITE_OK ? rc : rcUnlock;
}
static int zvCheckReservedLock(sqlite3_file *f, int *pOut){
  return REAL(f)->pMethods->xCheckReservedLock(REAL(f), pOut);
}
static int zvFileControl(sqlite3_file *f, int op, void *pArg){
  ZvfsFile *p = (ZvfsFile*)f;
  if(p->mode==ZVFS_MODE_CONTAINER){
    /* SIZE_HINT: the pager pre-extends the file to reduce fragmentation on
    ** the real filesystem; meaningless (and unsafe to forward -- it's a
    ** logical-size hint, not a physical one) once the container owns
    ** physical layout, so it's swallowed rather than delegated.
    ** PRAGMA: report "not handled here" so the pager falls back to its
    ** normal (non-VFS-specific) pragma handling. */
    if(op==SQLITE_FCNTL_SIZE_HINT) return SQLITE_OK;
    if(op==SQLITE_FCNTL_PRAGMA) return SQLITE_NOTFOUND;
    /* Task 15: SQLite sends this after opening a write transaction it
    ** intends to overwrite the entire file within (VACUUM's copy-back) --
    ** enter rebuild mode. Only ever fires on a rollback-journal commit
    ** under SQLite's own EXCLUSIVE lock (WAL-mode VACUUM goes through WAL
    ** frames and never sends this), so the container-level commit
    ** machinery's gateOk is inherently 1 for the whole rebuild. */
    if(op==SQLITE_FCNTL_OVERWRITE) return zctr_begin_overwrite(p->pCtr);
    /* Task 17 fix (real bug, root-caused against wal.c, not a shim gap
    ** anticipated by the plan): SQLite's own checkpoint routine
    ** (walCheckpoint in wal.c) only calls xSync on the main db file when a
    ** checkpoint backfills the WHOLE current WAL in one pass --
    ** `if(mxSafeFrame==walIndexHdr(pWal)->mxFrame){ ...xTruncate...;
    ** xSync...; }`. A PASSIVE checkpoint partially blocked by a concurrent
    ** reader's read-mark (the ordinary "backfill what you safely can,
    ** never block" contract -- exactly what
    ** test/integration/test_wal.c's snapshot-isolation step exercises)
    ** copies pages into the main db file via ordinary xWrite calls but
    ** skips BOTH the eventual xTruncate and the final xSync entirely.
    ** That is correct and sufficient for a plain flat-file VFS: a write()
    ** is immediately visible to any other reader of the SAME file without
    ** an intervening fsync (fsync there buys crash durability, not
    ** visibility) -- verified directly against vanilla SQLite (unix VFS,
    ** no shim) reproducing this exact scenario correctly with zero syncs
    ** in between.
    **
    ** Our container breaks that assumption: xWrite here (zctr_write) only
    ** ever STAGES a new page record; it becomes visible to any read --
    ** this same connection's own later reads after revalidation, and
    ** every other connection's reads -- only once the header is flipped
    ** (zctr_sync). A blocked-partial checkpoint that never reaches
    ** SQLite's own final xSync therefore left the checkpoint's freshly
    ** written pages permanently orphaned (unreferenced by the committed
    ** map), while SQLite still advances the wal-index's nBackfill counter
    ** for exactly the frames just written -- so a brand-new reader,
    ** needing no WAL frame to cover those pages, legitimately falls back
    ** to the main db file and saw the STALE, pre-checkpoint committed
    ** generation instead (reproduced directly: a fresh SELECT after the
    ** blocked reader's own COMMIT got "no such table" from a container
    ** still parked at its pre-CREATE-TABLE generation).
    **
    ** Fix: SQLITE_FCNTL_CKPT_DONE is SQLite's own dedicated notification
    ** for exactly this boundary -- fired unconditionally right after the
    ** checkpoint's write loop finishes, regardless of whether the
    ** backfill was full or partial, and explicitly BEFORE the wal-index's
    ** nBackfill is updated to reflect it (see its doc comment in
    ** sqlite3.h). Committing here, when dirty, closes the gap: the
    ** header -- and therefore every reader's fallback view of the main db
    ** file -- is durably updated before SQLite exposes those frames as
    ** "safe to skip in the WAL" to anyone. gateOk is computed (Task 18's
    ** zvGateOk, see its own comment above) rather than hardcoded: THIS is
    ** the call site where an overlapping reader's still-open snapshot most
    ** directly matters -- a checkpoint is precisely the moment our
    ** container might otherwise recycle extents that reader's cached root
    ** still references. SQLite discards this file control's return value
    ** unconditionally (walCheckpoint never assigns
    ** sqlite3OsFileControl(...,CKPT_DONE,0) to anything), so a genuine
    ** I/O failure here isn't specially surfaced through this path --
    ** but zctr_sync's own abort-reset-on-failure contract (Task 10) keeps
    ** the container internally consistent regardless: the checkpoint's
    ** effect simply isn't made durable, and the WAL frames (never
    ** discarded by a partial checkpoint) remain authoritative for every
    ** reader and for crash recovery. A later full/TRUNCATE checkpoint or
    ** ordinary commit on this same generation is a safe, idempotent no-op
    ** here (zctr_sync itself no-ops once c->dirty is already clear). */
    if(op==SQLITE_FCNTL_CKPT_DONE)
      return zctr_is_dirty(p->pCtr) ? zctr_sync(p->pCtr, 0, /*gateOk=*/zvGateOk(p)) : SQLITE_OK;
  }
  if(p->mode==ZVFS_MODE_PASSTHROUGH && op==SQLITE_FCNTL_OVERWRITE){
    /* Task 16: VACUUM's own copy-back fires this fcntl on ANY destination
    ** it is about to overwrite wholesale, regardless of whether that
    ** destination happens to already be one of our containers -- a plain
    ** db opened through zstdvfs gets the identical notification. Enter a
    ** from-scratch conversion: get the plain database's own current size
    ** (via the base VFS, not zctr_logical_size -- there is no container
    ** yet), then build a fresh rebuild-mode container whose allocator eof
    ** starts strictly past every byte of that plain image, so its
    ** append-only stream can never collide with the still-untouched plain
    ** bytes underneath. mode stays PASSTHROUGH (reads keep delegating to
    ** those intact plain bytes) until a successful commit (zvSync) flips
    ** it to CONTAINER. */
    i64 sz;
    int rc = REAL(f)->pMethods->xFileSize(REAL(f), &sz);
    if(rc) return rc;
    rc = zctr_create_for_convert(&p->pCtr, file_io(p->pReal), (u64)sz);
    if(rc) return rc;
    zctr_set_gate_probe(p->pCtr, zvGateProbeThunk, p);  /* Task 18 */
    p->converting = 1;
    return SQLITE_OK;
  }
  int rc = REAL(f)->pMethods->xFileControl(REAL(f), op, pArg);
  if(op==SQLITE_FCNTL_VFSNAME && rc==SQLITE_OK){
    char *z = sqlite3_mprintf("zstdvfs/%z", *(char**)pArg);
    *(char**)pArg = z;
  }
  return rc;
}
static int zvSectorSize(sqlite3_file *f){
  ZvfsFile *p = (ZvfsFile*)f;
  if(p->mode==ZVFS_MODE_CONTAINER) return 4096;
  return REAL(f)->pMethods->xSectorSize(REAL(f));
}
static int zvDeviceCharacteristics(sqlite3_file *f){
  ZvfsFile *p = (ZvfsFile*)f;
  int base = REAL(f)->pMethods->xDeviceCharacteristics(REAL(f));
  if(p->mode==ZVFS_MODE_CONTAINER){
    /* The container's commit protocol (container.c: data barrier, then
    ** header flip, then flip barrier) is what actually makes writes
    ** power-safe -- none of that depends on the real filesystem doing
    ** atomic sector writes, so strip every ATOMIC* / SAFE_APPEND /
    ** BATCH_ATOMIC bit the base VFS advertises (those describe guarantees
    ** about *physical* page-sized writes that no longer apply once pages
    ** are variable-length compressed records). POWERSAFE_OVERWRITE is
    ** true on its own terms: an in-place record rewrite never happens --
    ** true of ordinary writes and, still, of the Task 15 OVERWRITE rebuild
    ** path (its stream is pure append; the pack loop that densifies it
    ** afterward is ordinary COW relocation, not in-place either) -- so a
    ** torn physical write next to ours can never corrupt content we
    ** didn't touch. */
    return (base & ~(SQLITE_IOCAP_ATOMIC|SQLITE_IOCAP_ATOMIC512|SQLITE_IOCAP_ATOMIC1K|
                      SQLITE_IOCAP_ATOMIC2K|SQLITE_IOCAP_ATOMIC4K|SQLITE_IOCAP_ATOMIC8K|
                      SQLITE_IOCAP_ATOMIC16K|SQLITE_IOCAP_ATOMIC32K|SQLITE_IOCAP_ATOMIC64K|
                      SQLITE_IOCAP_SAFE_APPEND|SQLITE_IOCAP_BATCH_ATOMIC))
           | SQLITE_IOCAP_POWERSAFE_OVERWRITE;
  }
  return base;
}
static int zvShmMap(sqlite3_file *f, int i, int sz, int ext, void volatile **pp){
  ZvfsFile *p = (ZvfsFile*)f;
  /* Task 18: xShmMap is WAL-only (a rollback-journal connection never calls
  ** it at all -- see zvShmLock's own comment below), so ITS OWN being
  ** called at all is the signal zvGateOk needs to tell "this handle is a
  ** WAL connection" apart from "this handle is rollback-journal, where the
  ** pager's own EXCLUSIVE lock already makes the gate trivially safe".
  ** Set unconditionally (before delegating), regardless of whether the map
  ** itself succeeds: even a failed attempt establishes that this is a WAL
  ** connection. */
  p->usedShm = 1;
  return REAL(f)->pMethods->xShmMap(REAL(f), i, sz, ext, pp);
}
/* Task 17 fix (real bug, root-caused, not a shim gap the plan anticipated):
** zvLock's own cache-coherence comment (see above) assumes every new
** transaction is preceded by a NONE->(>=SHARED) transition on the main-db
** file's OWN lock -- true for rollback-journal connections, which fully
** release that lock between transactions. It is FALSE for WAL-mode
** connections: verified directly (throwaway instrumented build) that a
** WAL reader's main-db file lock is acquired ONCE and held at SHARED
** across its entire session -- COMMIT and every subsequent BEGIN move
** exclusively through xShmLock on the wal-index's read-mark slots
** (WAL_READ_LOCK(0..WAL_NREADER-1) = shm lock indices 3..SQLITE_SHM_NLOCK-1;
** see wal.c's own "Index numbers for various locking bytes" comment --
** documented specifically for VFS/shim interop, the standard SQLITE_SHM_*
** flag values above are the public API for it), never touching xLock at
** all. zvLock's own revalidation therefore fires exactly once, at session
** open, and never again -- a reader's cached header/map/alloc state goes
** permanently stale the moment ANY other connection commits (e.g. a
** checkpoint backfill, see zvFileControl's SQLITE_FCNTL_CKPT_DONE comment
** for a second, compounding bug in the same test scenario) after that
** first transaction. Reproduced directly: a reader's fresh SELECT
** immediately after its own COMMIT (ending the snapshot that predated a
** writer's checkpoint) returned "no such table" -- the container was still
** parked at the pre-CREATE-TABLE generation.
**
** Fix, round 1 (incomplete -- see below): acquiring SHARED on any
** WAL_READ_LOCK slot is WAL's own signal that a new read snapshot is being
** established -- revalidate there, mirroring the semantics zvLock already
** uses the main-file NONE->SHARED edge for.
**
** That alone left a second, distinct hole, caught by mmap1.test (section 2:
** two connections, the second's own checkpoint reading pages the first's
** later checkpoint must revalidate against) and root-caused the same way:
** a CHECKPOINTER claims WAL_READ_LOCK(0) -- the same slot index an ordinary
** reader uses for the "read straight from the db file, skip the WAL"
** shortcut -- but does so with SQLITE_SHM_EXCLUSIVE, never SHARED (verified
** directly: instrumented build showed flags=10 i.e. LOCK|EXCLUSIVE for a
** checkpoint's own acquisition, vs. flags=6 i.e. LOCK|SHARED for an
** ordinary reader's). A SHARED-only filter therefore revalidates every
** plain reader but never a checkpointer about to run its own write loop --
** exactly backwards from what matters most, since the checkpointer is the
** one about to translate logical pgnos through c->map/c->alloc into
** physical writes. Reproduced directly: a second connection's checkpoint,
** issued after a first connection's own checkpoint plus writes had moved
** the map root, built its write loop on the FIRST connection's now-stale
** cached root -- landing on a physical offset a later generation had
** already reused for something else ("zstdvfs: bad map node", a real
** structural read failure, not a false-positive check).
**
** Final fix: match on SQLITE_SHM_LOCK alone (both SHARED and EXCLUSIVE),
** never on SQLITE_SHM_UNLOCK, still restricted to the WAL_READ_LOCK range
** (off>=3) so WAL_WRITE_LOCK(0)/WAL_CKPT_LOCK(1)/WAL_RECOVER_LOCK(2) --
** none of which mark a read-transaction or checkpoint-backfill boundary --
** are left alone. This also revalidates on the checkpoint's own upfront
** "steal every other reader's mark" EXCLUSIVE loop (WAL_READ_LOCK(1..N-1));
** redundant with the WAL_READ_LOCK(0) acquisition that follows it, but
** harmless (zctr_revalidate no-ops once c->hdr.txn already matches the
** on-disk header) and left in rather than special-cased out. Cheap: fires
** a handful of times per read-transaction-start or per checkpoint, not once
** per page. Never fires for a rollback-journal connection (xShmMap/
** xShmLock are WAL-only; a non-WAL connection never calls this at all).
**
** Task 18 reconciliation: the brief's own "reader-side revalidation" item
** (spec Sec6.2's last paragraph -- "on acquiring a read mark, revalidate
** the header txn counter; drop caches if it moved") asks for revalidation
** on a successful SHARED acquire of any WAL_READ_LOCK slot. That is a
** strict SUBSET of the fix already landed above (which, per the mmap1-2.4
** regression trace, correctly also covers a checkpointer's own EXCLUSIVE
** acquisition of the very same slots) -- there is exactly one revalidation
** hook here, not two: the existing `(flags & SQLITE_SHM_LOCK) && off>=3`
** condition below already fires for the brief's own narrower case, so nothing
** new is added for it. What Task 18 DOES add at this same call site is
** shmHeld[] bookkeeping (below), independent of revalidation, consumed by
** zvGateOk (above) to know which read-mark slots this handle itself
** currently holds -- so the gate never tries to contend with its own lock
** and is conservative about a slot held for a reason it can't itself
** distinguish (e.g. mid-checkpoint on this very connection). */
static int zvShmLock(sqlite3_file *f, int off, int n, int flags){
  ZvfsFile *p = (ZvfsFile*)f;
  int rc = REAL(f)->pMethods->xShmLock(REAL(f), off, n, flags);
  if(rc==SQLITE_OK){
    /* Task 18: track which of the 8 wal-index lock slots (SQLITE_SHM_NLOCK)
    ** this handle currently holds, and which kind (shmHeld's own struct
    ** comment above explains why the kind matters to zvGateOk) -- a
    ** lock/unlock call can span more than one slot in one shot (e.g. wal.c's
    ** own checkpoint-start walLockExclusive(WAL_READ_LOCK(1), WAL_NREADER-1)
    ** steals several read marks in a single xShmLock call), so mark/clear
    ** every slot in [off,off+n). SQLITE_SHM_LOCK and SQLITE_SHM_UNLOCK are
    ** the only two flag bits SQLite ever passes here (never both at once,
    ** never neither) -- see sqlite3.h's own SQLITE_SHM_* doc comment. */
    u8 v = (flags & SQLITE_SHM_LOCK) ? ((flags & SQLITE_SHM_EXCLUSIVE) ? 2 : 1) : 0;
    for(int i=off; i<off+n && i<8; i++) p->shmHeld[i] = v;
    if(p->mode==ZVFS_MODE_CONTAINER &&
       (flags & SQLITE_SHM_LOCK) && off>=3 /* WAL_READ_LOCK(0), wal.c */){
      rc = zctr_revalidate(p->pCtr);
    }
  }
  return rc;
}
static void zvShmBarrier(sqlite3_file *f){
  REAL(f)->pMethods->xShmBarrier(REAL(f));
}
static int zvShmUnmap(sqlite3_file *f, int del){
  return REAL(f)->pMethods->xShmUnmap(REAL(f), del);
}
static int zvFetch(sqlite3_file *f, i64 off, int n, void **pp){
  ZvfsFile *p = (ZvfsFile*)f;
  if(p->mode==ZVFS_MODE_CONTAINER){ *pp = 0; return SQLITE_OK; } /* forces xRead fallback */
  return REAL(f)->pMethods->xFetch(REAL(f), off, n, pp);
}
static int zvUnfetch(sqlite3_file *f, i64 off, void *pPage){
  ZvfsFile *p = (ZvfsFile*)f;
  if(p->mode==ZVFS_MODE_CONTAINER) return SQLITE_OK;
  return REAL(f)->pMethods->xUnfetch(REAL(f), off, pPage);
}

static const sqlite3_io_methods zvfs_io_methods = {
  3, zvClose, zvRead, zvWrite, zvTruncate, zvSync, zvFileSize,
  zvLock, zvUnlock, zvCheckReservedLock, zvFileControl,
  zvSectorSize, zvDeviceCharacteristics,
  zvShmMap, zvShmLock, zvShmBarrier, zvShmUnmap, zvFetch, zvUnfetch
};

static int zvOpen(sqlite3_vfs *pVfs, sqlite3_filename zName,
                  sqlite3_file *pFile, int flags, int *pOutFlags){
  sqlite3_vfs *pBase = BASEVFS(pVfs);
  ZvfsFile *p = (ZvfsFile*)pFile;
  memset(p, 0, sizeof(*p));
  p->pReal = (sqlite3_file*)&p[1];
  p->pBaseVfs = pBase;
  p->openFlags = flags;
  p->mode = ZVFS_MODE_PASSTHROUGH;
  int rc = pBase->xOpen(pBase, zName, p->pReal, flags, pOutFlags);
  if(rc!=SQLITE_OK) return rc;
  if(flags & SQLITE_OPEN_MAIN_DB){
    rc = detect_mode(p);
    if(rc!=SQLITE_OK){
      if(p->pReal->pMethods) p->pReal->pMethods->xClose(p->pReal);
      return rc;
    }
  }
  p->base.pMethods = &zvfs_io_methods;
  return SQLITE_OK;
}
static int zvDelete(sqlite3_vfs *v, const char *z, int sync){
  return BASEVFS(v)->xDelete(BASEVFS(v), z, sync);
}
static int zvAccess(sqlite3_vfs *v, const char *z, int f, int *pOut){
  return BASEVFS(v)->xAccess(BASEVFS(v), z, f, pOut);
}
static int zvFullPathname(sqlite3_vfs *v, const char *z, int n, char *zOut){
  return BASEVFS(v)->xFullPathname(BASEVFS(v), z, n, zOut);
}
static void *zvDlOpen(sqlite3_vfs *v, const char *z){
  return BASEVFS(v)->xDlOpen(BASEVFS(v), z);
}
static void zvDlError(sqlite3_vfs *v, int n, char *z){
  BASEVFS(v)->xDlError(BASEVFS(v), n, z);
}
static void (*zvDlSym(sqlite3_vfs *v, void *p, const char *z))(void){
  return BASEVFS(v)->xDlSym(BASEVFS(v), p, z);
}
static void zvDlClose(sqlite3_vfs *v, void *p){
  BASEVFS(v)->xDlClose(BASEVFS(v), p);
}
static int zvRandomness(sqlite3_vfs *v, int n, char *z){
  return BASEVFS(v)->xRandomness(BASEVFS(v), n, z);
}
static int zvSleep(sqlite3_vfs *v, int us){
  return BASEVFS(v)->xSleep(BASEVFS(v), us);
}
static int zvCurrentTime(sqlite3_vfs *v, double *p){
  return BASEVFS(v)->xCurrentTime(BASEVFS(v), p);
}
static int zvGetLastError(sqlite3_vfs *v, int n, char *z){
  return BASEVFS(v)->xGetLastError(BASEVFS(v), n, z);
}
static int zvCurrentTimeInt64(sqlite3_vfs *v, sqlite3_int64 *p){
  return BASEVFS(v)->xCurrentTimeInt64(BASEVFS(v), p);
}

/* Process-permanent registry of built ZvfsVfs blocks, keyed by name, kept
** entirely outside SQLite's own VFS list (sqlite3_vfs_find/vfsList). These
** blocks are registry metadata: by design they must outlive every SQLite
** lifecycle, exactly like the static structs backing the built-in "unix"
** VFS -- SQLITE_EXTRA_INIT (our test-suite entry point,
** test/suite/extra_init.c) reruns on every sqlite3_shutdown() followed by
** sqlite3_initialize() within the same process (documented core behavior:
** src/main.c clears isInit on shutdown and reruns bRunExtraInit on the
** next initialize), and many test files perform exactly that cycle. The
** registry exists so those repeat registrations reuse the same permanent,
** system-malloc'd block per name -- allocation-free and bounded by the
** number of distinct names ever registered -- rather than building a new
** one every cycle. (sqlite3_shutdown() does NOT clear vfsList -- checked
** against src/main.c, it never touches it -- so this isn't working around
** that; it's simply that "already registered" is a property of *this*
** name, not of vfsList's current contents, and the registry is where that
** property actually lives.)
**
** Because these blocks are never freed, they must NOT be allocated with
** sqlite3_malloc() -- that would make SQLite's own memstatus count them
** as permanently "used" heap, which is exactly what the built-in VFS
** objects (plain static structs, never sqlite3_malloc'd) never do.
** Allocated with the plain system allocator instead, for the same reason
** those statics are invisible to sqlite3_memory_used().
**
** zvfsRegistry is guarded by a static mutex (SQLITE_MUTEX_STATIC_APP1, a
** slot SQLite reserves for exactly this kind of application use -- no
** lazy-init race) since zstdvfs_register() is a public entry point (also
** called from ext/zstdvfs_ext.c's extension loader) that SQLite does not
** itself serialize the way it does SQLITE_EXTRA_INIT. The whole
** find-or-build-and-register sequence in zstdvfs_register() below runs
** under one critical section, not find-then-separately-add: two threads
** racing to register the same never-before-seen name must not both miss
** the lookup and each build+add their own block (that would leave two
** same-named blocks in the registry *and* two same-named entries in
** SQLite's vfsList -- os.c links by pointer, not name, so it never
** deduplicates that -- making sqlite3_vfs_find(zName) nondeterministic
** and orphaning one of the two blocks). The build step is a single small
** malloc done only at registration time, so holding the mutex across it
** is cheap. sqlite3_vfs_register() is called while still holding this
** mutex too: it (and sqlite3_vfs_find(), also called in here) takes
** SQLITE_MUTEX_STATIC_MAIN internally, a different static mutex slot than
** APP1, and nothing in SQLite ever acquires APP1 -- it's reserved for
** application use -- so there is no code path that acquires STATIC_MAIN
** and then tries to acquire APP1, and therefore no lock-ordering cycle.
*/
static ZvfsVfs *zvfsRegistry = 0;

int zstdvfs_register(const char *zName, const char *zBaseVfs, int makeDefault){
  sqlite3_mutex *m = sqlite3_mutex_alloc(SQLITE_MUTEX_STATIC_APP1);
  ZvfsVfs *p;
  int rc;

  sqlite3_mutex_enter(m);
  for(p=zvfsRegistry; p; p=p->pRegNext){
    if(strcmp(p->base.zName, zName)==0) break;
  }
  if(!p){
    sqlite3_vfs *pBase = sqlite3_vfs_find(zBaseVfs);
    if(!pBase){ sqlite3_mutex_leave(m); return SQLITE_ERROR; }
    size_t nName = strlen(zName)+1;
    p = malloc(sizeof(*p)+nName);
    if(!p){ sqlite3_mutex_leave(m); return SQLITE_NOMEM; }
    memset(p, 0, sizeof(*p));
    char *zCopy = (char*)&p[1];
    memcpy(zCopy, zName, nName);
    p->base.iVersion = 2;
    p->base.szOsFile = (int)sizeof(ZvfsFile) + pBase->szOsFile;
    p->base.mxPathname = pBase->mxPathname;
    p->base.zName = zCopy;
    p->base.pAppData = (void*)pBase;
    p->base.xOpen = zvOpen;
    p->base.xDelete = zvDelete;
    p->base.xAccess = zvAccess;
    p->base.xFullPathname = zvFullPathname;
    p->base.xDlOpen = zvDlOpen;
    p->base.xDlError = zvDlError;
    p->base.xDlSym = zvDlSym;
    p->base.xDlClose = zvDlClose;
    p->base.xRandomness = zvRandomness;
    p->base.xSleep = zvSleep;
    p->base.xCurrentTime = zvCurrentTime;
    p->base.xGetLastError = zvGetLastError;
    p->base.xCurrentTimeInt64 = zvCurrentTimeInt64;
    p->pRegNext = zvfsRegistry;
    zvfsRegistry = p;
  }
  /* Re-assert makeDefault unconditionally, even on a cache hit: a prior
  ** call may have registered without it. Allocation-free either way (see
  ** above), and safe to call while still holding m (see above). */
  rc = sqlite3_vfs_register(&p->base, makeDefault);
  sqlite3_mutex_leave(m);
  return rc;
}
