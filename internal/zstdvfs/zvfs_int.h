#ifndef ZVFS_INT_H
#define ZVFS_INT_H
#include "sqlite3.h"
#include <stdint.h>
#include <string.h>
typedef uint8_t u8; typedef uint16_t u16; typedef uint32_t u32;
typedef uint64_t u64; typedef sqlite3_int64 i64;

#define ZVFS_GRANULE        64
#define ZVFS_HDR_COPY_SIZE  512
#define ZVFS_HDR_BLOCK_SIZE 4096
#define ZVFS_REC_HDR_SIZE   24
#define ZVFS_NODE_SIZE      4096
#define ZVFS_FANOUT         256
#define ZVFS_REC_MAGIC      0x5A56
/* Task 19 review item: overridable via -D so test/unit/test_lockhole.c can
** drive real container churn across a hole small enough to reach without
** bigsmoke.sh's multi-GB scale, exercising the exact tail-walk hazard
** compact.c's own zcompact_step comment describes (a highest-offset free
** extent landing exactly at the hole's own never-written bytes) under an
** ordinary unit-test budget. Production/default value unchanged. */
#ifndef ZVFS_LOCK_HOLE_OFF
#define ZVFS_LOCK_HOLE_OFF  0x40000000
#endif
#ifndef ZVFS_LOCK_HOLE_LEN
#define ZVFS_LOCK_HOLE_LEN  65536
#endif
#define ZVFS_LEVEL_WRITE    3
#define ZVFS_LEVEL_REBUILD  19
/* Task 18 carried-in mandatory item (controller rulings, Tasks 14/17): under
   a WAL reader gate that can legitimately stay closed (gateOk==0) across
   MANY consecutive commits (a long-lived overlapping reader), each blocked
   commit's own frees land in a NEW, never-released pending generation --
   gen_insert (alloc.c) only coalesces entries WITHIN one generation/txn, so
   nGen (and the serialized ZREC_PENDING payload) grows roughly linearly with
   the number of consecutive blocked commits. This is a DIFFERENT growth
   mechanism than the one Task 14 fixed for savepoint-6.3 (a single
   transaction's own uncoalesced churn, fixed by gen_insert + the
   release-right-after-zmap_commit ordering) -- that fix bounds ONE
   generation's own size; it does nothing to stop the COUNT of generations
   from growing when the gate itself stays shut for a long time. See
   commit_once's own comment (container.c) for the mitigation this cap
   drives: past this many bytes of accumulated pending payload, a commit
   forces a synchronous gate re-probe before trusting a closed gate, and
   logs (sqlite3_log) if the re-probe still finds it shut. Chosen smaller
   than the "~1MB" illustrative figure from the design conversation that
   introduced this requirement: large enough that no realistic SINGLE
   commit's own (already gen_insert-coalesced) pending churn crosses it
   spuriously, small enough that the accumulation path is exercised by a
   bounded, fast test (test/integration/test_pending_cap.c) instead of
   requiring an impractically long-held reader. */
#define ZVFS_PENDING_CAP    (8u*1024u)
#define ZVFS_MAGIC          "zstd-vfs-v1\x00\x00\x00\x00\x00"   /* 16 bytes */
#define SQLITE_DB_MAGIC     "SQLite format 3"                    /* 16 bytes incl NUL */

#define ZVFS_MODE_UNDECIDED   0
#define ZVFS_MODE_PASSTHROUGH 1
#define ZVFS_MODE_CONTAINER   2
#define ZVFS_MODE_CORRUPT     3

/* record types */
#define ZREC_PAGE 1
#define ZREC_NODE 2
#define ZREC_FREELIST 3
#define ZREC_PENDING 4
/* record flags */
#define ZF_RAW 0x01

