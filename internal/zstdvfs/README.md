# internal/zstdvfs — vendored zstd-compressing SQLite VFS

Reads and writes SQLite databases stored as zstd-compressed containers.
A published full can therefore ship at half size and stay compressed on the
consumer's disk, because the VFS decompresses per page on read rather than
unpacking the file.

## Provenance

- `alloc.c`, `codec.c`, `compact.c`, `container.c`, `map.c`, `vfs_shim.c`,
  `zstdvfs.h`, `zvfs_int.h` — github.com/szatmary/sqlite-zstdvfs at the
  commit in `UPSTREAM_COMMIT`.
- `zstd_amalgam.c`, `zstd.h`, `zstd_errors.h` — zstd v1.5.7 (BSD/GPLv2, see
  LICENSE.zstd), the single-file amalgamation from
  `build/single_file_libs/create_single_file_library.sh`. Bundled rather than
  linked so `go get` of this module needs no system libzstd.
- `sqlite3.h` — a one-line shim onto `../sqlite3/sqlite3-binding.h`, because
  the upstream sources include `"sqlite3.h"` and our amalgamation header has
  a different name.

Upstream states no license. Confirm one before this module is published.

## Local changes (re-apply on upgrade)

`container.c`: four definitions (`zhdr_encode`, `zhdr_decode`, `zrec_encode`,
`zrec_decode`) took `u8 *` while `zvfs_int.h` declares sized arrays. Clang is
silent; GCC's `-Warray-parameter` makes it an error under upstream's `-Werror`.
The definitions now match the declarations.

Upstream's Makefile needs two more Linux fixes to build its own tests, which
do not affect these sources: `-D_DEFAULT_SOURCE` (for `usleep`/`random` under
`-std=c11`) and `-rdynamic` when linking test binaries that `dlopen` the
extension. Both are in the patch kept beside this work.

## Registration

`init()` registers the VFS as `"zstdvfs"` and deliberately does NOT make it
the process default: a database CREATED through this VFS is a container from
birth, which would silently compress the intermediate, every exported build
and every test fixture. Reads ask for it by name via `sqldrv.DSN`; creates
get the platform default and stay plain.

## Known limits (measured here, 2026-09-01)

Reading is solid: containers open, query and content-hash identically to the
plain files they were made from, and upstream's own suite passes on Linux
(4,700,340 checks, 0 failures) once the fixes above are applied.

Writing large transactions into an existing container does not yet work:

1. Rollback-journal mode: a mixed insert/update/delete transaction fails with
   `SQLITE_FULL` once the container has grown about 72.8 MB, on a disk with
   terabytes free. Measured twice, 4 KB apart, on containers of 152 MB and
   352 MB. The same statements split into 4,000-statement transactions
   succeed, as does a 155 MB single-transaction rewrite of a synthetic
   container — so it is workload-shaped, not a simple size cap.
2. WAL mode takes those same transactions, but afterwards
   `PRAGMA journal_mode=delete` (even after `wal_checkpoint(TRUNCATE)`) fails
   with `SQLITE_IOERR`/ENOENT on a 300 MB container, leaving a WAL that a
   later read-only open cannot recover ("attempt to write a readonly
   database").

Because a consumer applies daily patches by writing into its copy, this is
why `filmstock publish -compress` is off by default.
