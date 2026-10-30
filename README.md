# seapin

IPFS cache gateway for Algorand assets. Each region (EU, US) keeps its own SeaweedFS bucket and Postgres catalog of
every IPFS object Algorand ASAs reference. Only content on an allowlist is stored. Anything not cached is a 404:
seapin never fetches from IPFS while serving. Design, decisions and milestones: [PLAN.md](PLAN.md).

**Status:** M1 done: the read path (`seapin serve`). Next is M2: ingest (queue consumer, lanes, fetch sidecar) and
JSON link following. The scanner rewrite (`cmd/seapin-scan`) is M3.

## What `serve` answers

| request | answer |
|---|---|
| `GET/HEAD /ipfs/<cid>` cached | 200 / 206 (Range) / 304 (`If-None-Match: "<cid>"`). Cached for a year, `immutable`, `ETag` = CID |
| `/ipfs/<cid>/<path>` | resolved to the leaf CID through the catalog's `aliases`; 503 if the catalog is down |
| refused by the content policy | 403 with the reason (`not_allowed:<type>`, `too_large`, `directory`) |
| taken down (`takedowns` table) | 451 |
| not cached | 404, `Cache-Control: max-age=60` so the CDN asks again soon |
| invalid CID or path | 400 |

- Any CID spelling works: `Qm…` (v0), and base32, base36 or base58 v1. Objects are stored as CIDv1 base32 under
  `<cid[-4:-2]>/<cid[-2:]>/<cid>`.
- Every response carries `X-Content-Type-Options: nosniff` and a sandboxing `Content-Security-Policy`.
- The served type is re-checked against the allowlist even for stored objects.
- `/health` returns 503 only when S3 is unreachable; a Postgres outage shows in the body but keeps cached objects
  served. `/metrics` is Prometheus.

Content policy (`internal/policy`): the type is sniffed from the bytes, never taken from headers or extensions.
- Stored: png, jpeg, gif, webp, avif, bmp, SVG (sandboxed); mp4, webm, quicktime; mpeg, ogg, wav, flac, aac and mp4
  audio; GLB; JSON.
- Everything else is refused, and so is anything over 256 MiB.

## Layout

```
cmd/seapin          serve | migrate | seed <cid> <file> | health | version
internal/cidkey     CID parsing, CIDv1 base32 normalization, S3 key, sub-path validation, raw-CID verification
internal/policy     sniffing, allowlist, size cap, security headers
internal/store      S3 (SeaweedFS) access: stat, ranged reads, put, bucket
internal/catalog    Postgres catalog + embedded migrations (internal/catalog/migrations)
internal/serve      HTTP handler, ranged object reader, in-memory takedown list
internal/config     environment
```

## Dev stack

All listeners are on loopback in the 21xxx/31xxx range: SeaweedFS (S3 on 21833), Postgres (21432) and seapin
(http://127.0.0.1:21090).

```sh
./scripts/dev-init.sh                 # once: .env (random credentials, 0600) + dev/s3.json
docker compose up -d --build          # seaweedfs, postgres, seapin-migrate (one-shot), seapin
docker compose run --rm seapin-tool seed <cid> /seed/<file>   # put the file in ./dev/seed first
./scripts/dev-smoke.sh                # end-to-end check of the serve path
```

`seed` applies the same policy as ingest will. It verifies raw-codec CIDs against the file; dag-pb CIDs depend on
the original chunking, so for those the operator vouches. S3 identities:
- `seapin-serve` may only read (`Read:ipfs`).
- `seapin-ingest` (migrate, seed, later ingest) may also write and create the bucket.

## Configuration

| variable | default | |
|---|---|---|
| `LISTEN_ADDR` | `127.0.0.1:8090` | HTTP listener; keep it on loopback behind openresty |
| `S3_ENDPOINT` | required | SeaweedFS S3 gateway, `host:port` |
| `S3_BUCKET` | `ipfs` | |
| `S3_ACCESS_KEY`, `S3_SECRET_KEY` | required | the serve identity for `serve`, the ingest identity for `migrate`/`seed` |
| `S3_USE_SSL` | `false` | |
| `PG_DSN` | required | catalog Postgres |
| `MISS_MAX_AGE` | `60s` | `Cache-Control` max-age of a 404 |
| `TAKEDOWN_REFRESH` | `60s` | how often the takedown list is reloaded; `serve` won't start until it has loaded once |

## Tests

```sh
go test -race -p=$(nproc --ignore=1) ./...
# catalog against a real Postgres (throwaway schema), e.g. the dev stack's:
SEAPIN_TEST_PG_DSN='postgres://seapin:<PG_PASSWORD>@127.0.0.1:21432/seapin?sslmode=disable' go test ./internal/catalog/
```
