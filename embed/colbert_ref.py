#!/usr/bin/env python3
"""Reference MaxSim scores for src/colbert_test.go.

Re-encodes a few passages at float32 and scores them against a query with the
textbook formula, straight from the model — no quantisation, no offset table, no
mmap. The Go side has to reproduce these numbers from the int8 store alone, which
is the only way to catch an off-by-one in the offset table or a scale applied in
the wrong order: both produce plausible scores that merely rank worse.
"""

import argparse
import json
import os

import numpy as np

from colbert import ColBERT, load_passages


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="answerdotai/answerai-colbert-small-v1")
    ap.add_argument("--passages", default="out/passages.jsonl.gz")
    ap.add_argument("--out", required=True, help="dir holding the encoded index")
    ap.add_argument("--ref", required=True, help="reference json to write")
    ap.add_argument("--query", default="a hacker discovers reality is a simulation")
    ap.add_argument("--rows", type=int, default=24)
    args = ap.parse_args()

    model = ColBERT(args.model, fp16=False)
    _, texts = load_passages(args.passages, limit=args.rows)

    q = model.encode_query(args.query)                 # (qlen, dim)
    docs = model.encode_docs(texts)                    # list of (ntok, dim)
    scores = [float(np.max(q @ d.T, axis=1).sum()) for d in docs]

    tag = args.model.replace("/", "_")
    man = json.load(open(os.path.join(args.out, f"colbert.{tag}.json")))
    if man["count"] < args.rows:
        raise SystemExit(f"index has {man['count']} passages, need {args.rows}")

    with open(args.ref, "w") as f:
        json.dump({
            "query": args.query,
            "dim": model.dim,
            "query_tokens": q.tolist(),
            "rows": list(range(args.rows)),
            "scores": scores,
        }, f)
    print(f"{args.rows} reference scores -> {args.ref}")
    print(f"  range {min(scores):.3f} .. {max(scores):.3f}")


if __name__ == "__main__":
    main()
