package build

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// Composing the shipped embedding from text AND the structured data.
//
// The text model is the right tool for what is genuinely unstructured — plot,
// tone, the things only prose states. But genre, decade and their kin were
// squeezed through the same pipe: rendered into an English header and pushed
// through a language model in the hope it would re-infer categories we already
// held cleanly. A lossy round trip for data that started structured.
//
// Measured cost of that trip: Galaxy Quest's neighbours were five Star Trek
// films — the plot prose is full of spaceships, and "comedy" is a register the
// plot never states. With a 28-dim genre block appended, its neighbours are
// Spaceballs, The Hitchhiker's Guide to the Galaxy, Innerspace and Star Trek IV
// (the funny one), and Blade Runner's gain The Matrix and Total Recall.
//
// So the shipped vector is a concatenation of blocks, weighted and then
// normalised as one:
//
//	[ text · wText | genre · wGenre | decade · wDecade ]
//
//	text     1024   mean of the lead-only and passage-centroid embeddings.
//	                Lead-only finds sci-fi comedies for Galaxy Quest and loses
//	                Philip K. Dick for Blade Runner; the centroid does the
//	                reverse; the mean keeps both. Measured, not assumed.
//	genre      28   one NAMED dim per canonical genre, straight from the
//	                database. The vocabulary is closed and word-boundary
//	                canonicalised, which is what makes a dim per genre sane.
//	decade      1   year/2020, weighted low: a nudge, not a wall between eras.
//
// The blend lives IN the vector, not in a layer consumers must reassemble —
// one file, one vector per film, good defaults baked in. Nothing is lost by
// baking: the blocks keep their positions, so "more comedy" is still a push on
// one named dimension at query time. That control is structural, not inferred:
// it means the same thing at every position, unlike a local PCA axis, because
// the data states it rather than a neighbourhood implying it.
//
// A sidecar JSON names every dim past the text block, so a consumer can find
// the comedy dimension without knowing this build.
func CmdComposeVectors(args []string) {
	fs := flag.NewFlagSet("compose-vectors", flag.ExitOnError)
	leadPath := fs.String("lead", "", "lead-only float32 embeddings (count × dims)")
	centPath := fs.String("centroid", "", "passage-centroid float32 embeddings (count × dims)")
	idsPath := fs.String("ids", "", "int32 page_id per row")
	dbPath := fs.String("db", "", "published database, for genre and year")
	textDims := fs.Int("text-dims", 1024, "dims of the text embeddings")
	outF32 := fs.String("out-f32", "composed.f32.bin", "composed float32 output")
	outDims := fs.String("out-dims", "composed.dims.json", "names of the structured dims")
	wText := fs.Float64("w-text", 1.0, "text block weight")
	wGenre := fs.Float64("w-genre", 0.35, "genre block weight")
	wDecade := fs.Float64("w-decade", 0.05, "decade weight")
	fs.Parse(args)
	if *leadPath == "" || *centPath == "" || *idsPath == "" || *dbPath == "" {
		fatal(fmt.Errorf("compose-vectors needs -lead, -centroid, -ids and -db"))
	}

	idb, err := os.ReadFile(*idsPath)
	if err != nil {
		fatal(err)
	}
	count := len(idb) / 4
	ids := make([]int32, count)
	for i := range ids {
		ids[i] = int32(binary.LittleEndian.Uint32(idb[i*4:]))
	}
	lead, err := readF32(*leadPath, count, *textDims)
	if err != nil {
		fatal(err)
	}
	cent, err := readF32(*centPath, count, *textDims)
	if err != nil {
		fatal(err)
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	genres, years, vocab, err := loadFacets(db)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  %d films, %d genre dims: %s...\n",
		count, len(vocab), strings.Join(vocab[:min(6, len(vocab))], ", "))

	dims := *textDims + len(vocab) + 1
	out, err := os.Create(*outF32)
	if err != nil {
		fatal(err)
	}
	defer out.Close()

	row := make([]float32, dims)
	buf := make([]byte, dims*4)
	var noGenre int
	for r := range count {
		// Text block: the mean of the two poolings, unit-normalised, weighted.
		var tn float64
		for d := range *textDims {
			v := (lead[r*(*textDims)+d] + cent[r*(*textDims)+d]) / 2
			row[d] = v
			tn += float64(v) * float64(v)
		}
		scaleBlock(row[:*textDims], tn, *wText)

		// Genre block: unit-normalised multi-hot, so a one-genre film and a
		// four-genre film carry the same total genre weight.
		g := genres[int(ids[r])]
		var gn float64
		for d := range vocab {
			v := float32(0)
			if g != nil && g[d] {
				v = 1
			}
			row[*textDims+d] = v
			gn += float64(v) * float64(v)
		}
		if gn == 0 {
			noGenre++
		}
		scaleBlock(row[*textDims:*textDims+len(vocab)], gn, *wGenre)

		row[dims-1] = float32(*wDecade) * float32(years[int(ids[r])]) / 2020

		// One normalisation over the whole vector, so cosine similarity is
		// exactly the weighted blend of the blocks.
		var n float64
		for _, v := range row {
			n += float64(v) * float64(v)
		}
		scaleBlock(row, n, 1)

		for d, v := range row {
			binary.LittleEndian.PutUint32(buf[d*4:], math.Float32bits(v))
		}
		if _, err := out.Write(buf); err != nil {
			fatal(err)
		}
	}

	names := map[string]any{
		"text_dims": *textDims,
		"weights":   map[string]float64{"text": *wText, "genre": *wGenre, "decade": *wDecade},
		"dims":      map[string]int{},
	}
	dm := names["dims"].(map[string]int)
	for i, g := range vocab {
		dm["genre:"+g] = *textDims + i
	}
	dm["decade"] = dims - 1
	nb, _ := json.MarshalIndent(names, "", "  ")
	if err := os.WriteFile(*outDims, nb, 0o644); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "  wrote %s: %d × %d (%d films with no genre carry text only)\n",
		*outF32, count, dims, noGenre)
	fmt.Fprintf(os.Stderr, "  dim names -> %s\n", *outDims)
	fmt.Fprintf(os.Stderr, "  quantise with: filmstock vectors -in %s -ids %s -dims %d -scale SCALE -new-epoch\n",
		*outF32, *idsPath, dims)
}

