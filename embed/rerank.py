#!/usr/bin/env python3
"""Cross-encoder reranking — the last stage of the cascade.

The three stages differ in how late the query and the document are allowed to
interact, and that is exactly the cost/quality ladder:

  dense       one vector each, compared once          whole corpus, ~40 ms
  ColBERT     one vector per token, MaxSim            top ~500 passages
  cross-enc   query and passage in ONE forward pass   top ~50 passages

Only the last one lets the model read the query and the passage together, which
is why it is the most accurate and why it can never be an index: there is
nothing to precompute — every (query, passage) pair is a fresh forward pass.
That is 41-219 ms per pair on this class of hardware, so it is strictly a
server-side stage on a handful of candidates.

It doubles as the TEACHER for distillation: its scores are what a fine-tuned
ColBERT would be trained to reproduce.

The sidecar holds the passage text because the Go side stores only vectors and
offsets; it is addressed by passage ROW, the same row the ColBERT store uses, so
neither side has to agree on anything but an integer.

Usage:
  rerank.py serve [--model BAAI/bge-reranker-base] [--addr :8092]
"""

import argparse
import gzip
import json
import sys
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

MODELS = {
    # (max_length, description) — both are cross-encoders scoring one pair at a
    # time; the large one is ~3x the compute for a few points of nDCG.
    "BAAI/bge-reranker-base": 512,
    "BAAI/bge-reranker-large": 512,
    "cross-encoder/ms-marco-MiniLM-L6-v2": 512,
}


class CrossEncoder:
    def __init__(self, name, passages_path, batch=32, device=None):
        if name not in MODELS:
            raise SystemExit(f"unknown reranker {name!r}; add it to MODELS")
        import torch
        from transformers import AutoModelForSequenceClassification, AutoTokenizer

        self.torch = torch
        self.name = name
        self.maxlen = MODELS[name]
        self.batch = batch
        # Explicit, because on the GB10 the GPU shares system RAM with every
        # other job: a second CUDA process fails to allocate while an encode is
        # running, and falling back silently would hide a 10x slowdown.
        self.device = device or ("cuda" if torch.cuda.is_available() else "cpu")

        print(f"loading {name} on {self.device} ...", flush=True)
        self.tok = AutoTokenizer.from_pretrained(name)
        self.model = AutoModelForSequenceClassification.from_pretrained(name)
        self.model = self.model.to(self.device).eval()
        if self.device == "cuda":
            self.model = self.model.half()

        print(f"loading passage text from {passages_path} ...", flush=True)
        self.texts = []
        with gzip.open(passages_path, "rt", encoding="utf-8", errors="replace") as f:
            for line in f:
                line = line.strip()
                if line:
                    self.texts.append(json.loads(line)["text"])
        print(f"  {len(self.texts)} passages", flush=True)

    def score(self, query, rows):
        """Scores (query, passage[row]) for each row. Order of `rows` preserved."""
        torch = self.torch
        out = []
        for i in range(0, len(rows), self.batch):
            chunk = rows[i: i + self.batch]
            pairs = [(query, self.texts[r]) for r in chunk]
            enc = self.tok([p[0] for p in pairs], [p[1] for p in pairs],
                           padding=True, truncation=True, max_length=self.maxlen,
                           return_tensors="pt").to(self.device)
            with torch.inference_mode():
                logits = self.model(**enc).logits
            out.extend(logits[:, 0].float().cpu().tolist())
        return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["serve"])
    ap.add_argument("--model", default="BAAI/bge-reranker-base")
    ap.add_argument("--passages", default="/tank/mediadb/out/passages.jsonl.gz")
    ap.add_argument("--batch", type=int, default=32)
    ap.add_argument("--device", default=None, help="cuda | cpu (default: cuda if present)")
    ap.add_argument("--addr", default=":8092")
    args = ap.parse_args()

    ce = CrossEncoder(args.model, args.passages, args.batch, args.device)

    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_GET(self):
            u = urllib.parse.urlparse(self.path)
            if u.path != "/api/rerank":
                self.send_error(404)
                return
            qs = urllib.parse.parse_qs(u.query)
            q = qs.get("q", [""])[0]
            rows_raw = qs.get("rows", [""])[0]
            try:
                rows = [int(x) for x in rows_raw.split(",") if x != ""]
                bad = [r for r in rows if r < 0 or r >= len(ce.texts)]
                if bad:
                    raise ValueError(f"passage rows out of range: {bad[:5]}")
                body = json.dumps({"model": ce.name,
                                   "scores": ce.score(q, rows)}).encode()
            except Exception as e:
                body = json.dumps({"error": str(e)}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    host, _, port = args.addr.rpartition(":")
    srv = HTTPServer((host or "0.0.0.0", int(port)), H)
    print(f"cross-encoder on http://{host or '0.0.0.0'}:{port}/api/rerank "
          f"({ce.name})", flush=True)
    sys.stdout.flush()
    srv.serve_forever()


if __name__ == "__main__":
    main()
