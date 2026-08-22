# mediadb — Semantic Search / Embeddings Design

Status: **corpus chunked, embedding next.** Decisions below marked SUPERSEDED
were revised on 2026-08-04; the rest of the design stands.

## Decisions revised 2026-08-04 (these override the text below)

- **Target device is a Raspberry Pi 4, and 2-bit is the resident format.**
  Measured corpus: 170,421 docs -> **543,200 passages** (3.2/doc). At 1024 dims
  that is int2 **139 MB** resident, int8 556 MB mmap'd for rerank, float32 2.22 GB
  kept offline as the source of every other level. A full int2 scan touches 139 MB
  — roughly 40 ms of memory bandwidth on a Pi 4, so ~100-200 ms per query with the
  int8 rerank of the top ~1000 costing about 1 MB of random reads.

- **cgo IS allowed.** The no-cgo rule below was never a user constraint; it was
  assumed. Consequence: `sqlite-vec` becomes available, and NEON kernels are an
  option if Go's 2-bit scan is too slow.

- **SUPERSEDED — the query side is no longer a distilled static table.** §5's
  Model2Vec plan is dropped. With cgo, ggml/llama.cpp runs the REAL embedding
  model on the Pi with quantised weights (bge-m3 at Q4 is ~350 MB, a few hundred
  ms per short query). This removes the distillation step and with it the quality
  loss from compressing a 568M model into a lookup table, the risk of query and
  document vectors drifting apart, and the "static embeddings have a lower ceiling"
  caveat. Query and document embeddings are now the same model by construction;
  only the doc side runs on the GB10, purely for throughput.

- **Embeddings are ALWAYS built on the GB10**, so doc-model size is unconstrained.
  Its real selection criterion is therefore not benchmark score but how well it
  quantises for on-device query use.

- **Bake-off, not a guess.** `mediadb eval` exists and the lexical baseline is
  **0% recall / MRR 0.000** across 12 concept queries — they are worded to avoid
  titles, so trigram search cannot answer them and any semantic result is real
  signal. Candidates: bge-m3 (multilingual, 8k ctx), bge-large-en-v1.5 (English
  specialist), nomic-embed-text-v1.5 (Matryoshka, truncatable to 256 dims -> 35 MB
  at int2). Score each twice: float32 ceiling, and the deployed Pi configuration
  (Q4 query + int2 docs + int8 rerank). The GAP between those two numbers is the
  result that matters — it says whether 2-bit costs anything real.

- **Instrument every run**: wall time, throughput, sampled GPU util/memory, CPU,
  peak RSS, written as JSON beside the vectors (`embed/embed.py`).


This document explains, exactly, how semantic ("vector") search will work in
mediadb — what data feeds it, how text becomes vectors, how those vectors are
stored and searched, and how it stays runnable on small hardware.

---

## 0. The core idea in one paragraph

An **embedding** is a function that turns a piece of text into a fixed-length
list of numbers (a *vector*) — e.g. 384 floats — such that texts with similar
*meaning* land near each other in that 384-dimensional space. We embed every
movie's article once (offline), store the vectors, and at query time we embed
the user's question and return the movies whose vectors are *closest* (highest
cosine similarity). Because closeness is semantic, "movie about a hacker in a
simulation" can find *The Matrix* even though it shares no words with the plot.

---

## 1. Two separate data planes (do not conflate)

| Plane | Source | Contains | Where it lives | Who reads it |
|------|--------|----------|----------------|--------------|
| **Embedding corpus** | **Raw Wikipedia article** (full text) | Full cleaned prose — lead, plot, production, **reception/box office**, themes, … | `/tank/mediadb/text/<shard>/<pageid>.txt.gz` | Offline embedder (GB10) |
| **Browser record** | Our field extraction | title, credits, genre, dates, cover, **plot/overview** | `/tank/mediadb/movies/**.json.gz` + `movies.db` | The web server / user |

Key decision: **embeddings are built from the full raw article, not from our
narrow field extraction.** Reason: queries like *"1978 box office flops"* depend
on the *Reception / Box office* sections, which are not in the plot or infobox.
The browser record stays lean (plot/overview is welcome; the full text is not).

The corpus is kept on disk so we can re-chunk / re-embed with any model or
strategy **without re-parsing the 26 GB dump** each time (~2 GB gzipped for all
movies).

---

## 2. Pipeline overview