func scaleBlock(b []float32, sumsq float64, w float64) {
	if sumsq <= 0 {
		return
	}
	f := float32(w / math.Sqrt(sumsq))
	for i := range b {
		b[i] *= f
	}
}

func readF32(path string, count, dims int) ([]float32, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) != count*dims*4 {
		return nil, fmt.Errorf("%s is %d bytes; want %d rows × %d dims × 4 = %d",
			path, len(raw), count, dims, count*dims*4)
	}
	out := make([]float32, count*dims)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return out, nil
}

// loadFacets reads genre membership and years from the published database. The
// genre vocabulary is whatever the corpus states — a closed set of 28, but read
// rather than hard-coded so a new canonical genre flows through on its own.
func loadFacets(db *sql.DB) (map[int][]bool, map[int]int, []string, error) {
	type film struct {
		genres []string
		year   int
	}
	films := map[int]film{}
	seen := map[string]bool{}
	rows, err := db.Query(`SELECT id, genre, year FROM movies`)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, year int
		var g string
		if err := rows.Scan(&id, &g, &year); err != nil {
			return nil, nil, nil, err
		}
		var gs []string
		for _, t := range strings.Split(g, "·") {
			if t = strings.TrimSpace(t); t != "" {
				gs = append(gs, t)
				seen[t] = true
			}
		}
		films[id] = film{gs, year}
	}
	vocab := make([]string, 0, len(seen))
	for g := range seen {
		vocab = append(vocab, g)
	}
	sort.Strings(vocab)
	gi := map[string]int{}
	for i, g := range vocab {
		gi[g] = i
	}
	genres := make(map[int][]bool, len(films))
	years := make(map[int]int, len(films))
	for id, f := range films {
		years[id] = f.year
		if len(f.genres) == 0 {
			continue
		}
		b := make([]bool, len(vocab))
		for _, g := range f.genres {
			b[gi[g]] = true
		}
		genres[id] = b
	}
	return genres, years, vocab, rows.Err()
}
