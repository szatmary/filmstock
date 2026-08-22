#!/bin/bash
# Step 2: section-aware chunking + contextual headers + the "lean" corpus
# profile (lead/plot/cast/reception only), end to end.
#
# NOTE: this conflates three changes, so a gain cannot be attributed to one of
# them. That is deliberate — all three are independently motivated, and the
# baseline to beat is a single number. If it does NOT improve, they will have to
# be separated.
#
# Output goes to out/index2 so the previous (headerless, fixed-window) index
# stays intact for comparison — the baseline to beat is MRR 0.602.
set -eu
cd /tank/mediadb
fail() { echo "!!! FAILED at: $STEP" >&2; exit 1; }
trap fail ERR
step() { STEP="$1"; echo; echo "===== $(date '+%F %T')  $1"; }

V=./venv/bin/python
I2=out/index2
mkdir -p $I2

step "chunk (section-aware, with headers)"
./moviedb chunk -records out -out out/passages2.jsonl.gz -sections lean -workers 12 2>&1 | tail -4

step "embed bge-large"
$V embed/embed.py --model BAAI/bge-large-en-v1.5 --passages out/passages2.jsonl.gz \
  --batch 64 --out $I2 2>&1 | grep -vE "^Batches:|Loading weights" | tail -12

step "quantize"
./moviedb quantize -vectors $I2/vectors.f32.BAAI_bge-large-en-v1.5.bin -dim 1024 2>&1 | tail -3

step "score (baseline was MRR 0.602)"
./moviedb eval-vec -quant $I2/quant.BAAI_bge-large-en-v1.5.json \
  -ids $I2/passages.bin -qvecs out/index/queries.BAAI_bge-large-en-v1.5.bin \
  -dim 1024 -label "bge-large + headers" 2>&1 | grep -vE "rank [0-9]+$|MISS$"

echo
echo "===== $(date '+%F %T')  STEP 2 COMPLETE"
