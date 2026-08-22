package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"

	_ "modernc.org/sqlite"
)

// Retrieval evaluation.
//
// This exists BEFORE any embedding work, on purpose. The three ways an embedding
// pipeline fails quietly — pooling that doesn't match how the model was trained,
// omitting the "query:"/"passage:" prefixes an asymmetric model expects, and
// mixing model versions between documents and queries — all produce vectors that
// look perfectly normal and merely rank worse. Without a fixed query set and a
// baseline score, none of them is distinguishable from "this model is mediocre",
// and every tuning decision afterwards is guesswork.
//
// Scoring the CURRENT lexical search first also gives the number semantic search
// has to beat. Queries are deliberately worded to avoid the title, so a purely
// lexical engine should do badly — if it doesn't, the query set is too easy.

type evalQuery struct {
	Category string `json:"category"`
	Query    string `json:"query"`
	Relevant []struct {
		PageID int    `json:"page_id"`
		Title  string `json:"title"`
	} `json:"relevant"`
}

type evalSet struct {
	Queries []evalQuery `json:"queries"`
}

// retriever returns ranked page_ids for a query. Any retrieval strategy —
// lexical, semantic, or fused — plugs in here and is scored identically.
type retriever func(q string, n int) ([]int, error)

func cmdEval(args []string) {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	dbPath := fs.String("db", "out/search.db", "search database")
	setPath := fs.String("queries", "docs/eval/queries.json", "query set")
	n := fs.Int("n", 20, "retrieval depth")
	verbose := fs.Bool("v", false, "show the rank of each expected answer")
	fs.Parse(args)

	raw, err := os.ReadFile(*setPath)
	if err != nil {
		fatal(err)
	}
	var set evalSet
	if err := json.Unmarshal(raw, &set); err != nil {
		fatal(err)
	}
	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	lexical := func(q string, n int) ([]int, error) {
		res, err := searchMovies(context.Background(), db, q, "", n)
		if err != nil {
			return nil, err
		}
		ids := make([]int, 0, len(res))
		for _, r := range res {
			ids = append(ids, r.ID)
		}
		return ids, nil
	}

	score(set, "lexical (trigram + Dice)", lexical, *n, *verbose)
}

// score reports recall@1/5/N and MRR, broken down BY CATEGORY.
//
// The breakdown is the point: lexical and semantic retrieval fail in opposite
// directions, and a single aggregate number hides that. Lexical should win
// titles and typos, semantic should win concepts, and fusion is only worth
// having if it keeps both — which cannot be seen without splitting them out.
func score(set evalSet, name string, r retriever, n int, verbose bool) {
	var hits1, hits5, hitsN, total int
	var mrrSum float64
	type agg struct {
		q, rel, hit1, hit5, hitN int
		mrr                      float64
	}
	byCat := map[string]*agg{}
	var cats []string

	fmt.Printf("\n%s  (depth %d, %d queries)\n", name, n, len(set.Queries))
	fmt.Println("--------------------------------------------------------------")
	for _, q := range set.Queries {
		ids, err := r(q.Query, n)
		if err != nil {
			fmt.Printf("  ERROR %q: %v\n", q.Query, err)
			continue
		}
		ca := byCat[q.Category]
		if ca == nil {
			ca = &agg{}
			byCat[q.Category] = ca
			cats = append(cats, q.Category)
		}
		ca.q++
		ca.rel += len(q.Relevant) // recall denominators count ANSWERS, not queries
		rank := map[int]int{}
		for i, id := range ids {
			if _, seen := rank[id]; !seen {
				rank[id] = i + 1
			}
		}
		best := 0
		for _, want := range q.Relevant {
			total++
			pos, ok := rank[want.PageID]
			if !ok {
				continue
			}
			if pos <= 1 {
				hits1++
				ca.hit1++
			}
			if pos <= 5 {
				hits5++
				ca.hit5++
			}
			hitsN++
			ca.hitN++
			if best == 0 || pos < best {
				best = pos
			}
		}
		if best > 0 {
			mrrSum += 1 / float64(best)
			ca.mrr += 1 / float64(best)
		}
		if verbose {
			where := "MISS"
			if best > 0 {
				where = fmt.Sprintf("rank %d", best)
			}
			fmt.Printf("  %-46s %s\n", truncate(q.Query, 46), where)
		}
	}
	q := float64(len(set.Queries))
	fmt.Println("--------------------------------------------------------------")
	sort.Strings(cats)
	for _, cat := range cats {
		a := byCat[cat]
		// Recall is over relevant ITEMS; MRR is over queries (it uses each
		// query's best rank). Dividing recall by the query count reported
		// person r@20 = 109.4% the moment gold sets stopped being singletons.
		rel := float64(max(a.rel, 1))
		fmt.Printf("    %-12s n=%2d  rel=%3d  r@1 %5.1f%%  r@5 %5.1f%%  r@%d %5.1f%%  MRR %.3f\n",
			cat, a.q, a.rel, 100*float64(a.hit1)/rel, 100*float64(a.hit5)/rel,
			n, 100*float64(a.hitN)/rel, a.mrr/float64(a.q))
	}
	fmt.Printf("  OVERALL      n=%2d  r@1 %5.1f%%  r@5 %5.1f%%  r@%d %5.1f%%  MRR %.3f\n",
		len(set.Queries), 100*float64(hits1)/float64(total), 100*float64(hits5)/float64(total),
		n, 100*float64(hitsN)/float64(total), mrrSum/q)
}

