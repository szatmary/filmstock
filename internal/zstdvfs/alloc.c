#include "zvfs_int.h"
#include <assert.h>
#include <stdlib.h>

typedef struct ZGen { u64 txn; u32 n, cap; ZExt *a; } ZGen;
struct ZvfsAlloc {
  ZExt *fr; u32 nFr, capFr;    /* sorted by off, coalesced */
  ZGen *gen; u32 nGen, capGen; /* ascending txn */
  u64 eof, freeBytes;
  int appendOnly;              /* Task 15: OVERWRITE rebuild stream -- see
                                   zalloc_set_appendonly */
};

#define HOLE0 ((u64)ZVFS_LOCK_HOLE_OFF)
#define HOLE1 (HOLE0 + ZVFS_LOCK_HOLE_LEN)

/* Every heap allocation in this file funnels through here: a failed
   realloc/malloc on this path would otherwise leave a wild/NULL pointer
   silently corrupting allocator state. Fail loud and diagnosable instead. */
static void *zvfs_xrealloc(void *p, size_t n){
  void *q = realloc(p, n);
  if(!q && n!=0){
    sqlite3_log(SQLITE_NOMEM, "zstdvfs: out of memory in allocator");
    abort();
  }
  return q;
}

ZvfsAlloc *zalloc_new(void){
  ZvfsAlloc *a = zvfs_xrealloc(NULL, sizeof(*a));
  memset(a, 0, sizeof(*a));
  a->eof = ZVFS_HDR_BLOCK_SIZE;
  return a;
}

void zalloc_delete(ZvfsAlloc *a){
  if(!a) return;
  for(u32 i=0; i<a->nGen; i++) free(a->gen[i].a);
  free(a->gen); free(a->fr); free(a);
}

/* Insert extent {off,len} into fr, sorted by offset, coalescing with the
   immediate neighbor(s) it now touches. Caller has already validated bounds. */
static void fr_insert(ZvfsAlloc *a, u64 off, u32 len){
  u32 i = 0;
  while(i < a->nFr && a->fr[i].off < off) i++;
  int mergePrev = i>0 && a->fr[i-1].off + a->fr[i-1].len == off;
  int mergeNext = i<a->nFr && off + len == a->fr[i].off;
  if(mergePrev && mergeNext){
    a->fr[i-1].len += len + a->fr[i].len;
    memmove(&a->fr[i], &a->fr[i+1], (a->nFr-i-1)*sizeof(ZExt));
    a->nFr--;
  }else if(mergePrev){
    a->fr[i-1].len += len;
  }else if(mergeNext){
    a->fr[i].off = off; a->fr[i].len += len;
  }else{
    if(a->nFr == a->capFr){
      a->capFr = a->capFr ? a->capFr*2 : 16;
      a->fr = zvfs_xrealloc(a->fr, a->capFr*sizeof(ZExt));
    }
    memmove(&a->fr[i+1], &a->fr[i], (a->nFr-i)*sizeof(ZExt));
    a->fr[i].off = off; a->fr[i].len = len;
    a->nFr++;
  }
  a->freeBytes += len;
}

/* Insert extent {off,len} into one generation's pending array, sorted by
   offset, coalescing with the immediate neighbor(s) it now touches -- same
   algorithm as fr_insert above, applied to a ZGen instead of ZvfsAlloc's
   fr[]. A single commit can free many individually-small, often-adjacent
   extents (deletes, incr_vacuum relocations, cache-pressure-driven spills
   all landing in the same still-uncommitted generation); without this, the
   pending array grows one entry per zalloc_free call with no merging at
   all, and a churn-heavy single transaction can serialize a pending-list
   payload of thousands of uncoalesced entries into one oversized ZREC_PENDING
   record. Coalescing here only changes how compactly this NOT-YET-RELEASED
   generation's own bookkeeping is represented -- it does not make any of
   these extents reusable (that still requires zalloc_release, gated
   exactly as before) or change which commit's release exposes them to
   zalloc_trim, so it does not reopen the two-generation crash-safety
   argument: coalesced or not, this generation's extents remain pending
   until release() is called on it. */
static void gen_insert(ZGen *g, u64 off, u32 len){
  u32 i = 0;
  while(i < g->n && g->a[i].off < off) i++;
  int mergePrev = i>0 && g->a[i-1].off + g->a[i-1].len == off;
  int mergeNext = i<g->n && off + len == g->a[i].off;
  if(mergePrev && mergeNext){
    g->a[i-1].len += len + g->a[i].len;
    memmove(&g->a[i], &g->a[i+1], (g->n-i-1)*sizeof(ZExt));
    g->n--;
  }else if(mergePrev){
    g->a[i-1].len += len;
  }else if(mergeNext){
    g->a[i].off = off; g->a[i].len += len;
  }else{
    if(g->n == g->cap){
      g->cap = g->cap ? g->cap*2 : 8;
      g->a = zvfs_xrealloc(g->a, g->cap*sizeof(ZExt));
    }
    memmove(&g->a[i+1], &g->a[i], (g->n-i)*sizeof(ZExt));
    g->a[i].off = off; g->a[i].len = len;
    g->n++;
  }
}

