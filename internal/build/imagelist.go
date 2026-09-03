package build

import (
	"bufio"
	"compress/gzip"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/szatmary/filmstock/internal/record"
	"github.com/szatmary/filmstock/internal/sqldrv"
)

// Which wiki a file lives on, so image URLs can be built without asking.
//
// Wikimedia file URLs are deterministic — upload.wikimedia.org/wikipedia/
// {repo}/{md5[0]}/{md5[0:2]}/{name} — and the ONLY unknown is the repo: free
// files live on commons, non-free posters locally on en. Special:FilePath
// exists to answer exactly that question per request, which is why it is an
// expensive special page and throttles at about one request per two seconds.
// Publishing FilePath URLs therefore pushed a resolve-and-cache job onto every
// consumer, which is this pipeline's job, not theirs.
//
// The enwiki dump's image.sql.gz lists every LOCAL file — 83 MB answering the
// one question — so membership decides the repo offline and the published URL
// is the CDN's, final, with no special page anywhere.
var reImgName = regexp.MustCompile(`\('((?:[^'\\]|\\.)+)',`)

// CBuildImageList loads the local-file list into the resolver cache.
func CBuildImageList(args []string) {
	fs := flag.NewFlagSet("build-image-list", flag.ExitOnError)
	sqlPath := fs.String("sql", "", "enwiki image.sql.gz")
	dbPath := fs.String("db", defaultCachePath(), "resolver cache to write local_images into")
	fs.Parse(args)
	if *sqlPath == "" {
		fatal(fmt.Errorf("build-image-list needs -sql FILE"))
	}
	f, err := os.Open(*sqlPath)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		fatal(err)
	}
	db, err := sql.Open(sqldrv.Name, *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`PRAGMA journal_mode=OFF`, `PRAGMA synchronous=OFF`,
		`DROP TABLE IF EXISTS local_images`,
		`CREATE TABLE local_images(name TEXT PRIMARY KEY) WITHOUT ROWID`,
	} {
		if _, err := db.Exec(q); err != nil {
			fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		fatal(err)
	}
	ins, err := tx.Prepare(`INSERT OR IGNORE INTO local_images(name) VALUES(?)`)
	if err != nil {
		fatal(err)
	}

	start := time.Now()
	n := 0
	// Chunked scan with a carried tail, the same shape as the qidmap loader: a
	// tuple can straddle a chunk boundary, and losing the one that does means
	// one file quietly resolving to the wrong wiki.
	buf := make([]byte, 1<<20)
	tail := ""
	for {
		read, rerr := zr.Read(buf)
		chunk := tail + string(buf[:read])
		ms := reImgName.FindAllStringSubmatchIndex(chunk, -1)
		last := 0
		for _, m := range ms {
			name := chunk[m[2]:m[3]]
			name = strings.ReplaceAll(name, `\'`, `'`)
			name = strings.ReplaceAll(name, `\"`, `"`)
			name = strings.ReplaceAll(name, `\\`, `\`)
			if _, err := ins.Exec(name); err != nil {
				fatal(err)
			}
			n++
			last = m[1]
		}
		if len(chunk)-last < 4096 {
			tail = chunk[last:]
		} else {
			tail = chunk[len(chunk)-4096:]
		}
		if rerr != nil {
			break
		}
	}
	ins.Close()
	if err := tx.Commit(); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  %d local files -> local_images in %.1fs\n",
		n, time.Since(start).Seconds())
}

// loadLocalImages reads the set back for URL building. Nil when the table has
// not been built, which callers treat as "fall back to Special:FilePath".
func loadLocalImages(cachePath string) map[string]bool {
	if !tableExists(cachePath, "local_images", "") {
		return nil
	}
	db, err := sql.Open(sqldrv.Name, cachePath)
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM local_images`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[string]bool, 1<<20)
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			out[s] = true
		}
	}
	return out
}

// cdnImageURL builds the final upload.wikimedia.org URL for a filename.
//
// MediaWiki's hashed layout: md5 of the canonical name (spaces to underscores,
// first letter upper-cased), then /{h[0]}/{h[0:2]}/{name}. Local files serve
// from wikipedia/en, everything else from wikipedia/commons — membership in
// local_images is the whole decision. With no local set, Special:FilePath is
// the fallback: still correct, merely the throttled indirection this exists to
// remove.
func cdnImageURL(filename string, local map[string]bool) string {
	name, fp := record.CoverImageURL(filename)
	if name == "" {
		return ""
	}
	if local == nil {
		return fp
	}
	canon := strings.ReplaceAll(name, " ", "_")
	r, size := utf8.DecodeRuneInString(canon)
	if size > 0 && r != utf8.RuneError {
		canon = string(unicode.ToUpper(r)) + canon[size:]
	}
	repo := "commons"
	if local[canon] {
		repo = "en"
	}
	h := md5.Sum([]byte(canon))
	hx := hex.EncodeToString(h[:])
	return "https://upload.wikimedia.org/wikipedia/" + repo + "/" +
		hx[:1] + "/" + hx[:2] + "/" + url.PathEscape(canon)
}
