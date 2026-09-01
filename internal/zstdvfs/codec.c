#include "zvfs_int.h"
#include <zstd.h>

static u32 crc_tab[256];
static int crc_init_done;
static void crc_init(void){
  for(u32 i=0;i<256;i++){ u32 c=i;
    for(int k=0;k<8;k++) c = (c&1) ? 0xEDB88320u^(c>>1) : c>>1;
    crc_tab[i]=c; }
  crc_init_done=1;
}
u32 zcrc32(u32 seed, const void *buf, size_t n){
  if(!crc_init_done) crc_init();
  u32 c = seed ^ 0xFFFFFFFFu;
  const u8 *p = buf;
  while(n--) c = crc_tab[(c^*p++)&0xFF] ^ (c>>8);
  return c ^ 0xFFFFFFFFu;
}

static _Thread_local ZSTD_CCtx *tls_cctx;
static _Thread_local ZSTD_DCtx *tls_dctx;

int zcodec_compress(const u8 *pg, u32 pgsz, u8 *out, u32 *pnOut,
                    int level, int *pRaw){
  if(!tls_cctx){ tls_cctx = ZSTD_createCCtx(); if(!tls_cctx) return SQLITE_NOMEM; }
  ZSTD_CCtx_reset(tls_cctx, ZSTD_reset_session_and_parameters);
  ZSTD_CCtx_setParameter(tls_cctx, ZSTD_c_compressionLevel, level);
  ZSTD_CCtx_setParameter(tls_cctx, ZSTD_c_checksumFlag, 1);
  ZSTD_CCtx_setParameter(tls_cctx, ZSTD_c_contentSizeFlag, 1);
  size_t n = ZSTD_compress2(tls_cctx, out, pgsz-1, pg, pgsz);
  if(ZSTD_isError(n)){            /* incl. dstSize_tooSmall: did not shrink */
    memcpy(out, pg, pgsz);
    *pnOut = pgsz; *pRaw = 1;
    return SQLITE_OK;
  }
  *pnOut = (u32)n; *pRaw = 0;
  return SQLITE_OK;
}

int zcodec_decompress(const u8 *in, u32 nIn, int raw, u8 *pg, u32 pgsz){
  if(raw){
    if(nIn != pgsz) return SQLITE_IOERR_READ;
    memcpy(pg, in, pgsz);
    return SQLITE_OK;
  }
  if(!tls_dctx){ tls_dctx = ZSTD_createDCtx(); if(!tls_dctx) return SQLITE_NOMEM; }
  size_t n = ZSTD_decompressDCtx(tls_dctx, pg, pgsz, in, nIn);
  if(ZSTD_isError(n) || n != pgsz){
    sqlite3_log(SQLITE_IOERR_READ, "zstdvfs: zstd decompress failed (%s)",
                ZSTD_isError(n) ? ZSTD_getErrorName(n) : "size mismatch");
    return SQLITE_IOERR_READ;
  }
  return SQLITE_OK;
}