/* Pre-cross the lock hole so the next nBytes of extends are pure appends: if
   [eof, eof+n) would overlap the hole, skip eof to HOLE1 now, turning any
   [eof, HOLE0) gap into an ordinary free extent (coalesced, counted in
   freeBytes) rather than silently reclaiming it inside zalloc_extend. */
void zalloc_reserve_eof(ZvfsAlloc *a, u32 nBytes){
  u32 n = zvfs_gran_round(nBytes);
  if(a->eof < HOLE1 && a->eof + n > HOLE0){
    if(a->eof < HOLE0) fr_insert(a, a->eof, (u32)(HOLE0 - a->eof));
    a->eof = HOLE1;
  }
}

u64 zalloc_extend(ZvfsAlloc *a, u32 nBytes){
  u32 n = zvfs_gran_round(nBytes);
  zalloc_reserve_eof(a, n);
  u64 result = a->eof;
  a->eof += n;
  return result;
}

/* Task 15: while an OVERWRITE rebuild stream is in flight, every extent
   must land strictly above the (untouched, still-committed) old eof -- best
   fit reusing fr[] would reach back into space the CURRENT durable header
   still references (fr[] only ever holds space released from generations
   that were already superseded by a flip, which is not true of anything
   during a rebuild -- the old generation stays the sole committed one, in
   full, until the rebuild's own commit lands). Delegating unconditionally
   to zalloc_extend is what makes the whole stream a dense, purely-appended
   run above the old eof, exactly what the pack loop afterward relies on to
   relocate it forward as one contiguous block of work. */
void zalloc_set_appendonly(ZvfsAlloc *a, int on){ a->appendOnly = on; }

u64 zalloc_take(ZvfsAlloc *a, u32 nBytes){
  if(a->appendOnly) return zalloc_extend(a, nBytes);
  u32 n = zvfs_gran_round(nBytes);
  u32 best = a->nFr;
  for(u32 i=0; i<a->nFr; i++){
    if(a->fr[i].len >= n && (best==a->nFr || a->fr[i].len < a->fr[best].len))
      best = i;
  }
  if(best == a->nFr) return zalloc_extend(a, n);
  u64 result = a->fr[best].off;
  a->fr[best].off += n;
  a->fr[best].len -= n;
  a->freeBytes -= n;
  if(a->fr[best].len == 0){
    memmove(&a->fr[best], &a->fr[best+1], (a->nFr-best-1)*sizeof(ZExt));
    a->nFr--;
  }
  return result;
}

/* Same best-fit-lowest-offset scan as zalloc_take, without mutation: returns
   the offset zalloc_take would hand back, or 0 if the free list has nothing
   big enough (i.e. zalloc_take would extend eof instead). Used by compact.c
   to decide whether relocating a tail record into a lower gap is possible
   before actually doing the work. */
u64 zalloc_peek(ZvfsAlloc *a, u32 nBytes){
  u32 n = zvfs_gran_round(nBytes);
  u32 best = a->nFr;
  for(u32 i=0; i<a->nFr; i++){
    if(a->fr[i].len >= n && (best==a->nFr || a->fr[i].len < a->fr[best].len))
      best = i;
  }
  if(best == a->nFr) return 0;
  return a->fr[best].off;
}

/* See zvfs_int.h for the full rationale. off+n exactly at the tail (the
   ordinary case: zalloc_extend was the fallback that produced it, which is
   the ONLY thing zalloc_take ever does in append-only mode) means a true
   undo -- just retract eof, leaving no trace whatsoever, not even an fr[]
   entry; otherwise (a best-fit take from fr[] whose own end doesn't touch
   eof) give it back the ordinary way. */
void zalloc_untake(ZvfsAlloc *a, u64 off, u32 nBytes){
  u32 n = zvfs_gran_round(nBytes);
  if(off + n == a->eof){
    a->eof = off;
  }else{
    fr_insert(a, off, n);
  }
}

void zalloc_free(ZvfsAlloc *a, u64 off, u32 nBytes, u64 txn){
  u32 n = zvfs_gran_round(nBytes);
  ZGen *g;
  if(a->nGen > 0 && a->gen[a->nGen-1].txn == txn){
    g = &a->gen[a->nGen-1];
  }else{
    assert(a->nGen == 0 || txn >= a->gen[a->nGen-1].txn);
    if(a->nGen == a->capGen){
      a->capGen = a->capGen ? a->capGen*2 : 4;
      a->gen = zvfs_xrealloc(a->gen, a->capGen*sizeof(ZGen));
    }
    g = &a->gen[a->nGen++];
    g->txn = txn; g->n = 0; g->cap = 0; g->a = NULL;
  }
  gen_insert(g, off, n);
}