// scoreVector adds the semantic and fused retrievers to the evaluation. The
// query vectors come from embed_queries.py, which applies each model's required
// query prefix — a detail that fails silently when wrong, which is why the eval
// exists at all.
func cmdEvalVector(args []string) {
	fs := flag.NewFlagSet("eval-vec", flag.ExitOnError)
	dbPath := fs.String("db", "out/search.db", "search database")
	setPath := fs.String("queries", "docs/eval/queries.json", "query set")
	manifest := fs.String("quant", "", "quant.<model>.json (required)")
	idsPath := fs.String("ids", "out/index/passages.bin", "passage -> page_id map")
	qvecs := fs.String("qvecs", "", "query vectors from embed_queries.py (required)")
	dim := fs.Int("dim", 1024, "embedding dimension")
	n := fs.Int("n", 20, "retrieval depth")
	coarse := fs.Int("coarse", 2000, "int2 candidates before rerank")
	label := fs.String("label", "semantic", "name for the report")
	fs.Parse(args)

	raw, err := os.ReadFile(*setPath)
	if err != nil {
		fatal(err)
	}
	var set evalSet
	if err := json.Unmarshal(raw, &set); err != nil {
		fatal(err)
	}
	ix, err := loadQuantIndex(*manifest, *idsPath)
	if err != nil {
		fatal(err)
	}
	qraw, err := os.ReadFile(*qvecs)
	if err != nil {
		fatal(err)
	}
	nq := len(qraw) / (*dim * 4)
	if nq != len(set.Queries) {
		fatal(fmt.Errorf("query vectors hold %d rows but the set has %d queries", nq, len(set.Queries)))
	}
	qv := make([][]float32, nq)
	for i := range qv {
		qv[i] = make([]float32, *dim)
		for d := 0; d < *dim; d++ {
			off := (i**dim + d) * 4
			qv[i][d] = math.Float32frombits(binary.LittleEndian.Uint32(qraw[off:]))
		}
	}

	byQuery := map[string]int{}
	for i, q := range set.Queries {
		byQuery[q.Query] = i
	}
	semantic := func(q string, topn int) ([]int, error) {
		i, ok := byQuery[q]
		if !ok {
			return nil, fmt.Errorf("no vector for %q", q)
		}
		hits := ix.Search(qv[i], *coarse, topn)
		ids := make([]int, len(hits))
		for j, h := range hits {
			ids[j] = h.PageID
		}
		return ids, nil
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	lexical := func(q string, topn int) ([]int, error) {
		res, err := searchMovies(context.Background(), db, q, "", topn)
		if err != nil {
			return nil, err
		}
		ids := make([]int, 0, len(res))
		for _, r := range res {
			ids = append(ids, r.ID)
		}
		return ids, nil
	}
	fused := func(q string, topn int) ([]int, error) {
		lex, err := lexical(q, topn)
		if err != nil {
			return nil, err
		}
		sem, err := semantic(q, topn)
		if err != nil {
			return nil, err
		}
		f := fuseRRF([]rankedList{{ids: lex}, {ids: sem}}, nil, rrfK)
		ids := make([]int, 0, len(f))
		for _, h := range f {
			ids = append(ids, h.PageID)
		}
		return ids, nil
	}

	score(set, *label+" (int2 scan -> int8 rerank)", semantic, *n, true)
	score(set, *label+" + lexical, RRF fused", fused, *n, false)
}
