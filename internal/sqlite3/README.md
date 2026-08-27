# internal/sqlite3 — vendored SQLite, compiled by us

This package is the C SQLite engine and its Go binding, vendored so that
**our build compiles our sqlite3.c with our flags**. Consumers of filmstock
get FTS5-enabled SQLite by importing the library — no build tags required.

## Provenance

- `sqlite3-binding.c` / `sqlite3-binding.h` / `sqlite3ext.h` — the SQLite
  **3.53.4** amalgamation (public domain), as shipped in
  github.com/mattn/go-sqlite3 v1.14.50.
- All `.go` files, `sqlite3_opt_unlock_notify.c`, `LICENSE` — the Go binding
  from github.com/mattn/go-sqlite3 v1.14.50 (MIT, see LICENSE).

## Local changes (re-apply on upgrade)

1. `sqlite3.go`: `driverName` = `"filmstock-sqlite3"` (was `"sqlite3"`), so we
   can never collide with a consumer's own mattn/go-sqlite3 registration.
   Same rename in `static_mock.go`.
2. `sqlite3.go` CFLAGS: added `-DSQLITE_ENABLE_FTS5` and
   `-DSQLITE_ENABLE_MATH_FUNCTIONS` unconditionally (upstream gates these
   behind the `sqlite_fts5` / `sqlite_math_functions` build tags; those
   tag files are deleted here: `sqlite3_opt_fts5.go`,
   `sqlite3_opt_math_functions.go`).
3. Dropped: `*_test.go`, `_example/`, `upgrade/`, `go.mod`/`go.sum`,
   upstream README and SECURITY.md.

## Upgrading

    go get github.com/mattn/go-sqlite3@vX.Y.Z   # temporarily
    copy the file set listed above from the module cache
    re-apply the three local changes
    go mod tidy                                  # drops the dep again

Known risk: if a consuming binary statically links a second SQLite (its own
mattn import, for example), the C symbols collide at link time. The driver
*name* rename avoids the database/sql panic, but duplicate `sqlite3_*`
symbols must be resolved by using this copy alone.