```
                      OFFLINE (once, on the GB10 / DGX Spark)
  raw dump ──parse──▶ text/<id>.txt.gz ──chunk──▶ passages ──embed──▶ float32 vectors
                                                                   │
                                                   ┌───────────────┴───────────────┐
                                                   ▼                               ▼
                                            quantize (int8 / binary)        keep float32 (rerank)
                                                   │
                                                   ▼
                                          ship compact index  ─────────────────────────┐
                                                                                        │
                      ON THE DEVICE (N100 / Raspberry Pi, CPU only)                     ▼
  user query ──embed query──▶ query vector ──ANN/brute-force──▶ top-K passages ──roll up to movies
                                     │                                                   │
                                     └──────── fuse with trigram + structured filters ───┘
                                                                                        ▼
                                                                                   ranked results
```

Everything expensive (embedding ~600k–1M docs, any fine-tuning) happens **once,
offline, on the GB10**. The device only ever embeds *one short query* and scans a
compact vector index.

---

## 3. How text becomes a vector (the mechanics)

1. **Chunk.** A full article is thousands of words, but embedding models accept
   only ~512 tokens (~400 words). So each article is split into overlapping
   passages of ~256–400 tokens (e.g. 320-token windows, 64-token overlap). Each
   passage is embedded separately. A movie therefore has **several** vectors
   (typically 5–30), each tagged with its `page_id`.
   - This is what lets *"1978 box office flops"* match the *Box office* passage
     specifically, instead of being diluted by the whole article.

2. **Embed.** Each passage → the model → one vector (e.g. 384 floats). The model
   is a pre-trained sentence encoder (see §5). Internally: text → subword tokens
   → transformer → mean-pooled hidden states → L2-normalized vector.

3. **Similarity.** With L2-normalized vectors, **cosine similarity = dot
   product**. "Nearest" = largest dot product. That's the only math at query
   time.

---

## 4. Quantization ladder (why it fits a Pi)

All levels are derived from the **same** float32 vectors in one offline pass —
producing several is nearly free. Each step roughly halves size. For ~1M passages
at 384 dims:

| Format | Bits/dim | Packing | Bytes/vec | Total (~1M) | On-device distance | Ranking quality |
|---|---|---|---|---|---|---|
| float32 | 32 | — | 1536 | ~1.5 GB | dot product | reference (truth) |
| **int8** | 8 | 1/byte | 384 | ~384 MB | int dot (SIMD) | ≈ lossless |
| **int4** | 4 | 2/byte | 192 | ~192 MB | unpack + dot | small drop (~1–3% recall) |
| **int2** | 2 | 4/byte | 96 | ~96 MB | unpack + dot | coarse alone |
| binary (int1) | 1 | 8/byte | 48 | ~48 MB | Hamming/popcount | coarse alone |

Scalar quantization: map each normalized value (≈[-1,1]) onto N levels with a
scale factor. **Per-dimension calibration** matters a lot at int4/int2; below int8
uniform scalar loses real signal, so low-bit levels are only good **with a rerank
stage** (or fancier product quantization — more code, deferred).

**You don't pick one level — you cascade** (coarse filter → fine rerank):

```
int2 / binary scan over ALL vectors  →  top ~1000 candidates   (single-digit ms, tiny RAM)
        ↓
int8 rerank of those 1000            →  top ~50                (recovers quality)
        ↓  (optional)
float32 rerank of top 50             →  final order            (exact where it counts)
```

At this scale a quantized **brute-force scan is fast enough** — no FAISS/HNSW
dependency — but HNSW can be added later if the corpus grows. **Matryoshka (MRL)**
dimension truncation (384→128) composes on top of bit-quantization for another ~3×.

## 4b. On-disk storage format

**Not GGUF, not vectors-in-SQLite.** GGUF is a *model-weights* format (llama.cpp)
— wrong tool for a vector corpus; it only appears here if the query encoder is run
via llama.cpp. Vectors-in-SQLite is out because the fast path (`sqlite-vec`) is a C
extension that pure-Go `modernc.org/sqlite` can't load, leaving only slow BLOB-row
scans.

Instead: **flat, memory-mapped binary blobs** (one per quant level) + a manifest,
with metadata staying in SQLite. This is how FAISS/hnswlib/USearch store indexes,
and it's ideal for pure-Go on a Pi.

```
index/
  manifest.json        # {model, dim, count, bits, packing, scale, created, corpus_sha}
  vectors.bin1.bin     # binary codes, row-major   (coarse scan)
  vectors.int8.bin     # int8 codes                (rerank)
  vectors.int4.bin     # (optional) int4 codes
  passages.bin         # int32[count]: row → page_id   (roll-up + filter join)
  offsets.bin          # (optional) int32[count]: char offset of the passage in text/<id>
```

