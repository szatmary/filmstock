package filmstock

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Hashing what a database MEANS, not how it is stored.
//
// A consumer who applies daily patches holds the same facts as a freshly built
// file but different bytes: different page layout, different freelist,
// different AUTOINCREMENT counters. File hashes can only verify a download;
// convergence after a patch needs a hash over content — rows in a canonical
// order, synthetic ids excluded — so that "you now match the 20260820 full" is
// checkable by anyone, which is the promise the bridge diff makes.
//
// Every hashed table has an explicit canonical query. Synthetic AUTOINCREMENT
// keys are excluded and replaced with a stable ordering, because they renumber
// wholesale between builds while meaning nothing; credits reach through to the
// person's link target for the same reason. FTS tables are skipped (declared
// empty, rebuilt locally) and person_alias is skipped as derived. An
// unrecognised table is an ERROR, not a shrug: a new table hashed by guessed
// rules would let two databases agree on a number while disagreeing on facts.
//
// ContentHashVersion changes whenever the canonical queries do; hashes from
// different versions are not comparable and manifests must say which they used.
const ContentHashVersion = 1

// contentSpecs maps each table to its canonical row stream.
var contentSpecs = map[string]string{
	// page_id-keyed: the id IS the identity, so natural order and all columns.
	"movies": `SELECT id,title,year,release_date,director,producer,writer,starring,
	   music,distributor,country,language,genre,runtime,budget,gross,wikipedia_url,
	   cover_image_url,cover_image_file,wiki_title FROM movies ORDER BY id`,
	"television_series": `SELECT id,title,year,first_aired,last_aired,genre,creator,
	   starring,network,num_seasons,num_episodes,seasons_count,episodes_count,
	   cover_image_file,cover_image_url,wikipedia_url,wiki_title
	   FROM television_series ORDER BY id`,
	"events": `SELECT id,title,kind,award,edition,date,year,hosts,organizer,venue,
	   location,network,best_film,most_wins,opening_film,closing_film,
	   cover_image_file,wikipedia_url FROM events ORDER BY id`,
	"schedules":    `SELECT id,title,season,daypart,wikipedia_url FROM schedules ORDER BY id`,
	"external_ids": `SELECT id,kind,source,value FROM external_ids ORDER BY id,kind,source,value`,
	"franchises":   `SELECT id,qid,title FROM franchises ORDER BY id`,
	"franchise_members": `SELECT franchise_id,id,kind FROM franchise_members
	   ORDER BY franchise_id,id,kind`,
	"sequels": `SELECT id,kind,next_id FROM sequels ORDER BY id,kind,next_id`,

	// AUTOINCREMENT-keyed: the id renumbers between builds and is excluded;
	// order comes from what the row states.
	"television_seasons": `SELECT series_id,season,page_id,num_episodes,first_aired,
	   last_aired,network,starring,image,rank,rating,viewers
	   FROM television_seasons ORDER BY series_id,season`,
	"television_episodes": `SELECT series_id,season,number_overall,number_in_season,
	   title,air_date,viewers FROM television_episodes
	   ORDER BY series_id,season,number_overall,number_in_season,title,air_date`,
	"schedule_slots": `SELECT schedule_id,day,network,start,end,part,title,show_id,
	   rerun,rank,rating FROM schedule_slots
	   ORDER BY schedule_id,day,network,start,part,title,end`,

	// people are identified by their link target; credits reach through to it
	// so the synthetic person row id never enters the hash.
	"people": `SELECT page_id,qid,name,wiki,image_url FROM people ORDER BY wiki,page_id`,
	"credits": `SELECT p.wiki,c.work_id,c.work_type,c.role FROM credits c
	   JOIN people p ON p.id=c.person_id
	   ORDER BY p.wiki,c.work_type,c.work_id,c.role`,

	// the synopsis database
	"movie_text":      `SELECT id,overview,plot FROM movie_text ORDER BY id`,
	"television_text": `SELECT id,overview,plot FROM television_text ORDER BY id`,
	"episode_text": `SELECT series_id,summary FROM episode_text
	   ORDER BY series_id,summary`,

	// the vectors database
	"vectors":     `SELECT id,v FROM vectors ORDER BY id`,
	"vector_meta": `SELECT model,dims,count,lo,span FROM vector_meta ORDER BY model`,
}

// contentSkip are tables that are derived or deliberately unshipped, and so
// carry no facts of their own.
func contentSkip(name string) bool {
	if strings.HasPrefix(name, "sqlite_") || name == "person_alias" || name == "meta" {
		return true
	}
	for _, f := range FTSTables {
		if name == f || strings.HasPrefix(name, f+"_") {
			return true
		}
	}
	return false
}

// ContentHash hashes every content table, returning the per-table digests and
// a total. The total is the SHA-256 of "name digest" lines sorted by name, so
// two databases agree on the total exactly when they agree on every table.
func ContentHash(h *sql.DB) (total string, tables map[string]string, err error) {
	rows, err := h.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return "", nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return "", nil, err
		}
		if !contentSkip(n) {
			names = append(names, n)
		}
	}
	rows.Close()
	sort.Strings(names)

	tables = make(map[string]string, len(names))
	for _, name := range names {
		q, ok := contentSpecs[name]
		if !ok {
			return "", nil, fmt.Errorf(
				"filmstock: no canonical query for table %q — a new table must be "+
					"given one (and ContentHashVersion bumped) before it can be hashed", name)
		}
		d, err := hashQuery(h, q)
		if err != nil {
			return "", nil, fmt.Errorf("filmstock: hashing %s: %w", name, err)
		}
		tables[name] = d
	}
	sum := sha256.New()
	fmt.Fprintf(sum, "content-hash v%d\n", ContentHashVersion)
	for _, name := range names {
		fmt.Fprintf(sum, "%s %s\n", name, tables[name])
	}
	return hex.EncodeToString(sum.Sum(nil)), tables, nil
}

// hashQuery streams a query's rows into one digest. Values are serialised with
// a type tag so that 1, "1" and NULL cannot collide, and rows and fields carry
// explicit separators so field boundaries cannot shift.
func hashQuery(h *sql.DB, q string) (string, error) {
	rows, err := h.Query(q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		for _, v := range vals {
			switch x := v.(type) {
			case nil:
				sum.Write([]byte("n"))
			case int64:
				sum.Write([]byte("i" + strconv.FormatInt(x, 10)))
			case float64:
				sum.Write([]byte("f" + strconv.FormatFloat(x, 'g', -1, 64)))
			case string:
				sum.Write([]byte("t"))
				sum.Write([]byte(x))
			case []byte:
				sum.Write([]byte("b"))
				sum.Write(x)
			default:
				return "", fmt.Errorf("unhashable value %T", v)
			}
			sum.Write([]byte{0x1f})
		}
		sum.Write([]byte{0x1e})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
