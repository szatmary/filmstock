# tools/sqldiff

`sqldiff.c` and `sqlite3_stdio.{c,h}` are verbatim from the SQLite source
tree at version-3.53.4 (public domain) — the same release as the vendored
amalgamation in internal/sqlite3, so the differ and the library agree on
file-format behaviour. Build with:

    make sqldiff

Used by the release pipeline to emit daily patches (full -> daily) and the
bridge (previous chain tip -> new full). Record-level diffs are meaningful
because every published table is keyed by content-derived ids: two builds
with the same content are sqldiff-identical (measured: zero bytes), so the
bridge really is a null op unless something is wrong.
