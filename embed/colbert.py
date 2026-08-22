#!/usr/bin/env python3
"""ColBERT late interaction — the second search path.

The first path embeds a whole passage into ONE vector. That is what makes it
cheap enough for a Pi, and it is also its ceiling: "a hacker discovers reality is
a simulation" and "a simulation discovers a hacker" collapse to nearly the same
point, and a conjunctive query ("1978" AND "flop") is answered by a single
averaged direction that is neither.

ColBERT keeps one vector PER TOKEN and scores

    score(q, d) = sum_i max_j  q_i . d_j

so every query term finds its own best evidence in the passage independently.
That is the whole difference, and it costs ~200x the storage — which is why this
is the BIG IRON path (chunk.go's `large` profile), never the device path.

Both supported checkpoints are `HF_ColBERT`: a plain BERT plus a `linear`
projection down to 96 or 128 dims, with no bias. So there is no ColBERT library
here — the ~40 lines below ARE the model. Adding `colbert-ai`/`pylate` would drag
in a retrieval stack we already have in Go.

Three things fail SILENTLY if they are wrong, which is why each is explicit:

  * the [Q]/[D] marker token after [CLS] — the model was trained with them and
    tells the encoder which side it is looking at;
  * query augmentation — queries are padded to 32 with [MASK] and those outputs
    are KEPT as learned query expansion, not discarded as padding;
  * punctuation filtering on the document side only.

Usage:
  colbert.py encode  --passages out/passages.jsonl.gz --out out/colbert
  colbert.py queries --out out/colbert
  colbert.py serve   --out out/colbert [--addr :8091]
"""

import argparse
import gzip
import json
import os
import string
import sys
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from embed import ResourceSampler  # noqa: E402  same instrumentation as the dense path

# Registry rather than inference: every value here changes the vectors, and a
# wrong one produces normal-looking output that merely ranks worse. Taken from
# each checkpoint's artifact.metadata.
#
# Two families, and the difference is not cosmetic. The original ColBERT
# checkpoints are BERT with the projection at `linear.weight` and mark the side
# with the reserved token ids [unused0]/[unused1]. PyLate-style checkpoints are
# ModernBERT with the projection in a `1_Dense/` module and mark the side with
# the LITERAL text "[Q] "/"[D] ", which tokenises to several ordinary subwords.
# Feeding one family the other's convention silently costs accuracy.
MODELS = {
    "answerdotai/answerai-colbert-small-v1": {
        "dim": 96, "query_maxlen": 32, "doc_maxlen": 300, "hidden": 384,
        "linear": ("model.safetensors", "linear.weight"),
        "marker": "unused", "dtype": "fp16",
    },
    "colbert-ir/colbertv2.0": {
        "dim": 128, "query_maxlen": 32, "doc_maxlen": 180, "hidden": 768,
        "linear": ("model.safetensors", "linear.weight"),
        "marker": "unused", "dtype": "fp16",
    },
    # ModernBERT, 22 layers at 768 — roughly 10x the compute of the small model,
    # and materially stronger on BEIR. Values from config_sentence_transformers.json.
    "lightonai/GTE-ModernColBERT-v1": {
        "dim": 128, "query_maxlen": 48, "doc_maxlen": 300, "hidden": 768,
        "linear": ("1_Dense/model.safetensors", "linear.weight"),
        "marker": "text", "query_prefix": "[Q] ", "doc_prefix": "[D] ",
        # ModernBERT was trained in bf16 and overflows in fp16.
        "dtype": "bf16",
        # ModernBERT defaults to torch.compile'd kernels, which need Triton to
        # build cuda_utils against Python.h — absent without python3-dev, and it
        # fails at the first forward pass rather than at load. sdpa is plain
        # PyTorch attention and needs no compiler.
        "model_kwargs": {"attn_implementation": "sdpa", "reference_compile": False},
    },
}

Q_MARKER = 1  # [unused0]
D_MARKER = 2  # [unused1]


