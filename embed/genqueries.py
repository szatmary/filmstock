#!/usr/bin/env python3
"""Generating eval queries whose gold answers are COMPLETE, not guessed.

The hand-written set has a flaw that quietly distorts every score computed from
it: "films starring sigourney weaver" lists only Alien as relevant, so a system
that returns Ghostbusters at rank 1 is scored as having missed. The category
that looks worst across every experiment (person, MRR 0.28-0.34) is the one
where that flaw bites hardest, and with n=4 there is no way to tell how much of
it is the retriever.

For three of the five categories the gold set can be COMPUTED instead — from the
same credits and infobox fields the index was built from — so it is complete by
construction and nothing correct can be scored as a miss:

  person       every film with that credit
  conjunctive  every film matching year + credit
  title/typo   the one film, by construction

Only `concept` needs prose paraphrase, and that is written by hand.

Gold sets are kept SMALL (<= 3 films) on purpose. The scorer computes recall@k
over the number of relevant items, so a query with 40 correct answers can never
exceed r@1 = 2.5% and would drag the aggregate around for reasons that have
nothing to do with retrieval quality. Small complete gold keeps the new queries
commensurable with the existing ones.
"""

import argparse
import json
import random
import re
import sqlite3
import unicodedata

DISAMBIG = re.compile(r"\s*\((?:\d{4}\s+)?(?:film|movie|\d{4})\)\s*$", re.I)

# Adjacent keys on a QWERTY board — a typo has to be a plausible finger slip,
# not a random byte, or it measures nothing a real user would produce.
NEIGHBOURS = {
    "a": "qwsz", "b": "vghn", "c": "xdfv", "d": "serfcx", "e": "wsdr",
    "f": "drtgvc", "g": "ftyhbv", "h": "gyujnb", "i": "ujko", "j": "huikmn",
    "k": "jiolm", "l": "kop", "m": "njk", "n": "bhjm", "o": "iklp",
    "p": "ol", "q": "wa", "r": "edft", "s": "awedxz", "t": "rfgy",
    "u": "yhji", "v": "cfgb", "w": "qase", "x": "zsdc", "y": "tghu",
    "z": "asx",
}


def clean_title(t):
    return DISAMBIG.sub("", t).strip()


def ascii_ok(s):
    """Latin-script titles only.

    Not a quality judgement: a typo query has to be typed on the keyboard the
    NEIGHBOURS map describes, and a title in Cyrillic or Han script cannot
    produce a plausible finger slip.
    """
    n = unicodedata.normalize("NFKD", s).encode("ascii", "ignore").decode()
    return len(n) >= len(s) * 0.9 and len(n) > 6


