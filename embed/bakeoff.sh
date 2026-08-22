#!/bin/bash
# Bake-off: embed the same 543,200 passages with each candidate and record what
# each run cost. Prefixes are per-model and MUST match on the query side —
# omitting them fails silently, costing accuracy with no error.
set -u
cd "$(dirname "$0")/.."
V=./venv/bin/python
OUT=out/index
mkdir -p "$OUT"

run() {  # model, passage-prefix, extra flags
  echo "=================================================================="
  echo "$(date '+%F %T')  $1"
  echo "=================================================================="
  $V embed/embed.py --model "$1" --prefix "$2" --batch 64 --out "$OUT" $3 2>&1 \
    | grep -vE "^Batches:|^Loading weights|it/s\]$" | tail -20
}

run "BAAI/bge-large-en-v1.5"        ""                    ""
run "BAAI/bge-m3"                   ""                    ""
run "nomic-ai/nomic-embed-text-v1.5" "search_document: "  "--trust-remote-code"
echo "$(date '+%F %T')  bake-off complete"