static inline void put16le(u8 *p, u16 v){ p[0]=(u8)v; p[1]=(u8)(v>>8); }
static inline void put32le(u8 *p, u32 v){ put16le(p,(u16)v); put16le(p+2,(u16)(v>>16)); }
static inline void put64le(u8 *p, u64 v){ put32le(p,(u32)v); put32le(p+4,(u32)(v>>32)); }
static inline u16 get16le(const u8 *p){ return (u16)(p[0]|(p[1]<<8)); }
static inline u32 get32le(const u8 *p){ return get16le(p)|((u32)get16le(p+2)<<16); }
static inline u64 get64le(const u8 *p){ return get32le(p)|((u64)get32le(p+4)<<32); }
static inline u32 zvfs_gran_round(u32 n){ return (n + ZVFS_GRANULE-1) & ~(u32)(ZVFS_GRANULE-1); }
/* Task 16: zvfs_gran_round's 64-bit counterpart. Needed because
   zctr_create_for_convert rounds a plain database's own file SIZE (a u64 --
   real databases can exceed 4GiB), not a single record/extent length (a u32
   everywhere else in this codebase); the 32-bit version would silently
   truncate a large plainSize before rounding it. */
static inline u64 zvfs_gran_round64(u64 n){ return (n + ZVFS_GRANULE-1) & ~(u64)(ZVFS_GRANULE-1); }

typedef struct ZvfsHdr {
  u32 pageSize; u64 pageCount; u64 txn;
  u64 mapRoot, freeOff, pendOff, eof;
  u32 flags;
} ZvfsHdr;

typedef struct ZvfsRec { u8 type, flags; u32 nPayload; u64 key; u32 crc; } ZvfsRec;

u32 zcrc32(u32 seed, const void *buf, size_t n);   /* codec.c */
/* Compress one page. out must hold pgsz bytes. On no-shrink: *pRaw=1 and out
   holds the raw page (n=pgsz). zstd content checksum enabled. */
int zcodec_compress(const u8 *pg, u32 pgsz, u8 *out, u32 *pnOut, int level, int *pRaw);
/* Inverse. raw!=0 means payload is the raw page. Returns SQLITE_IOERR_READ on
   checksum/size mismatch (caller logs). */
int zcodec_decompress(const u8 *in, u32 nIn, int raw, u8 *pg, u32 pgsz);
void zhdr_encode(const ZvfsHdr*, u8 out[ZVFS_HDR_COPY_SIZE]);
int  zhdr_decode(const u8 in[ZVFS_HDR_COPY_SIZE], ZvfsHdr*);   /* OK | SQLITE_NOTADB */
/* pick valid copy with higher txn; *pWhich = 0(A)/1(B); NOTADB if neither */
int  zhdr_pick(const u8 *a, const u8 *b, ZvfsHdr *out, int *pWhich);
void zrec_encode(const ZvfsRec*, u8 out[ZVFS_REC_HDR_SIZE]);
int  zrec_decode(const u8 in[ZVFS_REC_HDR_SIZE], ZvfsRec*);  /* OK | SQLITE_IOERR_READ (bad magic/type) */
#define ZREC_TOTAL(nPayload) zvfs_gran_round(ZVFS_REC_HDR_SIZE + (nPayload))
static inline u64 zrec_node_key(u8 level, u32 firstPgno){ return ((u64)level<<56)|firstPgno; }
static inline u8  zrec_key_level(u64 k){ return (u8)(k>>56); }
static inline u32 zrec_key_pgno(u64 k){ return (u32)k; }

typedef struct ZExt { u64 off; u32 len; } ZExt;
typedef struct ZvfsAlloc ZvfsAlloc;
ZvfsAlloc *zalloc_new(void);                       /* eof starts at ZVFS_HDR_BLOCK_SIZE */
void zalloc_delete(ZvfsAlloc*);
int  zalloc_load(ZvfsAlloc*, const u8 *freePay, u32 nFree,
                 const u8 *pendPay, u32 nPend, u64 eof); /* OK | SQLITE_IOERR_READ */
u64  zalloc_take(ZvfsAlloc*, u32 nBytes);   /* granule-rounds; best-fit lowest-offset,
                                               else extends eof; skips the lock hole */
