#ifndef ZSTDVFS_H
#define ZSTDVFS_H
#ifdef __cplusplus
extern "C" {
#endif
int zstdvfs_register(const char *zName, const char *zBaseVfs, int makeDefault);
#ifdef __cplusplus
}
#endif
#endif