void zalloc_release(ZvfsAlloc *a, u64 uptoTxn){
  u32 keep = 0;
  for(u32 i=0; i<a->nGen; i++){
    if(a->gen[i].txn <= uptoTxn){
      for(u32 j=0; j<a->gen[i].n; j++)
        fr_insert(a, a->gen[i].a[j].off, a->gen[i].a[j].len);
      free(a->gen[i].a);
    }else{
      a->gen[keep++] = a->gen[i];
    }
  }
  a->nGen = keep;
}

/* Task 15: the OVERWRITE rebuild commit's own allocator reset -- the whole
   OLD generation (everything below the just-appended dense stream) becomes
   free space in one shot, replacing §7.4's original "slide the new
   generation down in chunk-moves" mechanism: an ordinary best-fit
   allocation into this span, followed by the incremental pack loop
   (zcompact_full), does the same densification work as ordinary,
   already-crash-safe commit machinery, so nothing bespoke is needed here
   beyond correctly describing the span. Every existing generation entry
   (gen[]) is discarded outright rather than released into fr[] first:
   those entries describe extents somewhere inside [freeFrom,freeTo) too
   (they were freed by ordinary commits against the OLD generation, which
   this call is superseding wholesale), so re-adding them individually on
   top of the span this function is about to insert would double-count the
   same bytes. Splits around the lock hole exactly like zalloc_reserve_eof
   does for a single extent, generalized to a possibly-hole-spanning
   range. */
void zalloc_reset_span(ZvfsAlloc *a, u64 freeFrom, u64 freeTo, u64 eof){
  for(u32 i=0; i<a->nGen; i++) free(a->gen[i].a);
  a->nGen = 0;
  a->nFr = 0;
  a->freeBytes = 0;
  u64 lo1 = freeFrom;
  u64 hi1 = freeTo < HOLE0 ? freeTo : (freeFrom < HOLE0 ? HOLE0 : freeFrom);
  if(hi1 > lo1) fr_insert(a, lo1, (u32)(hi1-lo1));
  u64 lo2 = freeFrom > HOLE1 ? freeFrom : HOLE1;
  u64 hi2 = freeTo;
  if(hi2 > lo2) fr_insert(a, lo2, (u32)(hi2-lo2));
  a->eof = eof;
}

u64 zalloc_eof(const ZvfsAlloc *a){ return a->eof; }
u64 zalloc_free_bytes(const ZvfsAlloc *a){ return a->freeBytes; }
u32 zalloc_free_count(const ZvfsAlloc *a){ return a->nFr; }

int zalloc_free_at(const ZvfsAlloc *a, u32 i, ZExt *out){
  if(i >= a->nFr) return SQLITE_IOERR_READ;
  *out = a->fr[i];
  return SQLITE_OK;
}

/* fr[] is kept sorted by offset, so the highest-offset free extent -- the one
   that borders the tail compaction walks toward, and the one zalloc_trim can
   pop -- is always the last element. */
int zalloc_last_free(const ZvfsAlloc *a, ZExt *out){
  if(a->nFr == 0) return 0;
  *out = a->fr[a->nFr-1];
  return 1;
}

/* Pop trailing free extent(s) that directly abut eof, lowering eof over them.
   Safe because fr[] holds only *released* extents: zalloc_release only moves
   a generation's frees in here once release-gating has already proved no
   surviving root (committed or still-pending) references them, so there is
   nothing live left in that span for a lower eof to discard. Pending frees
   (not yet released) never appear in fr[], so this never trims a still-
   referenced tail -- that's the two-generation lag compact.c's caller relies
   on: this commit's own frees only become trimmable on a later commit. */
int zalloc_trim(ZvfsAlloc *a){
  int changed = 0;
  while(a->nFr && a->fr[a->nFr-1].off + a->fr[a->nFr-1].len == a->eof){
    a->eof = a->fr[a->nFr-1].off;
    a->freeBytes -= a->fr[a->nFr-1].len;
    a->nFr--;
    changed = 1;
  }
  return changed;
}

u32 zalloc_ser_free_size(const ZvfsAlloc *a){ return a->nFr * 12; }

u32 zalloc_ser_pend_size(const ZvfsAlloc *a){
  u32 n = 0;
  for(u32 i=0; i<a->nGen; i++) n += 12 + a->gen[i].n * 12;
  return n;
}

void zalloc_ser_free(const ZvfsAlloc *a, u8 *buf){
  for(u32 i=0; i<a->nFr; i++){
    put64le(buf, a->fr[i].off); put32le(buf+8, a->fr[i].len);
    buf += 12;
  }
}

