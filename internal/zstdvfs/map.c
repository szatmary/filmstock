#include "zvfs_int.h"
#include <stdlib.h>
#include <string.h>
#include <assert.h>

#define SLOT 16
#define CACHE_N 64
typedef struct MapNode { u64 off; u32 tick; u8 buf[ZVFS_NODE_SIZE]; } MapNode;
typedef struct Staged { u32 pgno; ZvfsMapEntry e; } Staged;
struct ZvfsMap {
  ZvfsIO io;
  u64 root, pageCount; int rootLevel;
  Staged *st; u32 nSt, capSt;          /* kept sorted by pgno */
  MapNode cache[CACHE_N]; u32 tick;    /* off==0 -> empty slot */
};

static int level_for(u64 pageCount){
  int L=0; u64 span=ZVFS_FANOUT;
  while(span < pageCount){ span <<= 8; L++; }
  return L;
}
static u32 slot_of(u32 pgno, int l){ return (u32)(((u64)(pgno-1) >> (8*l)) & 0xFF); }

static int node_read(ZvfsMap *m, u64 off, u8 **pBuf){
  MapNode *victim=&m->cache[0];
  for(int i=0;i<CACHE_N;i++){
    if(m->cache[i].off==off){ m->cache[i].tick=++m->tick; *pBuf=m->cache[i].buf; return SQLITE_OK; }
    if(m->cache[i].tick < victim->tick) victim=&m->cache[i];
  }
  ZvfsRec r;
  int rc = zctr_read_record(&m->io, off, &r, victim->buf, ZVFS_NODE_SIZE);
  if(rc) return rc;
  if(r.type!=ZREC_NODE || r.nPayload!=ZVFS_NODE_SIZE ||
     r.crc != zcrc32(0, victim->buf, ZVFS_NODE_SIZE)){
    sqlite3_log(SQLITE_IOERR_READ, "zstdvfs: bad map node at %lld", (long long)off);
    victim->off=0; return SQLITE_IOERR_READ;
  }
  victim->off=off; victim->tick=++m->tick; *pBuf=victim->buf;
  return SQLITE_OK;
}
static void cache_clear(ZvfsMap *m){ for(int i=0;i<CACHE_N;i++) m->cache[i]=(MapNode){0}; }

/* staged: binary search; insert keeps order */
static Staged *st_find(ZvfsMap *m, u32 pgno){
  u32 lo=0, hi=m->nSt;
  while(lo<hi){
    u32 mid = lo + (hi-lo)/2;
    if(m->st[mid].pgno==pgno) return &m->st[mid];
    if(m->st[mid].pgno<pgno) lo=mid+1; else hi=mid;
  }
  return NULL;
}

int zmap_set(ZvfsMap *m, u32 pgno, const ZvfsMapEntry *e){
  u32 lo=0, hi=m->nSt;
  while(lo<hi){
    u32 mid = lo + (hi-lo)/2;
    if(m->st[mid].pgno==pgno){ m->st[mid].e = *e; return SQLITE_OK; }
    if(m->st[mid].pgno<pgno) lo=mid+1; else hi=mid;
  }
  if(m->nSt==m->capSt){
    m->capSt = m->capSt ? m->capSt*2 : 64;
    m->st = realloc(m->st, m->capSt*sizeof(Staged));
  }
  memmove(&m->st[lo+1], &m->st[lo], (m->nSt-lo)*sizeof(Staged));
  m->st[lo].pgno = pgno; m->st[lo].e = *e;
  m->nSt++;
  return SQLITE_OK;
}

u32 zmap_staged_count(const ZvfsMap *m){ return m->nSt; }

int zmap_get(ZvfsMap *m, u32 pgno, ZvfsMapEntry *out){
  memset(out, 0, sizeof(*out));
  Staged *s = st_find(m, pgno);
  if(s){ *out = s->e; return SQLITE_OK; }
  if(!m->root || (u64)pgno > m->pageCount) return SQLITE_OK;
  u64 off = m->root;
  for(int l=m->rootLevel; ; l--){
    u8 *buf; int rc=node_read(m, off, &buf);
    if(rc) return rc;
    u8 *sl = buf + SLOT*slot_of(pgno, l);
    if(l==0){
      out->off=get64le(sl); out->nPayload=get32le(sl+8); out->flags=get32le(sl+12);
      return SQLITE_OK;
    }
    off = get64le(sl);
    if(!off) return SQLITE_OK;
  }
}

/* compact.c's node_is_live: descend from the committed root along firstPgno's
   slot path down to `level`, returning the offset stored there (the node's
   own offset if level==rootLevel, otherwise the interior child pointer one
   level up). 0 if the subtree doesn't reach that deep in the CURRENT
   committed tree (stale record from a taller/differently-shaped generation,
   or an absent child). Reads only the on-disk committed structure through
   the node cache -- staged (uncommitted) entries never change node bytes on
   disk, only zmap_commit's COW pass does that, so this always reflects
   exactly the tree a caller mid-zctr_sync (before its own zmap_commit call)
   would see landing on disk if it committed right now. */
int zmap_node_at(ZvfsMap *m, u8 level, u32 firstPgno, u64 *pOff){
  if(level > m->rootLevel){ *pOff = 0; return SQLITE_OK; }
  u64 off = m->root;
  for(int l = m->rootLevel; l > level; l--){
    if(!off) break;
    u8 *buf; int rc = node_read(m, off, &buf);
    if(rc) return rc;
    off = get64le(buf + SLOT*slot_of(firstPgno, l));
  }
  *pOff = off;
  return SQLITE_OK;
}