u64  zalloc_peek(ZvfsAlloc*, u32 nBytes);   /* offset best-fit WOULD return; 0 = would extend eof */
/* Task 19 fix (found via test_convert.c's stage-5 transient-fault sweep,
   root-caused rather than papered over): undo one zalloc_take(nBytes) call
   that returned `off`, for a caller whose own use of that extent (the
   record write it was reserved for) failed and never durably landed.
   Without this, a take followed by a failed write leaves a permanent
   "phantom gap" -- bytes the allocator considers consumed but that are
   neither a valid record nor tracked as free -- which a later compaction
   tail-walk (zcompact_step, compact.c) eventually tries to parse as a
   record and fails on for real (SQLITE_IOERR_READ, "bad record magic"):
   the same hazard class zalloc_reserve_eof's own lock-hole handling exists
   to avoid, see its comment. Symmetric with zalloc_take: if `off` is
   exactly the current tail (true whenever the original take extended eof
   -- always the case in append-only/rebuild mode, and often the case
   otherwise), simply retract eof, a true undo leaving no trace at all;
   otherwise (a best-fit take from fr[] that is no longer the tail) give it
   back to fr[] the ordinary way. Caller must pass the SAME nBytes given to
   the take call this reverses -- rounding is applied identically to both,
   and this must be called with nothing else having touched the allocator
   in between (true of every take-then-write call site in this codebase:
   the take and its record write are always immediately adjacent). */
void zalloc_untake(ZvfsAlloc*, u64 off, u32 nBytes);
u64  zalloc_extend(ZvfsAlloc*, u32 nBytes); /* always appends at eof; zalloc_take's fallback
                                               when fr[] has no fit (list records reach it that
                                               way -- see zctr_sync), and every zalloc_take call
                                               unconditionally while append-only (Task 15
                                               OVERWRITE rebuild stream -- see
                                               zalloc_set_appendonly) */
/* alloc.c addition: pre-cross the lock hole so the next nBytes of extends are pure
   appends (any skipped gap becomes an ordinary free extent NOW, keeping list
   serialization sizes stable) */
void zalloc_reserve_eof(ZvfsAlloc*, u32 nBytes);
void zalloc_free(ZvfsAlloc*, u64 off, u32 nBytes, u64 txn);  /* -> pending gen txn */
void zalloc_release(ZvfsAlloc*, u64 uptoTxn); /* gens <= uptoTxn -> free, coalesced */
/* Task 15 (OVERWRITE rebuild): while on, zalloc_take delegates to
   zalloc_extend unconditionally -- the rebuild stream must land strictly
   above the untouched, still-committed old eof (see zalloc_take's own
   comment in alloc.c). */
void zalloc_set_appendonly(ZvfsAlloc*, int on);
/* Task 15: the rebuild commit's allocator reset -- [freeFrom,freeTo) minus
   the lock hole becomes ONE PENDING generation tagged `txn` (the commit's
   own intended new txn -- not immediately reusable; see alloc.c's own
   comment for why release must wait, same two-generation discipline as
   zalloc_free), every prior pending generation is discarded outright (not
   released -- see alloc.c), and eof is set to the given value. Replaces
   §7.4's slide-down: what used to need bespoke chunk-move machinery is now
   just "describe the old generation as pending space," with the ordinary
   commit protocol's own step-0 release (once this commit's header is
   durable) and the incremental pack loop (zcompact_full) doing the actual
   densification via ordinary crash-safe commits. */
void zalloc_reset_span(ZvfsAlloc*, u64 freeFrom, u64 freeTo, u64 eof, u64 txn);
u64  zalloc_eof(const ZvfsAlloc*);
u64  zalloc_free_bytes(const ZvfsAlloc*);
u32  zalloc_free_count(const ZvfsAlloc*);
int  zalloc_free_at(const ZvfsAlloc*, u32 i, ZExt *out);  /* i-th free extent by offset */
int  zalloc_last_free(const ZvfsAlloc*, ZExt*);  /* highest-offset free extent; 0 if none */
int  zalloc_trim(ZvfsAlloc*);  /* pop trailing free extent(s) abutting eof; lowers eof;
                                   returns 1 if changed. Safe: fr[] holds only released
                                   extents (no live generation still references them), so
                                   lowering eof over them never discards anything live. */
u32  zalloc_ser_free_size(const ZvfsAlloc*);
u32  zalloc_ser_pend_size(const ZvfsAlloc*);
void zalloc_ser_free(const ZvfsAlloc*, u8 *buf);
void zalloc_ser_pend(const ZvfsAlloc*, u8 *buf);