void zalloc_ser_pend(const ZvfsAlloc *a, u8 *buf){
  for(u32 i=0; i<a->nGen; i++){
    put64le(buf, a->gen[i].txn); put32le(buf+8, a->gen[i].n);
    buf += 12;
    for(u32 j=0; j<a->gen[i].n; j++){
      put64le(buf, a->gen[i].a[j].off); put32le(buf+8, a->gen[i].a[j].len);
      buf += 12;
    }
  }
}

/* Validate an extent as fit for the on-disk free/pending payload: granule
   aligned, above the header block, not overlapping the lock hole. */
static int ext_valid(u64 off, u32 len){
  if(len == 0 || (len % ZVFS_GRANULE) != 0 || (off % ZVFS_GRANULE) != 0) return 0;
  if(off < ZVFS_HDR_BLOCK_SIZE) return 0;
  if(off < HOLE1 && off+len > HOLE0) return 0;
  return 1;
}

/* Both payloads may now be padded (zctr_sync places FREELIST/PENDING
   records with an upper-bound reserved size, zero-filling whatever the
   actual content doesn't use -- see that function's comment). Padding is
   distinguished from real content by a terminator that can never occur
   naturally: a free-list entry's off is always >= ZVFS_HDR_BLOCK_SIZE
   (ext_valid enforces this on every real entry), so off==0 unambiguously
   marks "no more entries, rest is padding"; a pending group's txn is
   always >= 1 (the first commit a virgin container -- c->hdr.txn starts
   at 0 -- ever makes is txn=1), so txn==0 marks the same for the pending
   list. Bytes after the terminator are never inspected or validated --
   deliberately: this loader's job is to recover the real content, not to
   police what a correctly-written padded record fills unused space with. */
int zalloc_load(ZvfsAlloc *a, const u8 *freePay, u32 nFree,
                const u8 *pendPay, u32 nPend, u64 eof){
  if(nFree % 12 != 0) return SQLITE_IOERR_READ;
  u32 cap = nFree / 12;
  ZExt *fr = cap ? zvfs_xrealloc(NULL, cap*sizeof(ZExt)) : NULL;
  u64 prevEnd = 0;
  u64 bytes = 0;
  u32 n = 0;
  for(u32 i=0; i<cap; i++){
    u64 off = get64le(freePay + i*12);
    u32 len = get32le(freePay + i*12 + 8);
    if(off == 0) break;                     /* terminator: stop, ignore the rest */
    /* off+len>eof would wrap for off near UINT64_MAX; compare without adding. */
    if(!ext_valid(off, len) || off > eof || len > eof-off || (n>0 && off <= prevEnd)){
      free(fr); return SQLITE_IOERR_READ;
    }
    fr[n].off = off; fr[n].len = len;
    prevEnd = off + len;
    bytes += len;
    n++;
  }

  const u8 *p = pendPay;
  u32 remain = nPend;
  u32 nGen = 0, capGen = 0;
  ZGen *gen = NULL;
  u64 prevTxn = 0;
  while(remain > 0){
    if(remain < 12) goto pend_bad;
    u64 txn = get64le(p);
    if(txn == 0) break;                     /* terminator: stop, ignore the rest */
    u32 count = get32le(p+8);
    p += 12; remain -= 12;
    if(nGen > 0 && txn < prevTxn) goto pend_bad;
    if((u64)count*12 > remain) goto pend_bad;
    if(nGen == capGen){ capGen = capGen ? capGen*2 : 4; gen = zvfs_xrealloc(gen, capGen*sizeof(ZGen)); }
    ZGen *g = &gen[nGen++];
    g->txn = txn; g->n = count; g->cap = count;
    g->a = count ? zvfs_xrealloc(NULL, count*sizeof(ZExt)) : NULL;
    for(u32 j=0; j<count; j++){
      u64 off = get64le(p); u32 len = get32le(p+8);
      /* off+len>eof would wrap for off near UINT64_MAX; compare without adding. */
      if(!ext_valid(off, len) || off > eof || len > eof-off) goto pend_bad;
      g->a[j].off = off; g->a[j].len = len;
      p += 12; remain -= 12;
    }
    prevTxn = txn;
  }

  for(u32 i=0; i<a->nGen; i++) free(a->gen[i].a);
  free(a->gen); free(a->fr);
  a->fr = fr; a->nFr = n; a->capFr = cap;
  a->gen = gen; a->nGen = nGen; a->capGen = capGen;
  a->eof = eof; a->freeBytes = bytes;
  return SQLITE_OK;

pend_bad:
  free(fr);
  for(u32 k=0; k<nGen; k++) free(gen[k].a);
  free(gen);
  return SQLITE_IOERR_READ;
}