class ColBERT:
    """BERT + linear projection + L2 normalisation, per token."""

    def __init__(self, name, device=None, fp16=True):
        if name not in MODELS:
            raise SystemExit(f"unknown model {name!r}; add it to MODELS with its "
                             f"dim/query_maxlen/doc_maxlen from artifact.metadata")
        import torch
        from transformers import AutoModel, AutoTokenizer
        from huggingface_hub import hf_hub_download
        from safetensors.torch import load_file

        self.cfg = MODELS[name]
        self.name = name
        self.device = device or ("cuda" if torch.cuda.is_available() else "cpu")
        self.torch = torch

        self.tok = AutoTokenizer.from_pretrained(name)
        self.bert = AutoModel.from_pretrained(
            name, **self.cfg.get("model_kwargs", {})).to(self.device).eval()
        if self.bert.config.hidden_size != self.cfg["hidden"]:
            raise SystemExit(f"hidden size {self.bert.config.hidden_size} != registry "
                             f"{self.cfg['hidden']} — wrong checkpoint")

        lin_file, lin_key = self.cfg["linear"]
        w = load_file(hf_hub_download(name, lin_file))[lin_key]
        if tuple(w.shape) != (self.cfg["dim"], self.cfg["hidden"]):
            raise SystemExit(f"{lin_file}:{lin_key} is {tuple(w.shape)}, expected "
                             f"{(self.cfg['dim'], self.cfg['hidden'])}")
        self.linear = torch.nn.Linear(self.cfg["hidden"], self.cfg["dim"], bias=False)
        self.linear.weight.data = w
        self.linear = self.linear.to(self.device).eval()

        if fp16 and self.device == "cuda":
            dt = torch.bfloat16 if self.cfg["dtype"] == "bf16" else torch.float16
            self.bert = self.bert.to(dt)
            self.linear = self.linear.to(dt)

        self.mask_id = self.tok.mask_token_id
        self.cls_id = self.tok.cls_token_id
        self.sep_id = self.tok.sep_token_id
        self.pad_id = self.tok.pad_token_id
        # Document-side punctuation skiplist, as in the reference implementation:
        # punctuation carries no retrieval signal but would win max() against a
        # query term often enough to matter.
        self.skiplist = {self.tok.encode(c, add_special_tokens=False)[0]
                         for c in string.punctuation
                         if self.tok.encode(c, add_special_tokens=False)}

    @property
    def dim(self):
        return self.cfg["dim"]

    def _forward(self, ids, attn):
        torch = self.torch
        with torch.inference_mode():
            h = self.bert(input_ids=ids, attention_mask=attn).last_hidden_state
            v = self.linear(h)
            return torch.nn.functional.normalize(v.float(), p=2, dim=2)

    def encode_docs(self, texts):
        """Returns a list of (ntokens, dim) float32 arrays, punctuation removed."""
        torch = self.torch
        maxlen = self.cfg["doc_maxlen"]
        rows = []
        for t in texts:
            if self.cfg["marker"] == "text":
                body = self.tok.encode(self.cfg["doc_prefix"] + t, add_special_tokens=False,
                                       truncation=True, max_length=maxlen - 2)
                rows.append([self.cls_id] + body + [self.sep_id])
            else:
                body = self.tok.encode(t, add_special_tokens=False,
                                       truncation=True, max_length=maxlen - 3)
                rows.append([self.cls_id, D_MARKER] + body + [self.sep_id])
        width = max(len(r) for r in rows)
        ids = torch.full((len(rows), width), self.pad_id, dtype=torch.long)
        attn = torch.zeros((len(rows), width), dtype=torch.long)
        for i, r in enumerate(rows):
            ids[i, : len(r)] = torch.tensor(r)
            attn[i, : len(r)] = 1
        v = self._forward(ids.to(self.device), attn.to(self.device)).cpu().numpy()

        out = []
        for i, r in enumerate(rows):
            keep = [j for j, tid in enumerate(r) if tid not in self.skiplist]
            out.append(v[i, keep, :].astype(np.float32))
        return out

    def encode_query(self, text):
        """Returns (query_maxlen, dim) float32 — [MASK] expansion vectors kept."""
        torch = self.torch
        qlen = self.cfg["query_maxlen"]
        if self.cfg["marker"] == "text":
            body = self.tok.encode(self.cfg["query_prefix"] + text,
                                   add_special_tokens=False)[: qlen - 2]
            real = [self.cls_id] + body + [self.sep_id]
        else:
            body = self.tok.encode(text, add_special_tokens=False)[: qlen - 3]
            real = [self.cls_id, Q_MARKER] + body + [self.sep_id]
        ids = torch.tensor([real + [self.mask_id] * (qlen - len(real))])
        # attend_to_mask_tokens=False: the [MASK] positions produce outputs but
        # are not attended TO by the real tokens.
        attn = torch.tensor([[1] * len(real) + [0] * (qlen - len(real))])
        v = self._forward(ids.to(self.device), attn.to(self.device)).cpu().numpy()
        return v[0].astype(np.float32)


