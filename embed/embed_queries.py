#!/usr/bin/env python3
"""Embed the eval query set with a given model, for scoring the vector index.

The QUERY prefix is not the same as the passage prefix, and getting it wrong is
the classic silent failure: the vectors come out normal-looking and merely rank
worse, with no error anywhere. bge-en-v1.5 wants an instruction on the query side
only; nomic wants "search_query: " against the documents' "search_document: ";
bge-m3 wants neither. They are listed explicitly here rather than inferred.
"""

import argparse
import json
import os

import numpy as np

# model -> (passage prefix used at index time, query prefix required here)
PREFIXES = {
    "BAAI/bge-large-en-v1.5": ("", "Represent this sentence for searching relevant passages: "),
    "BAAI/bge-m3": ("", ""),
    "nomic-ai/nomic-embed-text-v1.5": ("search_document: ", "search_query: "),
}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", required=True)
    ap.add_argument("--queries", default="docs/eval/queries.json")
    ap.add_argument("--out", default="out/index")
    ap.add_argument("--trust-remote-code", action="store_true")
    args = ap.parse_args()

    if args.model not in PREFIXES:
        raise SystemExit(f"unknown model {args.model!r}; add its prefixes to PREFIXES")
    _, qprefix = PREFIXES[args.model]

    qs = json.load(open(args.queries))["queries"]
    texts = [qprefix + q["query"] for q in qs]

    from sentence_transformers import SentenceTransformer
    import torch

    dev = "cuda" if torch.cuda.is_available() else "cpu"
    model = SentenceTransformer(args.model, device=dev,
                                trust_remote_code=args.trust_remote_code)
    vecs = model.encode(texts, batch_size=16, convert_to_numpy=True,
                        normalize_embeddings=True).astype(np.float32)

    tag = args.model.replace("/", "_")
    path = os.path.join(args.out, f"queries.{tag}.bin")
    vecs.tofile(path)
    print(f"{len(qs)} queries x {vecs.shape[1]} dims -> {path}")
    print(f"  query prefix: {qprefix!r}")


if __name__ == "__main__":
    main()
