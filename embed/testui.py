#!/usr/bin/env python3
"""Throwaway UI for judging semantic search quality by hand.

Deliberately NOT the shipping path: it brute-forces float32 with numpy rather
than using the int2 -> int8 cascade, so what you see is the retrieval CEILING.
If a result is bad here, no amount of quantisation tuning will save it — the
model or the chunking is at fault. Comparing this against `filmstock eval-vec`
separates "the model can't find it" from "quantisation lost it".

It also shows the matching PASSAGE, not just the film, because that is usually
the thing that explains a surprising hit.
"""

import gzip
import json
import sqlite3
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

import numpy as np

INDEX = "out/index"
DB = "out/search.db"

MODELS = {
    "bge-large": ("BAAI/bge-large-en-v1.5", 1024,
                  "Represent this sentence for searching relevant passages: ", False),
    "nomic": ("nomic-ai/nomic-embed-text-v1.5", 768, "search_query: ", True),
}

state = {}


def boot(which):
    if which in state:
        return state[which]
    name, dim, qprefix, trust = MODELS[which]
    tag = name.replace("/", "_")
    print(f"loading {name} ...", flush=True)
    from sentence_transformers import SentenceTransformer
    m = SentenceTransformer(name, trust_remote_code=trust)
    vecs = np.memmap(f"{INDEX}/vectors.f32.{tag}.bin", dtype=np.float32, mode="r")
    vecs = vecs.reshape(-1, dim)
    state[which] = (m, vecs, dim, qprefix)
    print(f"  {vecs.shape[0]} vectors x {dim} dims", flush=True)
    return state[which]


def load_shared():
    if "ids" in state:
        return
    state["ids"] = np.fromfile(f"{INDEX}/passages.bin", dtype=np.int32)
    print("loading passage text ...", flush=True)
    texts = []
    with gzip.open("out/passages.jsonl.gz", "rt", errors="replace") as f:
        for line in f:
            texts.append(json.loads(line)["text"])
    state["texts"] = texts
    state["db"] = sqlite3.connect(DB, check_same_thread=False)
    print(f"  {len(texts)} passages", flush=True)


def search(which, q, topn=15):
    m, vecs, dim, qprefix = boot(which)
    load_shared()
    qv = m.encode([qprefix + q], normalize_embeddings=True).astype(np.float32)[0]
    scores = vecs @ qv                      # brute force: the quality ceiling
    order = np.argsort(-scores)[: topn * 8]  # oversample, then roll up to works

    best = {}
    for i in order:
        pid = int(state["ids"][i])
        if pid not in best or scores[i] > best[pid][0]:
            best[pid] = (float(scores[i]), int(i))
    ranked = sorted(best.items(), key=lambda kv: -kv[1][0])[:topn]

    out = []
    for pid, (sc, pi) in ranked:
        row = state["db"].execute(
            "select title, year from movies where id=?", (pid,)).fetchone()
        if not row:
            # not a film — the passage corpus is films-only today, but a television page_id
            # would land here once the television corpus exists.
            row = ("(not in movies table)", None)
        snippet = state["texts"][pi][:320]
        out.append({"page_id": pid, "title": row[0], "year": row[1],
                    "score": round(sc, 4), "passage": snippet})
    return out


PAGE = """<!doctype html><meta charset=utf-8><title>filmstock semantic test</title>
<style>
body{background:#141416;color:#e8e8ea;font:15px/1.5 system-ui,sans-serif;margin:0;padding:28px}
.wrap{max-width:900px;margin:0 auto}
h1{font-size:18px;font-weight:600;margin:0 0 4px}
.sub{color:#8a8a92;font-size:13px;margin-bottom:18px}
input,select{background:#1e1e22;border:1px solid #33333a;color:#e8e8ea;border-radius:7px;
 padding:11px 13px;font-size:15px}
input{width:100%;box-sizing:border-box}
.row{display:flex;gap:10px;margin-bottom:6px}
.r{border-top:1px solid #26262c;padding:13px 0}
.t{font-weight:600}.y{color:#8a8a92;font-weight:400}
.s{float:right;color:#6ea8fe;font-variant-numeric:tabular-nums;font-size:13px}
.p{color:#a0a0aa;font-size:13px;margin-top:5px}
.hint{color:#6a6a72;font-size:12px;margin-top:14px}
</style>
<div class=wrap>
<h1>filmstock — semantic search</h1>
<div class=sub>brute-force float32 (the quality ceiling, not the shipping cascade)</div>
<div class=row>
  <input id=q placeholder="describe a film — e.g. hacker discovers reality is a simulation" autofocus>
  <select id=m><option value=bge-large>bge-large</option><option value=nomic>nomic</option>
  </select>
</div>
<div class=hint>first query per model loads it (~30s)</div>
<div id=out></div></div>
<script>
const q=document.getElementById('q'),m=document.getElementById('m'),out=document.getElementById('out')
let t
function go(){
  if(!q.value.trim())return
  out.innerHTML='<div class=hint>searching…</div>'
  fetch('/api/search?model='+m.value+'&q='+encodeURIComponent(q.value)).then(r=>r.json()).then(rs=>{
    out.innerHTML=rs.map(r=>`<div class=r><span class=s>${r.score}</span>
      <span class=t>${r.title}</span> <span class=y>${r.year||''}</span>
      <div class=p>${r.passage.replace(/</g,'&lt;')}…</div></div>`).join('')||'<div class=hint>nothing</div>'
  }).catch(e=>out.innerHTML='<div class=hint>error: '+e+'</div>')
}
q.addEventListener('keydown',e=>{if(e.key==='Enter')go()})
m.addEventListener('change',go)
</script>"""


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        if u.path == "/":
            b = PAGE.encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)
            return
        if u.path == "/api/embed":
            # Query-vector service for the Go browser. Go cannot run the encoder,
            # but it owns the int2 -> int8 cascade, so the split is: Python
            # embeds one short query, Go does the search. The MODEL and PREFIX
            # must match whatever produced the document vectors, or results
            # degrade silently.
            p = urllib.parse.parse_qs(u.query)
            q = (p.get("q") or [""])[0]
            which = (p.get("model") or ["bge-large"])[0]
            try:
                m, _v, dim, qprefix = boot(which)
                vec = m.encode([qprefix + q], normalize_embeddings=True).astype("float32")[0]
                body = json.dumps({"dim": int(dim), "vector": [float(x) for x in vec]})
            except Exception as e:
                body = json.dumps({"error": str(e)})
            b = body.encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)
            return
        if u.path == "/api/search":
            p = urllib.parse.parse_qs(u.query)
            q = (p.get("q") or [""])[0]
            which = (p.get("model") or ["bge-large"])[0]
            try:
                res = search(which, q)
            except Exception as e:
                res = [{"page_id": 0, "title": f"error: {e}", "year": None,
                        "score": 0, "passage": ""}]
            b = json.dumps(res).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)
            return
        self.send_response(404)
        self.end_headers()


if __name__ == "__main__":
    print("http://0.0.0.0:8090", flush=True)
    HTTPServer(("0.0.0.0", 8090), H).serve_forever()
