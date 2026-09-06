# Hosting the release tree

The tree published under `bucket/` is served at **https://filmstock.halide.tv**,
from a Cloudflare R2 bucket named `filmstock` attached to that hostname as a
custom domain.

    filmstock.halide.tv/builds.json          the catalog
    filmstock.halide.tv/<id>/manifest.json   per-build files and hashes
    filmstock.halide.tv/<id>/<file>          databases and patches

## Why this shape

The bucket root is the tree root. The hostname already says `filmstock`, so a
`filmstock/` key prefix would only produce `filmstock.halide.tv/filmstock/`.

Nothing published names a host. `builds.json` records each manifest as a path
relative to the tree root, and the consumer holds the base
(`filmstock.Update(ctx, baseURL, dir)`), so the same bytes serve unchanged from
R2, from a directory on disk, or from whatever hosts this next. Moving hosts is
a DNS change and a bucket copy; no file in the tree is rewritten, and no
released client needs an update.

One product, one subdomain: `filmstock.halide.tv` can be repointed at another
bucket, another CDN, or another provider without touching `halide.tv` or
anything else served from it.

## Uploading

    cp scripts/r2.env.example /tank/mediadb/.r2.env   # fill in, chmod 600
    set -a; . /tank/mediadb/.r2.env; set +a
    scripts/upload-r2.py --dry-run
    scripts/upload-r2.py

The script is idempotent — it skips any object whose size and sha256 already
match — so it is both the initial backfill and the per-day publish step. It
uploads a build's data files before that build's `manifest.json`, and
`builds.json` last of all, so a consumer arriving mid-upload sees the previous
complete tree rather than a build whose files are not there yet.

`scripts/upload-r2.py --verify-only` checks the bucket against local without
uploading.

## The one thing that will silently break consumers

The patches are named `*.sql.gz` and their manifest `sha256` is over the gzip
bytes as they sit on disk. If an object is stored with
`Content-Encoding: gzip`, R2 advertises it as an encoded representation and
Go's default HTTP transport transparently gunzips it on the way in — so the
updater hashes the SQL, not the gzip, and rejects every daily with a sha256
mismatch.

`upload-r2.py` sets `Content-Type` explicitly per extension and never sets
`Content-Encoding`. Do not "fix" that by teaching it to infer encoding from the
extension. Cloudflare compressing a response *in transit* is a different thing
and is safe: the client decompresses back to the stored bytes.

## Cache headers

Build files live under immutable keys and are sent
`Cache-Control: public, max-age=31536000, immutable`. `builds.json` is the only
mutable object and gets `max-age=60`, which bounds how long a consumer can keep
missing a fresh build.

After changing `builds.json` outside of a normal publish, purge it:
Cloudflare dashboard > halide.tv > Caching > Configuration > Purge custom URLs.
