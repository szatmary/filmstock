//go:build !cgo || filmstock_purego

package sqldrv

import (
	_ "modernc.org/sqlite"
)

// Name is the database/sql driver name every filmstock open must use.
const Name = "sqlite"

// PureGo reports whether the fallback pure-Go driver is in use.
const PureGo = true
