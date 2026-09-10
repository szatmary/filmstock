package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/szatmary/filmstock"
)

func main() {
	dir := os.Args[1]
	log.SetOutput(os.Stdout)
	log.SetFlags(0)

	t := time.Now()
	core, build, changed, err := filmstock.Update(context.Background(), filmstock.DefaultBaseURL, dir)
	if err != nil {
		fmt.Println("UPDATE FAILED:", err)
		os.Exit(1)
	}
	fmt.Printf("\n== fresh install ==\n  build=%s changed=%v in %s\n  %s\n",
		build, changed, time.Since(t).Round(time.Second), core)

	db, err := filmstock.Open(core)
	if err != nil {
		fmt.Println("OPEN FAILED:", err)
		os.Exit(1)
	}
	defer db.Close()
	var films, series, eps, people int
	db.SQL().QueryRow(`SELECT (SELECT count(*) FROM movies),(SELECT count(*) FROM television_series),
	   (SELECT count(*) FROM television_episodes),(SELECT count(*) FROM people)`).Scan(&films, &series, &eps, &people)
	fmt.Printf("  films=%d series=%d episodes=%d people=%d\n", films, series, eps, people)

	var title string
	db.SQL().QueryRow(`SELECT title FROM television_series WHERE id=156045`).Scan(&title)
	fmt.Printf("  spot check 156045 = %q\n", title)

	// Second call: already current, must be a no-op.
	t = time.Now()
	_, build2, changed2, err := filmstock.Update(context.Background(), filmstock.DefaultBaseURL, dir)
	fmt.Printf("\n== second call ==\n  build=%s changed=%v err=%v in %s\n",
		build2, changed2, err, time.Since(t).Round(time.Millisecond))
	if changed2 {
		fmt.Println("  BUG: reported a change when already current")
	}
}
