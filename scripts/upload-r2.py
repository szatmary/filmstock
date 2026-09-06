#!/usr/bin/env python3
"""Sync the release tree to the R2 bucket behind https://filmstock.halide.tv.

The bucket root IS the tree root, because the hostname already says filmstock:
builds.json sits at the root and each build is a directory beside it. Nothing
uploaded names a host, so the same bytes serve unchanged from R2, from a plain
directory on disk, or from whatever hosts this next.

    filmstock.halide.tv/builds.json          the catalog
    filmstock.halide.tv/<id>/manifest.json   per-build files and hashes
    filmstock.halide.tv/<id>/<file>          the databases and patches

Two things this script is careful about, both of which break consumers silently
rather than loudly:

CONTENT-ENCODING. The patches are named *.sql.gz and their manifest sha256 is
over the gzip bytes as they sit on disk. An uploader that infers
`Content-Encoding: gzip` from the extension — several do — makes R2 advertise
the object as an encoded representation, and Go's default HTTP transport then
transparently gunzips it on the way in. The updater would hash the SQL instead
of the gzip and reject every daily with a sha256 mismatch. So content type is
set explicitly here, per extension, and Content-Encoding is never set at all.
(Cloudflare compressing a response in transit is a different thing and is
safe: the client decompresses back to the stored bytes.)

ORDER. builds.json is how a consumer discovers what exists, and a manifest is
how it discovers a build's files. Both are published only after the bytes they
point at are all in place, so a consumer that arrives mid-upload sees the old,
complete tree rather than a build it cannot fetch. Within a build the data
files go first, then manifest.json; builds.json goes last of all, once every
build is complete.

Credentials come from the environment (see scripts/r2.env.example):

    R2_ACCOUNT_ID  R2_ACCESS_KEY_ID  R2_SECRET_ACCESS_KEY  R2_BUCKET

Usage:
    scripts/upload-r2.py [--root bucket] [--dry-run] [--verify-only] [--force]
"""

import argparse
import hashlib
import json
import os
import sys
import time

try:
    import boto3
    from boto3.s3.transfer import TransferConfig
    from botocore.config import Config
    from botocore.exceptions import ClientError
except ImportError:
    sys.exit("upload-r2: boto3 is required (pip install boto3)")

# Explicit, by extension. Anything not listed is refused rather than guessed:
# an unrecognised extension in the release tree means the tree changed shape,
# and that is worth a human looking at it, not a default.
CONTENT_TYPE = {
    ".db": "application/vnd.sqlite3",
    ".gz": "application/gzip",
    ".json": "application/json",
}

# 1.3 GB of the tree is one file, so multipart parallelism is what sets the
# wall time. 64 MB parts keep the part count small enough that a retry is cheap.
TRANSFER = TransferConfig(
    multipart_threshold=64 * 1024 * 1024,
    multipart_chunksize=64 * 1024 * 1024,
    max_concurrency=8,
    use_threads=True,
)


def sha256_of(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 22), b""):
            h.update(chunk)
    return h.hexdigest()


def content_type(key):
    ext = os.path.splitext(key)[1]
    if ext not in CONTENT_TYPE:
        raise SystemExit(f"upload-r2: {key}: unknown extension {ext!r}; "
                         f"add it to CONTENT_TYPE deliberately")
    return CONTENT_TYPE[ext]


def plan(root):
    """Every (local path, key) in publish order: a build's data, then its
    manifest, then builds.json once every build is complete."""
    items = []
    for d in sorted(os.listdir(root)):
        bdir = os.path.join(root, d)
        if not os.path.isdir(bdir):
            continue
        names = sorted(os.listdir(bdir))
        data = [n for n in names if n != "manifest.json"]
        for n in data:
            items.append((os.path.join(bdir, n), f"{d}/{n}"))
        if "manifest.json" in names:
            items.append((os.path.join(bdir, "manifest.json"), f"{d}/manifest.json"))
    catalog = os.path.join(root, "builds.json")
    if os.path.isfile(catalog):
        items.append((catalog, "builds.json"))
    return items