/* physical-file IO vtable; real impl wraps sqlite3_file (Task 10), tests use memio */
typedef struct ZvfsIO {
  void *ctx;
  int (*xRead)(void *ctx, void *buf, u32 n, u64 off);      /* exact read or error */
  int (*xWrite)(void *ctx, const void *buf, u32 n, u64 off);
  int (*xTruncate)(void *ctx, u64 size);
  int (*xSync)(void *ctx);
  int (*xFileSize)(void *ctx, u64 *pSize);
} ZvfsIO;

/* record IO (container.c): caller allocates via ZvfsAlloc */
int zctr_write_record(const ZvfsIO*, u64 off, const ZvfsRec*, const u8 *payload);
int zctr_read_rechdr(const ZvfsIO*, u64 off, ZvfsRec*);
int zctr_read_record(const ZvfsIO*, u64 off, ZvfsRec*, u8 *payload, u32 cap);

typedef struct ZvfsMapEntry { u64 off; u32 nPayload; u32 flags; } ZvfsMapEntry;
typedef struct ZvfsMap ZvfsMap;
ZvfsMap *zmap_new(const ZvfsIO*);
void zmap_delete(ZvfsMap*);
void zmap_reset(ZvfsMap*, u64 root, u64 pageCount);  /* set committed view, drop staged+cache */
int  zmap_get(ZvfsMap*, u32 pgno, ZvfsMapEntry*);    /* absent -> out->off==0 */
int  zmap_set(ZvfsMap*, u32 pgno, const ZvfsMapEntry*);   /* stage */
u32  zmap_staged_count(const ZvfsMap*);
/* compact.c's node_is_live: walk interior slots from the committed root down to
   (level,firstPgno), returning the stored child offset there (0 if the subtree
   doesn't exist at that level -- e.g. a stale record from a taller generation). */
int  zmap_node_at(ZvfsMap*, u8 level, u32 firstPgno, u64 *pOff);
/* Write COW nodes for staged entries; free replaced/dropped NODE extents into txn's
   pending gen (page-record extents are the container's job). Handles height growth
   and shrink for newPageCount. Updates committed view, clears staged+cache. */
int  zmap_commit(ZvfsMap*, ZvfsAlloc*, u64 txn, u64 newPageCount, u64 *pNewRoot);

/* Opaque to callers outside container.c/compact.c; used from Task 15 for
   OVERWRITE-mode rebuild state. */
struct ZvfsRebuild;

/* internal-shared (not public): compact.c reads/writes fields directly during
   zcompact_step, which runs inside zctr_sync before container.c's own COW/
   list-record steps -- moved here from container.c in Task 14 for that reason. */