def make_typo(title, rng):
    """One plausible slip: transposition, dropped letter, or adjacent key."""
    letters = [i for i, c in enumerate(title) if c.isalpha()]
    if len(letters) < 6:
        return None
    kind = rng.choice(["swap", "drop", "near"])
    i = rng.choice(letters[1:-1])
    if kind == "swap" and i + 1 < len(title):
        return title[:i] + title[i + 1] + title[i] + title[i + 2:]
    if kind == "drop":
        return title[:i] + title[i + 1:]
    c = title[i].lower()
    if c in NEIGHBOURS:
        return title[:i] + rng.choice(NEIGHBOURS[c]) + title[i + 1:]
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", default="out/search.db")
    ap.add_argument("--out", required=True)
    ap.add_argument("--n-person", type=int, default=60)
    ap.add_argument("--n-conjunctive", type=int, default=60)
    ap.add_argument("--n-title", type=int, default=40)
    ap.add_argument("--n-typo", type=int, default=40)
    ap.add_argument("--seed", type=int, default=20260813)
    args = ap.parse_args()

    rng = random.Random(args.seed)
    db = sqlite3.connect(args.db)
    out = []

    # Titles that identify exactly one film. Hundreds of films share a title
    # ("The Gift", "Alone"), and a title query for one of those has no single
    # right answer — it is an ambiguity test, not a retrieval test.
    rows = db.execute("select id, title, year from movies where year is not null").fetchall()
    by_clean = {}
    for pid, title, year in rows:
        by_clean.setdefault(clean_title(title).lower(), []).append((pid, title, year))
    unique = [v[0] for v in by_clean.values()
              if len(v) == 1 and ascii_ok(clean_title(v[0][1]))]
    rng.shuffle(unique)

    for pid, title, year in unique[:args.n_title]:
        out.append({"category": "title", "query": clean_title(title).lower(),
                    "relevant": [{"page_id": pid, "title": title}],
                    "gold": "complete", "source": "generated"})

    used = {pid for pid, _, _ in unique[:args.n_title]}
    typos = 0
    for pid, title, year in unique[args.n_title:]:
        if typos >= args.n_typo:
            break
        t = make_typo(clean_title(title), rng)
        if not t or t.lower() == clean_title(title).lower():
            continue
        out.append({"category": "typo", "query": t.lower(),
                    "relevant": [{"page_id": pid, "title": title}],
                    "gold": "complete", "source": "generated"})
        used.add(pid)
        typos += 1

    # PERSON — restricted to people with 2-3 film credits in one role, so the
    # complete answer set stays small. The name must also be unique among
    # people, or the query is asking about two different humans.
    dupe = {n for (n,) in db.execute(
        "select name from people group by name having count(*) > 1")}
    for role, phrase in (("Director", "films directed by"),
                         ("Cast", "movies starring"),
                         ("Writer", "films written by")):
        cands = db.execute("""
            select p.name, group_concat(c.work_id)
            from credits c join people p on p.id = c.person_id
            where c.work_type = 'movie' and c.role = ?
            group by p.id having count(*) between 2 and 3
        """, (role,)).fetchall()
        rng.shuffle(cands)
        n = 0
        want = args.n_person // 3
        for name, works in cands:
            if n >= want:
                break
            if name in dupe or len(name) < 6 or not ascii_ok(name):
                continue
            ids = [int(w) for w in works.split(",")]
            titles = dict(db.execute(
                "select id, title from movies where id in (%s)" %
                ",".join("?" * len(ids)), ids).fetchall())
            if len(titles) != len(ids):
                continue
            out.append({"category": "person",
                        "query": f"{phrase} {name.lower()}",
                        "relevant": [{"page_id": i, "title": titles[i]} for i in ids],
                        "gold": "complete", "source": "generated"})
            n += 1

    # CONJUNCTIVE — year AND a credit. Both facts come from the infobox, so the
    # set of films satisfying both is exactly computable; keep only conjunctions
    # that land on 1-3 films.
    cands = db.execute("""
        select p.name, m.year, group_concat(m.id)
        from credits c
        join people p on p.id = c.person_id
        join movies m on m.id = c.work_id
        where c.work_type = 'movie' and c.role = 'Director' and m.year is not null
        group by p.id, m.year having count(*) between 1 and 2
    """).fetchall()
    rng.shuffle(cands)
    n = 0
    for name, year, works in cands:
        if n >= args.n_conjunctive:
            break
        if name in dupe or len(name) < 6 or not ascii_ok(name):
            continue
        ids = [int(w) for w in works.split(",")]
        titles = dict(db.execute(
            "select id, title from movies where id in (%s)" %
            ",".join("?" * len(ids)), ids).fetchall())
        if len(titles) != len(ids):
            continue
        out.append({"category": "conjunctive",
                    "query": f"{year} film directed by {name.lower()}",
                    "relevant": [{"page_id": i, "title": titles[i]} for i in ids],
                    "gold": "complete", "source": "generated"})
        n += 1

    with open(args.out, "w") as f:
        json.dump(out, f, indent=1)

    counts = {}
    for q in out:
        counts[q["category"]] = counts.get(q["category"], 0) + 1
    print(f"{len(out)} queries -> {args.out}")
    for c in sorted(counts):
        print(f"  {c:12s} {counts[c]}")
    print(f"  unique-title pool {len(unique)} of {len(by_clean)} distinct titles")


if __name__ == "__main__":
    main()
