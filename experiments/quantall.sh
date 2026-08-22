#!/bin/bash
set -u
cd "$(dirname "$0")/.."
I=out/index
./filmstock quantize -vectors $I/vectors.f32.BAAI_bge-large-en-v1.5.bin        -dim 1024
./filmstock quantize -vectors $I/vectors.f32.BAAI_bge-m3.bin                   -dim 1024
./filmstock quantize -vectors $I/vectors.f32.nomic-ai_nomic-embed-text-v1.5.bin -dim 768
echo "QUANTIZE ALL DONE"