typedef struct ZvfsContainer ZvfsContainer;
struct ZvfsContainer {
  ZvfsIO io;
  ZvfsHdr hdr;            /* committed state */
  int whichHdr;           /* last-written copy; -1 before first commit */
  ZvfsMap *map;
  ZvfsAlloc *alloc;
  int allocLoaded;        /* lazy: readers never load it */
  u32 freeRecBytes, pendRecBytes;  /* ZREC_TOTAL of loaded list records */
  u32 pageSize;
  u64 stagedCount;        /* logical page count incl. staged writes/truncate */
  int dirty;
  /* Item 1 fix (docs/design.md Sec7.3's former OPEN BUG -- burst
     fragmentation converging over ~N commits instead of a handful): bytes
     zalloc_release moved from pending to free at THIS commit_once call's
     own step 0 (container.c) -- i.e. the backlog a PRIOR commit's writes
     fragmented, only now becoming visible to compaction per Sec5.3's
     two-generation discipline (an extent freed by commit N cannot be
     reused, or even seen as a compaction candidate, until commit N's
     header is durable -- so it is always the commit AFTER the one that
     fragmented something that gets first crack at repacking it, never that
     same commit). compact.c's byte-budget quota (quota_bytes) scales with
     this alone, deliberately NOT with this commit's own write volume: an
     earlier version also added the write volume, reasoning by analogy with
     the (at-the-time-rejected) quota-byte-budget-WIP.patch attempt
     recorded in .superpowers/reports/, and measurement (RUN_BIG=1 `make
     bigsmoke`) caught that as a real regression -- a large bulk-INSERT
     commit writes many megabytes but releases nothing, yet the write-
     volume term alone was enough to saturate the budget's cap on every
     such commit for no benefit. Bytes released, by contrast, is exactly
     "the backlog this commit's own step 0 just exposed" -- a burst's own
     commit cannot compact what it just fragmented (Sec5.3, structural), so
     it releases little of its own; it is always the FOLLOWING commit, even
     a trivially small one, that releases the whole backlog at its own step
     0 and is what compact.c's descending multi-run sweep (below) needs to
     be sized for, to fully vacate the runs a burst scattered in one pass.
     Recomputed fresh at the top of every commit_once call (like
     compactFull/lastCompactMoved above) -- valid only for that one call. */
  u64 bytesReleasedThisCommit;
  u8 *pg1;                /* decompressed page-1 cache */
  u8 *pgbuf, *paybuf;     /* pageSize scratch: decompress dst, payload src */
  int rebuild;            /* OVERWRITE mode (Task 15) */
  struct ZvfsRebuild *pRb;
  /* Task 16: this container was created by zctr_create_for_convert for a
     plain-database VACUUM conversion, not zctr_create/zctr_open -- it is
     virgin (whichHdr==-1) for the entire rebuild stream and has NO prior
     committed generation to fall back to. commit_once consults this
     (together with whichHdr==-1, a structural belt-and-suspenders check --
     see commit_once's own comment) to pick the torn-flip-safe B-then-A
     header commit (spec Sec7.5) instead of the ordinary single-alternate-
     copy write; Task 19's completeness gate (zctr_sync) consults it to
     decide whether an incomplete/abandoned rebuild attempt reports failure
     (convert: no fallback content exists outside this container) or
     silently falls back to the prior committed generation (ordinary
     OVERWRITE: that generation is real and durable).

     Lifecycle invariant (coordinator-ruled fix, both halves required):
     convert MUST be 0 whenever whichHdr != -1, i.e. the instant ANY header
     copy becomes durably adopted. This is not only true on commit_once's
     own full-success path (which clears it directly) -- it is equally true
     on a PARTIAL commit failure that still durably adopts one copy: if
     copy B lands and syncs but copy A's own write then fails for real (not
     a crash -- the connection survives), zctr_sync_abort's recovery finds
     B valid and adopts it (whichHdr flips from -1), and clears convert
     right there too. Getting this wrong would let THIS SAME container's
     very next commit re-enter the B-first branch and overwrite the only
     currently-valid copy with no fallback -- worse than the hazard the
     flag exists to prevent in the first place.

     If the container never reaches ANY adoption (whichHdr stays -1) --
     whether from a genuine pre-adoption I/O failure or the zero-progress
     convert early-return (zctr_sync's own guard) -- it holds no
     recoverable state at all and the caller (vfs_shim.c) discards it
     wholesale (zctr_close) the moment it observes !zctr_has_committed(),
     rather than leaving it attached for a later transaction-end boundary
     to eventually clean up: zctr_sync_abort's own zctr_reset_empty branch
     (whichHdr==-1 unconditionally routes there) has already reset the
     allocator to a fresh zalloc_new() -- eof pinned at ZVFS_HDR_BLOCK_SIZE
     -- so a surviving container's next ordinary write lands exactly there
     regardless of what offset it targets, corrupting the live plain bytes
     passthrough is still serving reads from (verified directly:
     test/integration/test_convert.c's raw VFS-level regression). */
  int convert;
  /* Task 15 support for zcompact_full's unbounded pack pass (compact.c):
     compactFull, read by compact.c's quota(), forces the next zcompact_step
     call inside a commit_once invocation to use an unbounded quota instead
     of the normal self-regulating one; lastCompactMoved is set (0 or more)
     by that same zcompact_step call so zcompact_full can tell whether the
     pass actually relocated anything (eof alone under-detects progress --
     see zcompact_full's own comment in compact.c for why). Both are pure
     scratch, valid only for the duration of one commit_once call. */
  int compactFull;
  int lastCompactMoved;
  /* Task 18 (WAL reader gate ceiling): optional re-probe hook, bound by
     vfs_shim.c (zctr_set_gate_probe, called right after every
     zctr_open/zctr_create/zctr_create_for_convert success) to a thunk that
     re-runs the real gate computation (zvGateOk) against this container's
     owning ZvfsFile. commit_once calls it, past ZVFS_PENDING_CAP of
     accumulated pending payload, to check whether a caller-supplied
     gateOk==0 is still true before accepting the resulting unbounded
     deferral -- conditions the caller observed before entering this
     function (e.g. an overlapping reader's snapshot) can have cleared in
     the interim. NULL for containers built outside vfs_shim.c (unit tests
     driving container.c directly over a memio ZvfsIO, with no real shm to
     probe, and no WAL reader-gate hazard to begin with) -- commit_once
     treats NULL as "cannot re-probe" and only logs. */
  int (*gateProbe)(void*);
  void *gateProbeCtx;
};

