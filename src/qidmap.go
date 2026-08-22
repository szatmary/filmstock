package main

import (
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// reWikibase matches a page_props tuple that assigns a Wikidata item to a page:
//
//	(12345,'wikibase_item','Q678',NULL)
var reWikibase = regexp.MustCompile(`\((\d+),'wikibase_item','Q(\d+)'`)

// cmdBuildQidmap builds a title→Q-id lookup table by joining the MySQL
// page_props dump (page_id → wikibase_item) with the multistream index
// (page_id → title). Output: table wiki_qid(title PRIMARY KEY, qid) in the db.
func cmdBuildQidmap(args []string) {
	fs := flag.NewFlagSet("build-qidmap", flag.ExitOnError)
	pp := fs.String("pageprops", "../dump/enwiki-latest-page_props.sql.gz", "page_props.sql.gz")
	idx := fs.String("index", "../dump/enwiki-latest-pages-articles-multistream-index.txt.bz2", "multistream index")
	dbPath := fs.String("db", "../movies.db", "output SQLite database")
	fs.Parse(args)

	// 1. page_id -> qid from page_props (streamed, chunked regex scan).
	pid2qid := make(map[int]int32, 1<<24)
	f, err := os.Open(*pp)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		fatal(err)
	}
	scanChunks(gz, func(chunk []byte) {
		for _, m := range reWikibase.FindAllSubmatch(chunk, -1) {
			pid, _ := strconv.Atoi(string(m[1]))
			q, _ := strconv.ParseInt(string(m[2]), 10, 64)
			pid2qid[pid] = int32(q)
		}
	})
	fmt.Fprintf(os.Stderr, "page_props: %d pages have a Wikidata item\n", len(pid2qid))

	// 2. Join with the index (page_id -> title) and write title -> qid.
	xf, err := os.Open(*idx)
	if err != nil {
		fatal(err)
	}
	defer xf.Close()
	sc := bufio.NewScanner(bzip2.NewReader(bufio.NewReaderSize(xf, 1<<20)))
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	db.Exec(`PRAGMA journal_mode=OFF; PRAGMA synchronous=OFF;`)
	db.Exec(`DROP TABLE IF EXISTS wiki_qid`)
	// page_id is carried alongside the title: Wikidata states relations between
	// items (Q-ids), and joining those back to articles needs a page_id, not a
	// title. Titles are aliases — redirects mean several map to one article.
	if _, err := db.Exec(`CREATE TABLE wiki_qid(title TEXT PRIMARY KEY, qid INTEGER, page_id INTEGER)`); err != nil {
		fatal(err)
	}
	tx, _ := db.Begin()
	stmt, _ := tx.Prepare(`INSERT OR IGNORE INTO wiki_qid(title,qid,page_id) VALUES(?,?,?)`)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		// format: offset:pageid:title  (title may contain ':')
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		pid, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		if q, ok := pid2qid[pid]; ok {
			stmt.Exec(parts[2], q, pid)
			if n++; n%1_000_000 == 0 {
				fmt.Fprintf(os.Stderr, "\r  mapped %d titles...", n)
			}
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		fatal(err)
	}
	db.Exec(`CREATE INDEX idx_wiki_qid ON wiki_qid(qid)`)
	db.Exec(`CREATE INDEX idx_wiki_qid_page ON wiki_qid(page_id)`)
	fmt.Fprintf(os.Stderr, "\rwiki_qid built: %d title→Q-id rows\n", n)
}

// scanChunks streams r in ~8MB chunks, carrying a small tail overlap so regex
// matches are never split across a chunk boundary.
func scanChunks(r io.Reader, fn func([]byte)) {
	const chunk = 8 << 20
	const overlap = 256
	buf := make([]byte, chunk+overlap)
	tail := 0
	for {
		n, err := io.ReadFull(r, buf[tail:chunk+overlap])
		total := tail + n
		if total > 0 {
			fn(buf[:total])
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return
		}
		if err != nil {
			return
		}
		// carry the last `overlap` bytes so a boundary-straddling tuple is re-seen
		copy(buf, buf[total-overlap:total])
		tail = overlap
	}
}
