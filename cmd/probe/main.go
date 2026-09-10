package main

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/szatmary/filmstock/internal/sqldrv"
)

func main() {
	db, err := sql.Open(sqldrv.Name, sqldrv.DSN(os.Args[1], true))
	if err != nil {
		panic(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var ids []int64
	rows, err := db.Query(`SELECT page_id FROM pages LIMIT 2000`)
	if err != nil {
		panic(err)
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	fmt.Println("sample ids:", len(ids))

	// The exact query importIncr runs per page that yields no claims.
	st, err := db.Prepare(`SELECT kind FROM pages WHERE page_id = ?`)
	if err != nil {
		panic(err)
	}
	defer st.Close()
	t := time.Now()
	n := 0
	for _, id := range ids {
		r, err := st.Query(id)
		if err != nil {
			panic(err)
		}
		for r.Next() {
			var k string
			r.Scan(&k)
			n++
		}
		r.Close()
	}
	d := time.Since(t)
	fmt.Printf("Kinds x%d: %s  (%.0f lookups/s, %d rows)\n",
		len(ids), d.Round(time.Millisecond), float64(len(ids))/d.Seconds(), n)
}