int  zctr_open(ZvfsContainer**, ZvfsIO io);    /* existing container; SQLITE_NOTADB if hdr invalid */
int  zctr_create(ZvfsContainer**, ZvfsIO io);  /* fresh empty; writes nothing until first sync */
void zctr_close(ZvfsContainer*);
int  zctr_read(ZvfsContainer*, void *buf, int amt, i64 off);
int  zctr_write(ZvfsContainer*, const void *buf, int amt, i64 off);
int  zctr_truncate(ZvfsContainer*, i64 logicalSize);
/* the commit point; gateOk!=0 -> pending gens through this txn become reusable
   (rollback mode: always 1 under EXCLUSIVE; WAL checkpoint: Task 18 computes) */
int  zctr_sync(ZvfsContainer*, int syncFlags, int gateOk);
/* Task 18: bind (or clear, with probe==NULL) the pending-cap re-probe hook
   -- see ZvfsContainer.gateProbe's own comment above for what calls this
   and why. Safe to call at any time, including before any commit; a NULL
   probe simply disables the ceiling's re-probe step (commit_once still
   logs). */
void zctr_set_gate_probe(ZvfsContainer*, int (*probe)(void*), void *ctx);
i64  zctr_logical_size(ZvfsContainer*);
u32  zctr_page_size(const ZvfsContainer*);
int  zctr_revalidate(ZvfsContainer*);  /* re-read hdr; on txn change reset map/caches */
int  zctr_is_dirty(const ZvfsContainer*);  /* staged, unsynced changes present? */
/* Task 16: has this container ever durably committed a header copy (i.e.
   c->whichHdr != -1)? Used by vfs_shim.c's zvSync/zvUnlock (converting a
   plain db) to decide, after ANY zctr_sync attempt (success or failure),
   which of two structurally different outcomes just happened, using the
   container's OWN adopted state rather than the sync attempt's own return
   code (see container.c's own comment at the definition for why those two
   can disagree specifically for the torn-flip-safe conversion commit):
     - true -> a real, durable generation now exists (whether this sync
       fully succeeded, or only partially -- e.g. copy B landed but copy
       A's own write then failed for real). Switch to CONTAINER-mode
       dispatch; this container is now the authoritative representation.
     - false -> nothing durable exists at all (rc is necessarily non-OK).
       Discard the container immediately (zctr_close) rather than leaving
       it attached for a later transaction-end boundary to clean up --
       see zvSync's own comment for the corruption a surviving, virgin,
       never-adopted container's own next write can cause. */
int  zctr_has_committed(const ZvfsContainer*);
/* Task 15: SQLITE_FCNTL_OVERWRITE entry point (vfs_shim.c's zvFileControl,
   container mode only) -- enters rebuild mode: allocates c->pRb, points its
   map at a fresh empty tree, and switches c->alloc to append-only so the
   whole rebuild stream lands strictly above the still-untouched, still-
   committed old eof. See container.c's zctr_rebuild_write/zctr_sync (the
   rebuild-commit branch) for the rest of the flow. */