def remote_head(s3, bucket, key):
    try:
        return s3.head_object(Bucket=bucket, Key=key)
    except ClientError as e:
        if e.response["Error"]["Code"] in ("404", "NoSuchKey", "NotFound"):
            return None
        raise


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", default="/tank/mediadb/bucket")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--verify-only", action="store_true",
                    help="check what is already up there; upload nothing")
    ap.add_argument("--force", action="store_true",
                    help="re-upload even when size and sha256 already match")
    args = ap.parse_args()

    missing = [v for v in ("R2_ACCOUNT_ID", "R2_ACCESS_KEY_ID",
                           "R2_SECRET_ACCESS_KEY", "R2_BUCKET")
               if not os.environ.get(v)]
    if missing:
        sys.exit(f"upload-r2: missing env: {', '.join(missing)}\n"
                 f"  source your r2.env (see scripts/r2.env.example)")

    account = os.environ["R2_ACCOUNT_ID"]
    bucket = os.environ["R2_BUCKET"]
    s3 = boto3.client(
        "s3",
        endpoint_url=f"https://{account}.r2.cloudflarestorage.com",
        aws_access_key_id=os.environ["R2_ACCESS_KEY_ID"],
        aws_secret_access_key=os.environ["R2_SECRET_ACCESS_KEY"],
        region_name="auto",
        config=Config(retries={"max_attempts": 5, "mode": "standard"}),
    )

    items = plan(args.root)
    if not items:
        sys.exit(f"upload-r2: nothing to upload under {args.root}")

    uploaded = skipped = 0
    sent_bytes = 0
    problems = []
    t0 = time.time()

    for path, key in items:
        size = os.path.getsize(path)
        head = remote_head(s3, bucket, key)

        # HEAD first, and hash only when the answer can still change. A
        # published build directory is immutable, so an object already up there
        # at the right size with a recorded sha256 is current by construction —
        # rehashing 1.3 GB to learn that on every daily run is pure cost. The
        # cases that do need a hash: nothing up there yet (the digest becomes
        # the object's metadata), a size match with no recorded digest (an
        # older upload, so compare properly), or --force.
        digest = None

        def local_digest():
            nonlocal digest
            if digest is None:
                digest = sha256_of(path)
            return digest

        if head is not None:
            recorded = head.get("Metadata", {}).get("sha256")
            if head["ContentLength"] != size:
                same = False
            elif recorded:
                # --verify-only and --force are the modes that are asking the
                # real question, so they pay for the hash; a plain sync trusts
                # size plus a recorded digest under an immutable key.
                same = (recorded == local_digest()
                        if (args.force or args.verify_only) else True)
            else:
                same = False  # size matches but nothing recorded; re-upload to record it
            # Whatever else is true, an object serving with Content-Encoding
            # set will fail the consumer's hash check. Say so loudly.
            if head.get("ContentEncoding"):
                problems.append(f"{key}: Content-Encoding: {head['ContentEncoding']} "
                                f"is set; consumers will hash decompressed bytes")
            if same and not args.force:
                skipped += 1
                continue
            if not same and args.verify_only:
                problems.append(f"{key}: differs from local "
                                f"(remote {head['ContentLength']}B, local {size}B)")
                continue
        elif args.verify_only:
            problems.append(f"{key}: not uploaded")
            continue

        if args.verify_only:
            continue

        print(f"  {'[dry-run] ' if args.dry_run else ''}"
              f"{key}  {size/1e6:.1f} MB", flush=True)
        if not args.dry_run:
            s3.upload_file(
                path, bucket, key,
                ExtraArgs={
                    "ContentType": content_type(key),
                    # sha256 travels with the object so a later run can tell
                    # "same bytes" from "same size" without re-downloading.
                    "Metadata": {"sha256": local_digest()},
                    # Immutable content under an immutable key. builds.json is
                    # the one thing that changes, so it gets a short TTL.
                    "CacheControl": ("public, max-age=60"
                                     if key == "builds.json"
                                     else "public, max-age=31536000, immutable"),
                },
                Config=TRANSFER,
            )
        uploaded += 1
        sent_bytes += size

    dt = time.time() - t0
    if args.verify_only:
        print(f"\nverify: {len(items)} objects checked in {dt:.1f}s")
    else:
        rate = f"{sent_bytes/1e6/dt:.1f} MB/s" if dt > 0 and sent_bytes else "-"
        print(f"\n{uploaded} uploaded ({sent_bytes/1e9:.2f} GB, {rate}), "
              f"{skipped} already current, {dt:.1f}s")

    if problems:
        print("\nPROBLEMS:")
        for p in problems:
            print("  " + p)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
