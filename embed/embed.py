#!/usr/bin/env python3
"""Offline passage embedding for filmstock, run on the GB10.

Reads passages.jsonl.gz, writes float32 vectors as a flat memory-mappable blob
plus the page_id per row, and a metrics file recording exactly what the run cost.

Design notes:

* float32 is the ARTIFACT, not the deliverable. Every quantisation level (int8,
  int2, binary) is derived from it in a later local pass, so the expensive GPU
  work happens once regardless of how many formats we end up shipping.

* Batches are sorted by length. With a median article of 307 words and a long
  tail past 1200, unsorted batches pad almost everything to the longest member
  and waste most of the compute.

* Instrumentation is part of the job, not something to watch in nvidia-smi: wall
  time, throughput, and sampled GPU/CPU/RAM are written next to the vectors so
  runs stay comparable across models and hardware.

* Asymmetric models (e5/bge) were trained with literal "query: "/"passage: "
  prefixes. Omitting them costs real accuracy and fails silently, so the prefix
  is explicit and recorded in the manifest — the query side MUST match it.
"""

import argparse
import gzip
import json
import os
import statistics
import subprocess
import threading
import time

import numpy as np


class ResourceSampler:
    """Samples GPU/CPU/RAM on a background thread for the duration of a run.

    Peak and mean both matter: a mean hides a spike that would OOM a smaller box,
    and a peak alone hides an under-utilised GPU.
    """

    def __init__(self, interval=1.0):
        self.interval = interval
        self.gpu_util, self.gpu_mem_mb, self.cpu_pct, self.rss_mb = [], [], [], []
        self._stop = threading.Event()
        self._t = None

    def _nvidia(self):
        """GPU utilisation and memory.

        nvidia-smi is useless here: the GB10 has unified memory, so
        --query-gpu=memory.used/total both report [N/A]. torch's own counters do
        work, and utilisation comes from NVML directly.
        """
        util = mem = None
        try:
            import pynvml
            if not getattr(self, "_nvml", False):
                pynvml.nvmlInit()
                self._h = pynvml.nvmlDeviceGetHandleByIndex(0)
                self._nvml = True
            util = float(pynvml.nvmlDeviceGetUtilizationRates(self._h).gpu)
        except Exception:
            pass
        try:
            import torch
            if torch.cuda.is_available():
                mem = torch.cuda.memory_reserved() / 1024.0 / 1024.0
        except Exception:
            pass
        return util, mem

    def _proc(self):
        try:
            import resource
            rss = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024.0
        except Exception:
            rss = None
        try:
            with open("/proc/loadavg") as f:
                load = float(f.read().split()[0])
            cpu = 100.0 * load / (os.cpu_count() or 1)
        except Exception:
            cpu = None
        return cpu, rss

    def _run(self):
        while not self._stop.wait(self.interval):
            u, m = self._nvidia()
            if u is not None:
                self.gpu_util.append(u)
            if m is not None:
                self.gpu_mem_mb.append(m)
            c, r = self._proc()
            if c is not None:
                self.cpu_pct.append(c)
            if r is not None:
                self.rss_mb.append(r)

    def start(self):
        self._t = threading.Thread(target=self._run, daemon=True)
        self._t.start()
        return self

    def stop(self):
        self._stop.set()
        if self._t:
            self._t.join(timeout=5)

    @staticmethod
    def _stats(v):
        if not v:
            return None
        return {"mean": round(statistics.fmean(v), 1),
                "peak": round(max(v), 1),
                "samples": len(v)}

    def report(self):
        return {
            "gpu_util_pct": self._stats(self.gpu_util),
            "gpu_mem_mb": self._stats(self.gpu_mem_mb),
            "cpu_pct": self._stats(self.cpu_pct),
            "rss_mb": self._stats(self.rss_mb),
        }