- Each `vectors.*.bin` is `count × dim` packed codes back-to-back, row-major
  (row *i* = passage *i*). **mmap** it → the OS pages it in and search is a tight
  SIMD/bit loop over contiguous memory. Pure Go, no cgo.
- **Metadata + structured filters stay in `movies.db`** (year, genre, credits —
  already indexed). `passages.bin` maps a vector row back to its `page_id`, which
  is how a hit joins SQLite for the "1978" filter and fuses with trigram results.

| Thing | Stored as | Why |
|---|---|---|
| Doc vectors (int8/int4/int2/binary) | flat mmap'd `.bin` blobs | fastest brute-force, pure-Go, Pi-friendly |
| Passage → movie map | flat `int32` array (`passages.bin`) | roll-up + filter join |
| Movie metadata / filters | SQLite (`movies.db`) | already there; powers hybrid filters |
| Query encoder model | `.onnx` (or GGUF if via llama.cpp) | that's what GGUF is actually for |

The whole `index/` dir (a manifest + a few `.bin` files, tens–hundreds of MB) is
what ships to the N100/Pi.

---

## 5. The embedding model (query side is the only device cost)

Doc vectors are precomputed offline, so the **only** model that runs on the Pi is
the one embedding the *query string*. Options, cheapest first:

- **Static embeddings (Model2Vec / distilled):** a token→vector lookup table;
  embed a query by averaging its word vectors. **No neural net at inference** —
  microseconds, pure-CPU, trivially runnable on a Pi, and implementable in Go
  (no Python, no CGo). Lower ceiling than a transformer but strong with rerank.
- **int8 ONNX transformer (e.g. MiniLM / bge-small):** a real encoder, quantized.
  ~20–40 ms/query on N100, ~100–200 ms on a Pi. Higher quality.

Offline (GB10) we can use a *larger/better* model (e.g. `bge-base`, `e5-base`) to
embed the docs, and optionally distill it to a static table for the query side.
The doc and query models must produce compatible vectors (same model family, or a
distilled counterpart).

**Dimensions:** prefer a Matryoshka (MRL) model so we can truncate to 256 or 128
dims for smaller/faster indexes with minimal quality loss.

---

## 6. Query flow, with the "1978 box office flops" example

A great query because it's **hybrid** — semantic *and* structured:

1. **Parse structured filters** from the query where possible: `1978` → candidate
   filter `year = 1978` (or a range). This uses the metadata we already index.
2. **Semantic part:** embed `"box office flops"` → query vector.
3. **Retrieve:** binary Hamming scan over passage vectors → top ~500, restricted
   (or post-filtered) to movies with `year = 1978`; int8 rerank.
4. **Roll up** passage hits to movies; a movie whose *Box office* passage says
   "was a major box-office bomb" scores high.
5. **Fuse** with the existing trigram/lexical results via **Reciprocal Rank
   Fusion (RRF)**: `score(d) = Σ 1/(k + rank_i(d))` over each ranked list. Lexical
   wins exact titles; semantic wins concepts. Keep both — don't replace.

Result: 1978 films described as flops/bombs, even though "flop" may never appear
verbatim.

---

## 7. Do we need to *train* anything? Do we need an LLM?

**Neither, to start.**

*Training:* we *encode* with a pre-trained model; we don't train one. Pre-trained
encoders already understand Wikipedia prose.

*LLM:* an **embedding model is a small encoder** (20–100M params) that outputs a
vector — it is **not** a generative LLM, and vector search needs no LLM at all. A
generative LLM would only enter if we wanted the browser to *write answers* (RAG)
instead of *returning ranked results* — a separate feature that **won't run on an
N100/Pi** (GB10 or an API only). Query parsing ("1978" → year filter) is a regex,
not an LLM. **Deliberately LLM-free at serve time** — it's the one thing that can't
fit the device.

Optional, later optimizations (only if evaluation shows gaps):
- **Fine-tune the encoder** on domain pairs mined *for free* from Wikipedia:
  redirects/aliases → canonical title, "also known as", hatnotes; with hard
  negatives. Sharpens domain results.
- **Learning-to-rank** (small gradient-boosted model) to fuse signals
  (trigram score, semantic score, popularity) — needs some labels/click data.

---

## 8. Serving architecture note

The web server is Go using pure-Go `modernc.org/sqlite`, which **cannot load C
vector extensions** (e.g. `sqlite-vec`). So the vector index is either:
- **In-process Go** (recommended): load the quantized vector blob + static query
  embedder into the Go server; do Hamming/int8 scans in Go. No Python, no GPU,
  one binary — ideal for N100/Pi.
