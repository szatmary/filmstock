//go:build cgo && !filmstock_purego

// Package zstdvfs registers the zstd-compressing SQLite VFS as the process
// default for filmstock's own SQLite.
//
// A published full may be a compressed container; every database the pipeline
// builds is an ordinary SQLite file. The VFS tells them apart by looking at
// the file — bytes beginning "SQLite format 3" pass through untouched,
// anything carrying the container header is decompressed per page — so
// opening a database through it is always safe.
//
// It is deliberately NOT the process default. A database CREATED through this
// VFS is a container from birth, so making it the default would quietly
// compress the intermediate, every exported build and every test fixture,
// paying compression on the write-heavy paths that gain nothing from it.
// Reads ask for it by name (see internal/sqldrv.DSN); creates get the
// platform default and stay plain.
package zstdvfs

/*
#cgo CFLAGS: -I${SRCDIR} -I${SRCDIR}/../sqlite3 -DZSTD_DISABLE_ASM
#include "zstdvfs.h"
*/
import "C"

import "fmt"

// Name is the registered VFS name, for callers that want it explicitly.
const Name = "zstdvfs"

func init() {
	// Registered, not made default; see the package comment.
	if rc := C.zstdvfs_register(C.CString(Name), nil, 0); rc != 0 {
		panic(fmt.Sprintf("filmstock: registering the zstd VFS failed: %d", int(rc)))
	}
}
