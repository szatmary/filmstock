//go:build cgo && !filmstock_purego

// Package sqldrv selects the SQLite driver for all filmstock database opens.
//
// Default (cgo): our vendored C SQLite (internal/sqlite3), the amalgamation
// compiled into the binary with FTS5 enabled — registered as
// "filmstock-sqlite3". Build with -tags filmstock_purego (or CGO_ENABLED=0)
// to fall back to the pure-Go modernc.org/sqlite driver instead.
package sqldrv

import (
	_ "github.com/szatmary/filmstock/internal/sqlite3"
)

// Name is the database/sql driver name every filmstock open must use.
const Name = "filmstock-sqlite3"

// PureGo reports whether the fallback pure-Go driver is in use.
const PureGo = false
