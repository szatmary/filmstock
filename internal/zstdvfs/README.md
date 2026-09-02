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

Upstream is public domain (the Unlicense); see LICENSE.zstdvfs.

## Local changes

None. The GCC portability fixes filmstock needed on 2026-09-01 (sized array
parameters in `container.c`, `-D_DEFAULT_SOURCE`, `-rdynamic`) are upstream
as of dca4058, so these are verbatim copies.

## Registration

`init()` registers the VFS as `"zstdvfs"` and deliberately does NOT make it
the process default: a database CREATED through this VFS is a container from
birth, which would silently compress the intermediate, every exported build
and every test fixture. Reads ask for it by name via `sqldrv.DSN`; creates
get the platform default and stay plain.

## State (2026-09-02)

Upstream's suite passes on Linux out of the box: 4,700,659 checks, 0 failures.

Two failures filmstock hit on 2026-09-01 — a false `SQLITE_FULL` on a large
write, and a WAL that a later read-only open could not recover — turned out
to be one bug, fixed upstream: a single `xWrite` above SQLite's undocumented
~128 KiB per-call contract, which the vendored unix VFS masks with
`nBuf & 0x1ffff` behind an assert that NDEBUG compiles out. A request that is
an exact multiple of 131072 masks to a zero-length write, which `unixWrite`
reads as `SQLITE_FULL` regardless of free space. In WAL mode it aborted the
checkpoint's commit unnoticed, leaving the container at the pre-transaction
generation while the WAL was truncated — silent loss, had filmstock not
verified content hashes. Both reproduce against this pin and pass.

A bulk write leaves the container substantially larger than its plain
equivalent, and online compaction claws that back only gradually, so a
`VACUUM` is wanted after a bulk rebuild. The updater does that after
rebuilding the FTS tables, and applies patches in batches, which converges
denser than one long transaction (311 MB vs 359 MB on the same patch, and
4.8 s vs 41.6 s).

Compaction needs TWO passes after a bulk rebuild, which is why the updater
VACUUMs twice: a container cannot reclaim space its own in-flight commit
freed, so the first pass rewrites the database while reclaiming almost
nothing and the second collects what the first freed. Measured on the
20260901 build: 1.03 GB after one pass, 370 MB after two, and a third
changes nothing.
