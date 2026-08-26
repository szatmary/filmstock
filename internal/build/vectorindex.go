package build

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"flag"
	"fmt"
	"os"

	"github.com/szatmary/filmstock"
)

// Embedding vectors, one row per work.
//
// Per row rather than one contiguous matrix, which is how the file on disk
// holds them. A single 175 MB blob would be rewritten in full every time one
// work changed, and the daily update is a patch of the few hundred records that
// actually moved — so the matrix form, which is ideal for a brute-force scan,
// is exactly wrong for a database that ships as daily diffs.
//
// A table of its own rather than a column on movies, so scanning the vectors
// does not drag every film's title and credits through the page cache with
// them. A consumer wanting brute-force search reads the whole table once.
//
// No extension is needed. 170,421 rows of 1024 int8 is well under a second to
// scan in Go, which is what filmstock.Vectors already does; sqlite-vec would
// buy SQL-level MATCH syntax and ANN indexing at a scale we are nowhere near,
// and would cost the file being openable without it.
const vectorSchema = `
DROP TABLE IF EXISTS vectors;
DROP TABLE IF EXISTS vector_meta;
-- NOT "WITHOUT ROWID", which costs four times the space here.
--
-- A WITHOUT ROWID table is an index B-tree, and index pages cap the payload
-- stored on the page itself at about 1002 bytes for a 4 KB page. A 1024-byte
-- vector exceeds that by 22 bytes, so every row spills into an overflow page of
-- its own: measured at 194,768 pages for 170,421 rows — a page each — and 760
-- MB for 175 MB of vectors. A rowid table allows around 4061 bytes on the page,
-- fitting three vectors to a page.
--
-- "id INTEGER PRIMARY KEY" IS the rowid, so nothing is given up: no second
-- index, no extra lookup.
CREATE TABLE vectors(
  id INTEGER PRIMARY KEY,   -- the work's page_id
  v  BLOB NOT NULL          -- dims × int8, quantised
);
-- The dequantisation, stored once rather than per row: dims × (lo, span) as
-- little-endian float32 pairs. A vector without it is uninterpretable, and so
-- is a vector without the model that produced it — there are three models'
-- files in this project's build directory and they are not comparable.
CREATE TABLE vector_meta(
  model TEXT NOT NULL,
  dims  INTEGER NOT NULL,
  count INTEGER NOT NULL,
  lo    BLOB NOT NULL,
  span  BLOB NOT NULL
);
`

// CIndexVectors loads an fsvec1 file into the database.
func CIndexVectors(args []string) {
	fs := flag.NewFlagSet("index-vectors", flag.ExitOnError)
	dbPath := fs.String("db", "index.db", "the database to add vectors to")
	vecPath := fs.String("vectors", "", "the fsvec1 file to load")
	model := fs.String("model", "", "the model that produced them, recorded with the vectors")
	fs.Parse(args)
	if *vecPath == "" {
		fatal(fmt.Errorf("index-vectors needs -vectors FILE"))
	}
	if *model == "" {
		fatal(fmt.Errorf("index-vectors needs -model NAME: a vector whose model is " +
			"unknown cannot be compared with anything"))
	}

	v, err := filmstock.OpenVectors(*vecPath)
	if err != nil {
		fatal(err)
	}
	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(vectorSchema); err != nil {
		fatal(err)
	}

	dims := v.Dims()
	lo, span := v.Scale()
	if _, err := db.Exec(`INSERT INTO vector_meta(model,dims,count,lo,span) VALUES(?,?,?,?,?)`,
		*model, dims, v.Len(), floats32(lo), floats32(span)); err != nil {
		fatal(err)
	}

	tx, err := db.Begin()
	if err != nil {
		fatal(err)
	}
	ins, err := tx.Prepare(`INSERT OR REPLACE INTO vectors(id,v) VALUES(?,?)`)
	if err != nil {
		fatal(err)
	}
	n := 0
	if err := v.Each(func(id int32, raw []uint8) error {
		if _, err := ins.Exec(int64(id), raw); err != nil {
			return err
		}
		n++
		return nil
	}); err != nil {
		fatal(err)
	}
	ins.Close()
	if err := tx.Commit(); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  %d vectors of %d dimensions from %s\n", n, dims, *model)
}

// floats32 serialises the dequantisation as little-endian float32, the layout
// the vector file itself uses.
func floats32(f []float32) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, f)
	return b.Bytes()
}
