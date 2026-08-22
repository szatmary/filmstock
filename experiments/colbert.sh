#!/bin/bash
# The ColBERT path, end to end: encode -> query tokens -> score.
#
# Baselines to beat, same 38-query set, depth 20:
#   lexical                    (trigram + Dice)
#   dense bge-large            MRR 0.602   (out/index, fixed-window corpus)
#   dense bge-large + headers  MRR 0.516   (out/index2, section-aware + lean)
#
# Encodes out/passages.jsonl.gz — the SAME passage file out/index was built
# from, because late interaction reranks that index's candidates and the two
# must be row-for-row aligned (Go checks and refuses if they are not).
# pipefail matters: every heavy step below pipes into grep, and without it a
# crashed encoder is masked by grep's exit status and the script reports success.
set -euo pipefail
cd /tank/mediadb
fail() { echo "!!! FAILED at: $STEP" >&2; exit 1; }
trap fail ERR
step() { STEP="$1"; echo; echo "===== $(date '+%F %T')  $1"; }

V=./venv/bin/python
MODEL=${1:-answerdotai/answerai-colbert-small-v1}
OUT=${2:-out/colbert}
TAG=${MODEL/\//_}
mkdir -p $OUT

# CUDA on the GB10 shares system RAM, and the driver will NOT reclaim page cache
# to satisfy an allocation: with ~1 GB MemFree every cudaMalloc fails with "out
# of memory" while `free` still shows 100 GB available. Checked here rather than
# 12 minutes in.
step "preflight"
FREE_GB=$(awk '/MemFree/{print int($2/1048576)}' /proc/meminfo)
echo "  MemFree ${FREE_GB} GB"
if [ "$FREE_GB" -lt 8 ]; then
  echo "!!! only ${FREE_GB} GB free — CUDA will fail. Run: sudo sysctl -w vm.drop_caches=3" >&2
  exit 1
fi

step "encode passages (one vector per token)"
$V embed/colbert.py encode --model "$MODEL" --passages out/passages.jsonl.gz \
  --out $OUT --batch ${BATCH:-128} 2>&1 | grep -vE "^Loading weights|it/s\]|UNEXPECTED|^Key|^---|^Notes|LOAD REPORT"

step "encode eval queries"
$V embed/colbert.py queries --model "$MODEL" --out $OUT 2>&1 | tail -2

step "score"
./moviedb eval-colbert \
  -quant   out/index/quant.BAAI_bge-large-en-v1.5.json \
  -ids     out/index/passages.bin \
  -qvecs   out/index/queries.BAAI_bge-large-en-v1.5.bin \
  -colbert $OUT/colbert.$TAG.json \
  -cqvecs  $OUT/colbert.queries.$TAG.bin \
  -dim 1024 2>&1 | grep -vE "rank [0-9]+$|MISS$"

echo
echo "===== $(date '+%F %T')  COLBERT COMPLETE"