int  zctr_begin_overwrite(ZvfsContainer*);
/* Task 16: plain-database VACUUM conversion entry point (vfs_shim.c's
** zvFileControl, PASSTHROUGH mode only -- a plain db opened through
** zstdvfs, not yet a container). Builds a FRESH container (like
** zctr_create) and immediately places it in the SAME rebuild mode
** zctr_begin_overwrite establishes (c->pRb allocated, c->rebuild=1,
** allocator append-only), but with the allocator's eof pre-advanced to
** zvfs_gran_round64(max(ZVFS_HDR_BLOCK_SIZE, plainSize)) -- strictly past
** every byte of the plain database being converted -- via
** zalloc_reset_span(alloc, startEof, startEof, startEof) (an empty free
** span, i.e. nothing below eof is ever reusable by this container's own
** allocator). c->convert is set to 1. Consequence: every record the
** conversion's rebuild stream places lands strictly above the plain
** database's own bytes, which stay completely untouched (readable via
** ordinary PASSTHROUGH delegation) until the rebuild's own commit
** (container.c's commit_once, B-then-A torn-flip-safe header write) makes
** the container durable. See ZvfsContainer.convert and zctr_sync's Task 19
** completeness gate for how an incomplete/abandoned rebuild attempt or a
** transaction ending without a commit unwinds this cleanly, leaving the
** plain bytes intact for vfs_shim.c to fall back to ordinary passthrough. */
int  zctr_create_for_convert(ZvfsContainer**, ZvfsIO io, u64 plainSize);
/* Discard any staged OVERWRITE rebuild (c->pRb) and revert c->rebuild/
   c->alloc to the ordinary, non-rebuild state -- always safe to call,
   including when no rebuild is active (a no-op then). Public: called
   internally (container.c's own mid-stream/zero-progress guards) AND from
   vfs_shim.c, at every write-transaction-end lock drop and at close, to
   structurally guarantee rebuild state never survives past the
   transaction that started it (fix round 4 -- see container.c's own
   comment at the definition for the reactivation-wipe this closes). */
int  zctr_rebuild_abort(ZvfsContainer*);

/* internal-shared (not public, same convention as zcompact_step below):
   the commit body factored out of zctr_sync (Task 15) -- writes COW map
   nodes/list records, syncs, flips the header, syncs again, releases this
   txn's own generation. Shared by zctr_sync's ordinary path, the OVERWRITE
   rebuild's own commit, and zcompact_full's pack-pass commits, all of which
   need the identical crash-safe protocol. Internally consults c->rebuild to
   skip the two steps that make no sense for the rebuild's own commit
   (compaction, and freeing old list records already subsumed by
   zalloc_reset_span) -- see container.c for the exact gates. */
int  commit_once(ZvfsContainer*, int gateOk);

/* internal-shared (not public, same convention as commit_once/zcompact_step):
   ensures c->pgbuf and c->paybuf are both allocated at c->pageSize, without
   touching whichever one is already there. Shared by zctr_write/zctr_read
   (container.c) and zcompact_step (compact.c) so no call site can allocate
   one without the other -- see container.c's own comment at the definition
   for the leak that motivated centralizing this. */
int  zctr_ensure_scratch(ZvfsContainer*);

/* compact.c: relocate up to a small quota of tail records into lower free
   gaps, called from zctr_sync after zctr_load_alloc / before the old list
   records are freed (see that call site's comment for the full protocol
   position). txn is the generation being committed (c->hdr.txn+1). */
int  zcompact_step(ZvfsContainer*, u64 txn);
/* Task 15: one unbounded-quota pack pass, packaged as one ordinary
   crash-safe commit (via commit_once) -- used by the OVERWRITE rebuild's
   pack-to-front loop under VACUUM's still-held EXCLUSIVE lock. Return: 1 =
   this pass made progress (relocated a record and/or shrank eof/the
   physical file) -- call again; 0 = no progress, already dense, done;
   negative = -rc, a genuine commit failure (container already reverted to
   the last durable generation by commit_once's own abort path) -- stop and
   propagate -returnvalue as the error. CAUTION: "1" is an optimistic
   per-pass signal, not a convergence guarantee -- a `while(zcompact_full(c))`
   loop needs its own independent stall guard (see compact.c's own comment
   on this function, and container.c's zctr_sync_rebuild for the guard). */
int  zcompact_full(ZvfsContainer*);
#endif
