#!/bin/bash
set -u
cd /tank/mediadb
I=out/index
./moviedb quantize -vectors $I/vectors.f32.BAAI_bge-large-en-v1.5.bin        -dim 1024
./moviedb quantize -vectors $I/vectors.f32.BAAI_bge-m3.bin                   -dim 1024
./moviedb quantize -vectors $I/vectors.f32.nomic-ai_nomic-embed-text-v1.5.bin -dim 768
echo "QUANTIZE ALL DONE"