def load_passages(path, limit=0):
    ids, texts = [], []
    with gzip.open(path, "rt", encoding="utf-8", errors="replace") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            d = json.loads(line)
            ids.append(d["page_id"])
            texts.append(d["text"])
            if limit and len(ids) >= limit:
                break
    return ids, texts


def calibrate(model, texts, batch, sample_n, seed=7):
    """Per-dimension int8 scales from a random sample.

    Per-dimension, not global: projected dimensions have very different dynamic
    ranges, and one global scale spends most of the int8 range on dimensions that
    never approach it. p99.9 rather than max so a single outlier token does not
    flatten everything else — same calibration rule as quantize.go.
    """
    rng = np.random.default_rng(seed)
    idx = rng.choice(len(texts), size=min(sample_n, len(texts)), replace=False)
    acc = []
    for i in range(0, len(idx), batch):
        chunk = [texts[j] for j in idx[i: i + batch]]
        acc.extend(model.encode_docs(chunk))
    allv = np.concatenate(acc, axis=0)
    scales = np.percentile(np.abs(allv), 99.9, axis=0).astype(np.float32)
    scales[scales <= 0] = 1e-6
    return scales, len(allv)


def cmd_encode(args):
    model = ColBERT(args.model, fp16=not args.fp32)
    tag = args.model.replace("/", "_")
    os.makedirs(args.out, exist_ok=True)

    print(f"loading passages from {args.passages} ...", flush=True)
    t0 = time.time()
    page_ids, texts = load_passages(args.passages, args.limit)
    n = len(texts)
    print(f"  {n} passages in {time.time()-t0:.1f}s", flush=True)
    if n == 0:
        raise SystemExit("no passages")

    print(f"calibrating int8 scales on {args.calib} passages ...", flush=True)
    tc = time.time()
    scales, ncal = calibrate(model, texts, args.batch, args.calib)
    print(f"  {ncal} token vectors, scale range {scales.min():.4f}..{scales.max():.4f}"
          f" in {time.time()-tc:.1f}s", flush=True)

    # Sorted by length so batches are homogeneous — unsorted, nearly every batch
    # pads to doc_maxlen and most of the GPU work is spent on padding. Rows are
    # written in THIS order and the offset table records where each landed, so
    # physical order never has to match passage order.
    #
    # DESCENDING, so the largest activation the run will ever need is allocated
    # in the first batch. The GB10 shares RAM with the page cache, which this job
    # fills as it writes; ascending order would ask for the peak allocation at
    # the very end, when memory is tightest, and fail after all the work.
    order = sorted(range(n), key=lambda i: len(texts[i]), reverse=True)

    tok_path = os.path.join(args.out, f"colbert.tokens.int8.{tag}.bin")
    off = np.zeros(n, dtype=np.uint64)
    lens = np.zeros(n, dtype=np.uint16)
    sampler = ResourceSampler().start()
    t0 = time.time()
    total_tokens = 0
    written = 0
    with open(tok_path, "wb", buffering=1 << 22) as fh:
        for b in range(0, n, args.batch):
            sel = order[b: b + args.batch]
            vecs = model.encode_docs([texts[i] for i in sel])
            for i, v in zip(sel, vecs):
                q = np.clip(np.rint(v / scales * 127.0), -127, 127).astype(np.int8)
                fh.write(q.tobytes())
                off[i] = total_tokens
                lens[i] = q.shape[0]
                total_tokens += q.shape[0]
            written += len(sel)
            if written % (args.batch * 50) == 0 or written == n:
                el = time.time() - t0
                print(f"\r  {written}/{n}  {written/el:.0f}/s  "
                      f"{total_tokens/1e6:.1f}M tokens  eta {(n-written)/(written/el)/60:.1f}m",
                      end="", flush=True)
    elapsed = time.time() - t0
    sampler.stop()
    print()

    off.tofile(os.path.join(args.out, f"colbert.offsets.{tag}.bin"))
    lens.tofile(os.path.join(args.out, f"colbert.lens.{tag}.bin"))
    np.asarray(page_ids, dtype=np.int32).tofile(
        os.path.join(args.out, f"colbert.pageids.{tag}.bin"))

    man = {
        "model": args.model,
        "bits": 8,
        "dim": model.dim,
        "count": n,
        "total_tokens": int(total_tokens),
        "mean_tokens": round(total_tokens / n, 1),
        "doc_maxlen": model.cfg["doc_maxlen"],
        "query_maxlen": model.cfg["query_maxlen"],
        "scales_int8": [float(s) for s in scales],
        "tokens_path": tok_path,
        "offsets_path": os.path.join(args.out, f"colbert.offsets.{tag}.bin"),
        "lens_path": os.path.join(args.out, f"colbert.lens.{tag}.bin"),
        "pageids_path": os.path.join(args.out, f"colbert.pageids.{tag}.bin"),
        "tokens_bytes": os.path.getsize(tok_path),
        "passages_source": os.path.abspath(args.passages),
        "elapsed_s": round(elapsed, 1),
        "passages_per_s": round(n / elapsed, 1),
        "resources": sampler.report(),
    }
    mp = os.path.join(args.out, f"colbert.{tag}.json")
    with open(mp, "w") as f:
        json.dump(man, f, indent=2)

    print(f"\n  model            {args.model}  dim={model.dim}")
    print(f"  passages         {n}")
    print(f"  token vectors    {total_tokens/1e6:.1f}M  ({man['mean_tokens']}/passage)")
    print(f"  encode time      {elapsed:.1f}s  ({man['passages_per_s']}/s)")
    r = sampler.report()
    for k, label in (("gpu_util_pct", "gpu util"), ("gpu_mem_mb", "gpu memory"),
                     ("cpu_pct", "cpu"), ("rss_mb", "host rss")):
        s = r.get(k)
        if s:
            print(f"  {label:16s} mean {s['mean']}  peak {s['peak']}")
    print(f"  int8 tokens      {man['tokens_bytes']/1e9:.2f} GB")
    print(f"  manifest         {mp}")


