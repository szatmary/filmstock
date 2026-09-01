//go:build cgo && !filmstock_purego

// Package sqldrv selects the SQLite driver for all filmstock database opens.
//
// Default (cgo): our vendored C SQLite (internal/sqlite3), the amalgamation
// compiled into the binary with FTS5 enabled — registered as
// "filmstock-sqlite3" — alongside the zstd VFS (internal/zstdvfs), which DSN
// routes reads through so a compressed published database opens exactly like
// a plain one.
// Build with -tags filmstock_purego (or CGO_ENABLED=0) to fall back to the
// pure-Go modernc.org/sqlite driver, which reads plain databases only.
package sqldrv

import (
	_ "github.com/szatmary/filmstock/internal/sqlite3"
	"github.com/szatmary/filmstock/internal/zstdvfs"
)

// Name is the database/sql driver name every filmstock open must use.
const Name = "filmstock-sqlite3"

// PureGo reports whether the fallback pure-Go driver is in use.
const PureGo = false

// DSN builds the connection string for opening an existing database that may
// be a compressed container: it routes the open through the zstd VFS, which
// passes a plain file through untouched and decompresses a container. Use it
// for every open of a database this process did not just create; creates keep
// the platform default so new databases are plain.
func DSN(path string, readOnly bool) string {
	dsn := "file:" + path + "?vfs=" + zstdvfs.Name
	if readOnly {
		dsn += "&mode=ro"
	}
	return dsn
}
