package build

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/szatmary/filmstock"
)

func CSearch(args []string) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	dbPath := fs.String("db", "out/search.db", "SQLite database")
	n := fs.Int("n", 20, "max results")
	field := fs.String("field", "title", "field to rank against: title|starring|director")
	fs.Parse(args)
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		fmt.Fprintln(os.Stderr, "usage: filmstock search [-n N] [-field title|starring|director] QUERY")
		os.Exit(2)
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	results, err := filmstock.SearchMovies(context.Background(), db, query, *field, *n)
	if err != nil {
		fatal(err)
	}
	if len(results) == 0 {
		fmt.Println("no matches.")
		return
	}
	for _, r := range results {
		yr := ""
		if r.Year > 0 {
			yr = fmt.Sprintf(" (%d)", r.Year)
		}
		fmt.Printf("%.2f  %s%s\n", r.Score, r.Title, yr)
		if r.Director != "" {
			fmt.Printf("        dir: %s\n", r.Director)
		}
		if r.Starring != "" {
			fmt.Printf("        cast: %s\n", filmstock.Truncate(r.Starring, 80))
		}
	}
}