def _peak_alloc():
    try:
        import torch
        if torch.cuda.is_available():
            return round(torch.cuda.max_memory_allocated() / 1024 / 1024, 1)
    except Exception:
        pass
    return None


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


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--passages", default="out/passages.jsonl.gz")
    ap.add_argument("--out", default="out/index")
    ap.add_argument("--model", default="BAAI/bge-m3")
    ap.add_argument("--prefix", default="", help='e.g. "passage: " for e5-family')
    ap.add_argument("--batch", type=int, default=64)
    ap.add_argument("--limit", type=int, default=0, help="embed only N passages (smoke test)")
    ap.add_argument("--fp16", action="store_true", default=True)
    ap.add_argument("--trust-remote-code", action="store_true",
                    help="required by nomic-embed and other custom architectures")
    args = ap.parse_args()

    os.makedirs(args.out, exist_ok=True)
    tag = args.model.replace("/", "_")

    print(f"loading passages from {args.passages} ...", flush=True)
    t0 = time.time()
    ids, texts = load_passages(args.passages, args.limit)
    n = len(ids)
    print(f"  {n} passages in {time.time()-t0:.1f}s", flush=True)
    if n == 0:
        raise SystemExit("no passages")

    # Sort by length so batches are homogeneous; keep the inverse permutation so
    # rows are written back in the original passage order.
    order = sorted(range(n), key=lambda i: len(texts[i]))
    inv = np.empty(n, dtype=np.int64)
    for pos, i in enumerate(order):
        inv[i] = pos

    from sentence_transformers import SentenceTransformer
    import torch

    dev = "cuda" if torch.cuda.is_available() else "cpu"
    print(f"loading {args.model} on {dev} ...", flush=True)
    tm = time.time()
    model = SentenceTransformer(args.model, device=dev,
                                trust_remote_code=args.trust_remote_code)
    if args.fp16 and dev == "cuda":
        model = model.half()
    load_s = time.time() - tm
    dim = model.get_sentence_embedding_dimension()
    print(f"  loaded in {load_s:.1f}s, dim={dim}", flush=True)

    sampler = ResourceSampler().start()
    t_embed = time.time()
    ordered = [args.prefix + texts[i] for i in order]
    vecs = model.encode(
        ordered,
        batch_size=args.batch,
        convert_to_numpy=True,
        normalize_embeddings=True,   # cosine becomes a dot product downstream
        show_progress_bar=True,
    ).astype(np.float32)
    embed_s = time.time() - t_embed
    sampler.stop()

    vecs = vecs[inv]                 # restore original passage order

    vec_path = os.path.join(args.out, f"vectors.f32.{tag}.bin")
    id_path = os.path.join(args.out, "passages.bin")
    vecs.tofile(vec_path)
    np.asarray(ids, dtype=np.int32).tofile(id_path)

    metrics = {
        "model": args.model,
        "prefix": args.prefix,
        "device": dev,
        "fp16": bool(args.fp16 and dev == "cuda"),
        "passages": n,
        "dim": dim,
        "batch_size": args.batch,
        "model_load_s": round(load_s, 1),
        "embed_s": round(embed_s, 1),
        "passages_per_s": round(n / embed_s, 1) if embed_s else None,
        "chars_per_s": round(sum(len(t) for t in texts) / embed_s) if embed_s else None,
        "float32_bytes": int(vecs.nbytes),
        "projected_int8_mb": round(n * dim / 1e6, 1),
        "projected_int2_mb": round(n * dim / 4 / 1e6, 1),
        "resources": sampler.report(),
        "gpu_peak_allocated_mb": _peak_alloc(),
        "vectors": vec_path,
        "page_ids": id_path,
    }
    mpath = os.path.join(args.out, f"metrics.{tag}.json")
    json.dump(metrics, open(mpath, "w"), indent=2)

    print("\n=== run ===")
    print(f"  model            {args.model}  dim={dim}")
    print(f"  passages         {n}")
    print(f"  embed time       {embed_s:.1f}s  ({metrics['passages_per_s']}/s)")
    r = metrics["resources"]
    if r["gpu_util_pct"]:
        print(f"  gpu util         mean {r['gpu_util_pct']['mean']}%  peak {r['gpu_util_pct']['peak']}%")
        print(f"  gpu memory       mean {r['gpu_mem_mb']['mean']} MB  peak {r['gpu_mem_mb']['peak']} MB")
    if r["cpu_pct"]:
        print(f"  cpu              mean {r['cpu_pct']['mean']}%  peak {r['cpu_pct']['peak']}%")
    if r["rss_mb"]:
        print(f"  host rss         peak {r['rss_mb']['peak']} MB")
    print(f"  float32          {vecs.nbytes/1e9:.2f} GB")
    print(f"  int8 / int2      {metrics['projected_int8_mb']} MB / {metrics['projected_int2_mb']} MB")
    print(f"  metrics          {mpath}")


if __name__ == "__main__":
    main()
