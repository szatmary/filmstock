//go:build !cgo || filmstock_purego

package sqldrv

import (
	_ "modernc.org/sqlite"
)

// Name is the database/sql driver name every filmstock open must use.
const Name = "sqlite"

// PureGo reports whether the fallback pure-Go driver is in use.
const PureGo = true

// DSN builds the connection string for opening an existing database. This
// build has no zstd VFS, so a compressed published database cannot be opened
// at all; a plain one opens normally.
func DSN(path string, readOnly bool) string {
	dsn := "file:" + path
	if readOnly {
		dsn += "?mode=ro"
	}
	return dsn
}