- or a **small sidecar** (Python + FAISS) if we adopt a transformer query encoder
  and want its ecosystem. Heavier; only if needed.

---

## 9. What exists today vs TODO

**Done**
- Full-text corpus extraction into `text/<shard>/<pageid>.txt.gz` (cleaned prose
  from the raw article) — the embedding source.
- Lean browser records with `plot` / `overview` / `genre`.
- Structured metadata (year, genre, credits) for the hybrid filters.
- Lexical trigram search to fuse against.

**TODO**
1. Chunker over the corpus (passage windows + overlap).
2. Offline embedding job on the GB10 (pick doc model; produce float32).
3. Quantize (int8 + binary); write the compact index blobs.
4. Query embedder for the device (start with static/Model2Vec in Go).
5. In-process ANN (binary scan → int8 rerank → roll-up).
6. Hybrid fusion (RRF) with trigram + structured filters; wire into `/api/all`.
7. Evaluation harness (a set of semantic queries with expected hits) to compare
   plot-only vs full-article, model sizes, and quantization levels.
8. Extend the corpus to television (series + episode text) once movies are validated.

---

## 10. Design principles (the through-line)

- **Precompute-heavy, serve-light.** All the cost is offline on the GB10; the
  device does one query embed + a compact scan.
- **Keep the raw source, index the signal.** Full text on disk for flexibility;
  lean records + quantized vectors for serving.
- **Hybrid, not replacement.** Semantic + lexical + structured filters, fused.
- **Runs on an N100 / Raspberry Pi.** Every choice (static query embedder, binary
  quantization, in-process Go) is in service of that constraint.

---

## 11. The second path: ColBERT late interaction (2026-08-13)

Everything above describes ONE path: a passage becomes one vector. That is what
makes it scannable on a Pi, and it is also its ceiling. A single averaged
direction cannot represent a conjunctive query — "1978 box office flops" has to
be answered by one point that is simultaneously about a year and about failure,
and the eval shows exactly where that breaks (concept queries MRR 0.212 against
title queries 1.000).

The second path keeps **one vector per token** and scores

    MaxSim(q, d) = sum_i max_j  q_i . d_j

so each query term finds its own evidence independently, twenty words apart if
need be. The two paths are not competitors; they are different points on the
same cost curve, and `chunk.go`'s corpus profiles already anticipated this —
`small`/`medium` for the device, `large` for BIG IRON, which is where this lives.

**It is a reranker, not a retriever.** Full ColBERT retrieval needs a PLAID
centroid index over 138M token vectors; reranking the dense path's candidates
needs no new structure at all:

    int2 scan   -> 4000 passages   (dense, whole corpus, RAM-resident)
    int8 rerank -> 4000 rescored   (dense)
    MaxSim      -> top 500         (mmap'd int8 token vectors, ~10 MB read)

The cost of that choice is explicit: **recall is capped by the dense stage**, so
`eval-colbert` prints the dense path beside it. Reranking cannot find what
retrieval missed; it can only fix the order.

**Measured** (`answerdotai/answerai-colbert-small-v1`, dim 96, 2026-08-13):

| | |
|---|---|
| passages | 606,570 (the same `out/passages.jsonl.gz` `out/index` was built from) |
| token vectors | 138.0M (227.5 per passage) |
| encode | 871 s, 696 passages/s, GPU peak 96% / 702 MB, host RSS peak 9.1 GB |
| store | 13.25 GB int8 (per-dimension p99.9 calibration, as in `quantize.go`) |

That is 21x the dense int8 index and 95x the int2 scan index — the reason this is
a server-only path, and the reason it must never become the default.

**Pieces**
- `embed/colbert.py` — `encode` (corpus), `queries` (eval set), `serve` (query
  token sidecar on :8091). No ColBERT library: both supported checkpoints are a
  plain BERT plus a `linear` projection, so the model is ~40 lines.
- `src/colbert.go` — mmap'd int8 token store, MaxSim rerank, `/api/colbert`,
  `eval-colbert`.
- `src/colbert_test.go` + `embed/colbert_ref.py` — the Go reader is checked
  against float32 MaxSim from the encoder itself. Offset-table and scale bugs do
  not error; they just rank worse, which is why this test exists.
- `colbert.sh` — encode -> queries -> score, with a MemFree preflight (see the
  GB10 unified-memory note in `colbert.sh`).

**Three silent failure modes**, each handled explicitly in `colbert.py`: the
`[Q]`/`[D]` marker token after `[CLS]`; query augmentation (queries padded to 32
with `[MASK]`, those outputs KEPT as learned expansion); punctuation filtering on
the document side only.