static int write_node(ZvfsMap *m, ZvfsAlloc *al, int level, u32 firstPgno,
                      const u8 *buf, u64 *pOff){
  ZvfsRec r = { .type=ZREC_NODE, .flags=0, .nPayload=ZVFS_NODE_SIZE,
                .key=zrec_node_key((u8)level, firstPgno),
                .crc=zcrc32(0, buf, ZVFS_NODE_SIZE) };
  *pOff = zalloc_take(al, ZREC_TOTAL(ZVFS_NODE_SIZE));
  return zctr_write_record(&m->io, *pOff, &r, buf);
}
static void free_node(ZvfsAlloc *al, u64 txn, u64 off){
  if(off) zalloc_free(al, off, ZREC_TOTAL(ZVFS_NODE_SIZE), txn);
}
static int free_subtree(ZvfsMap *m, ZvfsAlloc *al, u64 txn, int level, u64 off){
  if(!off) return SQLITE_OK;
  if(level>0){
    u8 *buf; int rc=node_read(m, off, &buf);
    if(rc) return rc;
    u8 copy[ZVFS_NODE_SIZE]; memcpy(copy, buf, ZVFS_NODE_SIZE);  /* cache may evict */
    for(int i=0;i<ZVFS_FANOUT;i++){
      rc = free_subtree(m, al, txn, level-1, get64le(copy+SLOT*i));
      if(rc) return rc;
    }
  }
  free_node(al, txn, off);
  return SQLITE_OK;
}

/* COW-rewrite the node at (level, firstPgno): apply staged[0..n) (all inside this
   node's range), recurse into children, drop subtrees beyond limitPgno. */
static int commit_node(ZvfsMap *m, ZvfsAlloc *al, u64 txn, int level, u32 firstPgno,
                       u64 oldOff, Staged *stv, u32 n, u64 limitPgno, u64 *pNewOff){
  u8 buf[ZVFS_NODE_SIZE];
  if(oldOff){ u8 *b; int rc=node_read(m, oldOff, &b); if(rc) return rc;
              memcpy(buf, b, ZVFS_NODE_SIZE); }
  else memset(buf, 0, ZVFS_NODE_SIZE);
  int rc;
  if(level==0){
    for(u32 i=0;i<n;i++){
      u8 *sl = buf + SLOT*slot_of(stv[i].pgno, 0);
      put64le(sl, stv[i].e.off); put32le(sl+8, stv[i].e.nPayload);
      put32le(sl+12, stv[i].e.flags);
    }
    for(int i=0;i<ZVFS_FANOUT;i++){          /* zero dropped tail slots */
      if((u64)firstPgno+i > limitPgno) memset(buf+SLOT*i, 0, SLOT);
    }
  }else{
    u64 childSpan = (u64)1 << (8*level);
    u32 i = 0;
    for(int c=0; c<ZVFS_FANOUT; c++){
      u64 childFirst = firstPgno + (u64)c*childSpan;
      u8 *sl = buf + SLOT*c;
      u64 childOld = get64le(sl);
      u32 j = i;
      while(j<n && (u64)stv[j].pgno < childFirst+childSpan) j++;
      if(childFirst > limitPgno){              /* dropped subtree */
        rc = free_subtree(m, al, txn, level-1, childOld);
        if(rc) return rc;
        memset(sl, 0, SLOT);
      }else if(j > i || (childOld && childFirst+childSpan-1 > limitPgno)){
        u64 childNew;
        rc = commit_node(m, al, txn, level-1, (u32)childFirst, childOld,
                         stv+i, j-i, limitPgno, &childNew);
        if(rc) return rc;
        put64le(sl, childNew); put64le(sl+8, 0);
      }
      i = j;
    }
  }
  free_node(al, txn, oldOff);
  return write_node(m, al, level, firstPgno, buf, pNewOff);
}

int zmap_commit(ZvfsMap *m, ZvfsAlloc *al, u64 txn, u64 newPageCount, u64 *pNewRoot){
  int rc, newLevel = level_for(newPageCount ? newPageCount : 1);
  /* height growth: wrap old root in slot 0 of taller nodes */
  while(m->root && m->rootLevel < newLevel){
    u8 buf[ZVFS_NODE_SIZE]; memset(buf, 0, sizeof buf);
    put64le(buf, m->root);
    u64 off; rc = write_node(m, al, m->rootLevel+1, 1, buf, &off);
    if(rc) return rc;
    m->root = off; m->rootLevel++;
  }
  if(m->rootLevel < newLevel) m->rootLevel = newLevel;   /* empty map growing */
  rc = commit_node(m, al, txn, m->rootLevel, 1, m->root,
                   m->st, m->nSt, newPageCount ? newPageCount : 0, &m->root);
  if(rc) return rc;
  /* height shrink: while root has only slot 0, collapse */
  while(m->rootLevel > level_for(newPageCount ? newPageCount : 1)){
    u8 *b; rc = node_read(m, m->root, &b);
    if(rc) return rc;
    u64 only = get64le(b);
    free_node(al, txn, m->root);
    m->root = only; m->rootLevel--;
  }
  m->pageCount = newPageCount;
  m->nSt = 0;
  cache_clear(m);
  *pNewRoot = m->root;
  return SQLITE_OK;
}

void zmap_reset(ZvfsMap *m, u64 root, u64 pageCount){
  m->root=root; m->pageCount=pageCount;
  m->rootLevel = level_for(pageCount ? pageCount : 1);
  m->nSt=0; cache_clear(m);
}

ZvfsMap *zmap_new(const ZvfsIO *io){
  ZvfsMap *m = malloc(sizeof(*m));
  memset(m, 0, sizeof(*m));
  m->io = *io;
  return m;
}

void zmap_delete(ZvfsMap *m){
  if(!m) return;
  free(m->st);
  free(m);
}
