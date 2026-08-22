# Retrieval experiments

Everything on this branch is the search-quality work: dense embeddings, ColBERT
late interaction, quantisation, cross-encoder reranking, and RRF fusion. `main`
is the build pipeline and does not need any of it.

The Go code for these paths ships on `main` too — they are opt-in `serve` flags,
off by default — so no file diverges between the two branches. What lives only
here is the Python side, the sweep scripts, and the logs they produced.

## Results

The numbers, and the conclusions drawn from them, are in
[../docs/TODO.md](../docs/TODO.md). Read that before re-running anything: several
of these experiments are settled and rerunning them is wasted GPU time.

| path | MRR | concept | conjunctive | person | title | typo |
|---|---|---|---|---|---|---|
| lexical | 0.198 | 0.000 | 0.002 | 0.002 | 1.000 | 0.884 |
| dense (bge-large, int2→int8) | 0.382 | 0.217 | 0.511 | 0.206 | 0.938 | 0.630 |
| **ColBERT rerank** | **0.575** | 0.426 | 0.858 | 0.474 | 0.977 | 0.553 |
| RRF fused (lexical+ColBERT) | 0.508 | 0.297 | 0.781 | 0.360 | 0.983 | 0.783 |

415-query eval set, depth 20. What the table says: **fusion loses** to its own
better input, and no single path wins everywhere — lexical owns title (1.000) and
typo (0.884), ColBERT owns everything else. That is an argument for routing by
query shape, not for blending.

## What is here

```
embed/
  embed.py          offline document embedding on the GB10; writes run metrics as JSON
  colbert.py        ColBERT encode + quantise + query-token serve
  colbert_ref.py    float32 reference, to prove the quantised store reproduces it
  embed_queries.py  embed the eval queries with the same model as the documents
  genqueries.py     eval-set generation
  rerank.py         cross-encoder sidecar
  testui.py         query-vector service the Go server calls
  bakeoff.sh        model comparison
experiments/
  rebuild_all.sh    full pipeline: extract → chunk → embed → quantise → score
  colbert.sh        the ColBERT build and sweep
  quantall.sh       quantise every model at every bit depth
  step2.sh          section-aware chunking + headers + lean profile (REGRESSED — see TODO)
  logs/             what each run actually printed
```

## Running anything

Every job here needs the venv and a built binary:

```sh
make build                      # from the repo root
./venv/bin/python embed/embed.py --model BAAI/bge-large-en-v1.5 --out out/index
```

The venv is not tracked — it is ~3.3 GB of PyTorch. Long jobs record wall time,
CPU, GPU utilisation and peak RSS alongside their output; that instrumentation is
the point, not decoration. Measure, do not assert.
