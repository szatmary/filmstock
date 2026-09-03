package build

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/szatmary/filmstock/internal/sqldrv"
)

// CmdSchema writes the consumer-facing schema reference.
//
// Generated from a real published build rather than written by hand, because
// a schema document that is maintained separately is a schema document that is
// wrong. SQLite keeps the original CREATE statement verbatim — comments and
// all — so the notes that explain each table travel inside the file they
// describe.
//
//	filmstock schema -db bucket/20260801/filmstock.db > docs/schema.md
func CmdSchema(args []string) {
	fs := flag.NewFlagSet("schema", flag.ExitOnError)
	core := fs.String("db", "", "the published core database")
	text := fs.String("text-db", "", "synopsis database (default: beside -db)")
	vec := fs.String("vectors-db", "", "vectors database (default: beside -db)")
	out := fs.String("out", "", "where to write (default stdout)")
	fs.Parse(args)
	if *core == "" {
		fatal(fmt.Errorf("schema needs -db FILE"))
	}
	if *text == "" {
		*text = defaultTextPath(*core)
	}
	if *vec == "" {
		*vec = strings.TrimSuffix(*core, ".db") + "-vectors.db"
	}

	var b strings.Builder
	b.WriteString(schemaPreamble)
	for _, f := range []struct{ path, name, what string }{
		{*core, "filmstock.db", "Every entity, every credit, and the search indexes."},
		{*text, "filmstock-text.db", "Prose: overviews, plots and episode summaries. Ships separately because a consumer that only searches never needs it."},
		{*vec, "filmstock-vectors.db", "Embedding vectors, for similarity and recommendation."},
	} {
		if _, err := os.Stat(f.path); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: absent, skipping\n", f.path)
			continue
		}
		s, err := describeDB(f.path, f.name, f.what)
		if err != nil {
			fatal(err)
		}
		b.WriteString(s)
	}
	if *out == "" {
		fmt.Print(b.String())
		return
	}
	if err := os.WriteFile(*out, []byte(b.String()), 0o644); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  -> %s\n", *out)
}

func describeDB(path, name, what string) (string, error) {
	db, err := sql.Open(sqldrv.Name, sqldrv.DSN(path, true))
	if err != nil {
		return "", err
	}
	defer db.Close()

	var b strings.Builder
	fmt.Fprintf(&b, "\n## `%s`\n\n%s\n", name, what)
	if fi, err := os.Stat(path); err == nil {
		fmt.Fprintf(&b, "\nAs published (%s): %d MB.\n", filepath.Base(path), fi.Size()/1048576)
	}

	rows, err := db.Query(`SELECT name, sql FROM sqlite_master
	    WHERE type='table' AND name NOT LIKE 'sqlite_%'
	      AND name NOT LIKE '%_fts_data' AND name NOT LIKE '%_fts_idx'
	      AND name NOT LIKE '%_fts_docsize' AND name NOT LIKE '%_fts_config'
	    ORDER BY name LIKE '%_fts', name`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	type tbl struct{ name, ddl string }
	var tables []tbl
	for rows.Next() {
		var t tbl
		var ddl sql.NullString
		if err := rows.Scan(&t.name, &ddl); err != nil {
			return "", err
		}
		t.ddl = ddl.String
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	for _, t := range tables {
		// An external-content FTS5 table counts its CONTENT table, so asking
		// how many rows movies_fts holds answers 166,159 even when the index
		// is empty — which is exactly how these ship. Say what is true.
		if strings.HasSuffix(t.name, "_fts") {
			fmt.Fprintf(&b, "\n### %s — ships empty, rebuild locally\n\n```sql\n%s;\n```\n",
				t.name, strings.TrimSpace(t.ddl))
			continue
		}
		var n int64
		db.QueryRow(`SELECT COUNT(*) FROM "` + t.name + `"`).Scan(&n)
		fmt.Fprintf(&b, "\n### %s — %s rows\n\n```sql\n%s;\n```\n", t.name, commas(n), strings.TrimSpace(t.ddl))
		idx, err := db.Query(`SELECT sql FROM sqlite_master WHERE type='index'
		     AND tbl_name=? AND sql IS NOT NULL ORDER BY name`, t.name)
		if err != nil {
			continue
		}
		var list []string
		for idx.Next() {
			var s string
			if idx.Scan(&s) == nil {
				list = append(list, "    "+strings.TrimSpace(s)+";")
			}
		}
		idx.Close()
		if len(list) > 0 {
			fmt.Fprintf(&b, "\nIndexes:\n\n```sql\n%s\n```\n", strings.Join(list, "\n"))
		}
	}
	return b.String(), nil
}

func commas(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