def cmd_queries(args):
    model = ColBERT(args.model, fp16=False)
    tag = args.model.replace("/", "_")
    qs = json.load(open(args.queries))["queries"]
    mat = np.stack([model.encode_query(q["query"]) for q in qs]).astype(np.float32)
    path = os.path.join(args.out, f"colbert.queries.{tag}.bin")
    mat.tofile(path)
    print(f"{len(qs)} queries x {mat.shape[1]} tokens x {mat.shape[2]} dims -> {path}")


def cmd_serve(args):
    """Query-side sidecar: Go cannot run a transformer, so it asks for the 32xD
    query matrix and does the MaxSim itself against the int8 store."""
    model = ColBERT(args.model, fp16=False)

    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_GET(self):
            u = urllib.parse.urlparse(self.path)
            if u.path != "/api/colbert":
                self.send_error(404)
                return
            q = urllib.parse.parse_qs(u.query).get("q", [""])[0]
            try:
                v = model.encode_query(q)
                body = json.dumps({"model": model.name, "dim": model.dim,
                                   "tokens": int(v.shape[0]),
                                   "vector": v.reshape(-1).tolist()}).encode()
            except Exception as e:  # surfaced to the caller, never swallowed
                body = json.dumps({"error": str(e)}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    host, _, port = args.addr.rpartition(":")
    srv = HTTPServer((host or "0.0.0.0", int(port)), H)
    print(f"colbert query encoder on http://{host or '0.0.0.0'}:{port}/api/colbert "
          f"({model.name}, dim={model.dim})", flush=True)
    srv.serve_forever()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["encode", "queries", "serve"])
    ap.add_argument("--model", default="answerdotai/answerai-colbert-small-v1")
    ap.add_argument("--passages", default="/tank/mediadb/out/passages.jsonl.gz")
    ap.add_argument("--queries", default="/tank/mediadb/docs/eval/queries.json")
    ap.add_argument("--out", default="/tank/mediadb/out/colbert")
    ap.add_argument("--batch", type=int, default=64)
    ap.add_argument("--calib", type=int, default=20000)
    ap.add_argument("--limit", type=int, default=0, help="encode only N passages (smoke test)")
    ap.add_argument("--fp32", action="store_true")
    ap.add_argument("--addr", default=":8091")
    args = ap.parse_args()
    {"encode": cmd_encode, "queries": cmd_queries, "serve": cmd_serve}[args.cmd](args)


if __name__ == "__main__":
    main()
