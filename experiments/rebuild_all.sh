#!/bin/bash
# Full rebuild after the <ref> ordering fix.
#
# The fix changes cleanText, which feeds BOTH the embedding corpus and the
# plot/overview on every record, so everything downstream is invalid: records,
# search.db, passages, vectors, quantised blobs. Nothing is reused.
#
# Fails loudly. The previous bake-off printed "complete" after a model had
# crashed because nothing checked exit codes.
set -eu
cd "$(dirname "$0")/.."

fail() { echo "!!! FAILED at step: $STEP" >&2; exit 1; }
trap fail ERR
step() { STEP="$1"; echo; echo "===== $(date '+%F %T')  $1"; }

V=./venv/bin/python
I=out/index

step "stop servers"
[ -f serve.pid ]  && kill "$(awk '{print $NF}' serve.pid)"  2>/dev/null || true
[ -f testui.pid ] && kill "$(cat testui.pid)" 2>/dev/null || true
sleep 2

step "wipe output (records are regenerated from the dumps; the resolver cache is kept)"
rm -rf out

step "extract + index"
./filmstock extract -dumps dump -out out -cache wikidata.db -workers 18 2>&1 | tail -12

step "chunk"
./filmstock chunk -records out -workers 12 2>&1 | tail -3

step "embed: bge-large"
$V embed/embed.py --model BAAI/bge-large-en-v1.5 --batch 64 --out $I 2>&1 | grep -vE "^Batches:|Loading weights" | tail -12
# bge-m3 dropped: it scored last on the previous bake-off (recall@20 23.1% vs
# 53.8% and 46.2%), is the slowest (68 min), and wants 10 GB of VRAM. Its only
# distinguishing feature is multilingual, which nothing consumes yet.
step "embed: nomic"
$V embed/embed.py --model nomic-ai/nomic-embed-text-v1.5 --prefix "search_document: " \
   --trust-remote-code --batch 64 --out $I 2>&1 | grep -vE "^Batches:|Loading weights" | tail -12

step "quantize"
./filmstock quantize -vectors $I/vectors.f32.BAAI_bge-large-en-v1.5.bin         -dim 1024 2>&1 | tail -3
./filmstock quantize -vectors $I/vectors.f32.nomic-ai_nomic-embed-text-v1.5.bin -dim 768  2>&1 | tail -3

step "embed eval queries"
$V embed/embed_queries.py --model BAAI/bge-large-en-v1.5 2>/dev/null | tail -1
$V embed/embed_queries.py --model nomic-ai/nomic-embed-text-v1.5 --trust-remote-code 2>/dev/null | tail -1

step "score"
./filmstock eval 2>&1 | tail -3
./filmstock eval-vec -quant $I/quant.BAAI_bge-large-en-v1.5.json \
  -qvecs $I/queries.BAAI_bge-large-en-v1.5.bin -dim 1024 -label bge-large 2>&1 | grep -E "recall@1 |\(int2|RRF"
./filmstock eval-vec -quant $I/quant.nomic-ai_nomic-embed-text-v1.5.json \
  -qvecs $I/queries.nomic-ai_nomic-embed-text-v1.5.bin -dim 768 -label nomic 2>&1 | grep -E "recall@1 |\(int2|RRF"

step "corpus health (was: 8% with raw infoboxes, 10k-char Harry Potter)"
python3 - <<'PY'
import glob, gzip, random
fs = glob.glob('out/text/*/*.txt.gz'); random.seed(1)
bad = 0
for f in random.sample(fs, 400):
    s = gzip.open(f, 'rt', errors='replace').read()
    if '{{Infobox' in s or s.lstrip().startswith('{{'):
        bad += 1
print(f"  raw infobox present: {bad}/400")
t = gzip.open('out/text/e1/667361.txt.gz', 'rt', errors='replace').read()
print(f"  Harry Potter (film): {len(t)} chars (was 10160)")
PY

step "restart server"
setsid nohup ./filmstock serve -db out/search.db -movies out/movies -television out/television -addr :8080 \
  > serve.log 2>&1 < /dev/null &
echo "server pid: $!" > serve.pid
sleep 2
echo "http $(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://localhost:8080/)"

echo
echo "===== $(date '+%F %T')  REBUILD COMPLETE"
