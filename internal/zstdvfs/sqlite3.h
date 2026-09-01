/* Shim: the VFS sources include "sqlite3.h"; our vendored amalgamation header
   is named sqlite3-binding.h, found via the -I in zstdvfs.go's cgo CFLAGS. */
#include "sqlite3-binding.h"
