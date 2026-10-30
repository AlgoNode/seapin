# seapin — redundant IPFS cache gateway: plan (v1, 2026-10-30)

Replace today's IPFS HTTP caching gateway (openresty + kubo + ipfs-cluster on htz-hel-ipfs-1/2) with two
independent SeaweedFS-backed caches, one in the EU and one in the US. `algoipfssvc` scans Algorand and queues every
IPFS object an ASA references; seapin fetches missing CIDs through a fetch-only IPFS sidecar, refuses dangerous
content, stores the rest in SeaweedFS and keeps a per-region Postgres catalog of CIDs and their references.

Constraints (operator, 2026-10-30): **append-only**: no eviction, objects are never deleted. **Eventual consistency**
between regions; losing cached data is acceptable (it is a cache of IPFS). Initial footprint: **1 VM per region, 3 TB
NVMe each (growable online and cheaply to 8 TB, no restart), single-node SeaweedFS** in each.

## 0. Evidence (shallow audit, 2026-10-30)

### seapin today: `main.go`, 179 lines, Go 1.25, minio-go
A minimal S3-backed read/write gateway. It is a usable skeleton for the read path; the rest still has to be built.

| # | finding | where | fix in |
|---|---|---|---|
| A1 | Anonymous `POST /upload` with unbounded `io.ReadAll`; the client's Content-Type is stored and served back, so anyone can host HTML/JS on the gateway origin (stored XSS, memory DoS) | `main.go:51`, `:112`, `:125` | M1: remove |
| A2 | `computeCID` hashes the whole file as one raw block, so it matches IPFS only for single-block raw files. Real NFT CIDs are mostly dag-pb (`Qm…`, `bafybei…`) | `main.go:150` | M1: goes away with A1 |
| A3 | `GET /ipfs/{cid}` matches one path segment, so `/ipfs/<cid>/1.png` (directory CIDs, ARC-19 `cid+ext`) returns 404 | `main.go:50` | M1 |
| A4 | The storage key is the CID string exactly as requested: `Qm…` and `bafy…` for the same content (or base32 vs base36) become different keys | `main.go:68` | M1: normalize to CIDv1 base32 |
| A5 | Content-Type comes straight from S3 metadata; no sniffing, no allowlist, no `nosniff`/CSP. The openresty Lua filter is today's only guard | `main.go:88-93` | M1 + M2 |
| A6 | `io.Copy` with a fixed Content-Length: no Range (video seeking), no conditional requests; copy error ignored | `main.go:94-99` | M1: `http.ServeContent` (`minio.Object` is an `io.ReadSeeker`) |
| A7 | `context.Background()` instead of `r.Context()`: aborted downloads keep reading from S3 | `main.go:68`, `:130` | M1 |
| A8 | `http.ListenAndServe` without server timeouts or graceful shutdown | `main.go:54` | M1 |
| A9 | A miss is a plain 404: no fetch path, queue consumer, Postgres or sidecar | — | M2 |
| A10 | `minioadmin` defaults in code and compose; compose runs unpinned `minio/minio`, not SeaweedFS, and follows none of the fleet compose conventions; no tests, metrics or S3 health check | `main.go:22-27`, `docker-compose.yml` | M1 |

### algoipfssvc (the scanner, `~/dev/_production/back/algoipfssvc`, svc-asset on htz-hel-ipfs-1)
Pipeline: RabbitMQ `tx` headers exchange (`type=acfg`) → `acfg` worker builds `AssetInfo` → `asset` queue (with
ttl/dlq retry) → resolves `ipfs://`, ARC-19 `template-ipfs://`, `/ipfs/` paths and `*.ipfs.*` subdomains → reads the
first 10 KiB through `ipfs.nodes[0].gw` → sniffs the type → follows the JSON fields `image`, `animation_url` and
`properties.file_url` → `log` exchange → `db` worker (one `x_asset_meta` JSONB row per asset on the etl Postgres) and
`static` worker (full download to an md5-sharded local directory plus Redis `cid → mime`).

- There is no per-CID record. `ASAextra` keeps one object per media class, so a second image overwrites the first
  (`internal/worker/asset_ipfs.go:145`). The new contract sends one message per reference.
- CIDs come from regexes and are never validated; `cid.Parse` is commented out (`asset_ipfs.go:88,102,116`). seapin
  must validate every CID it receives.
- Paths are already in play: ARC-19 produces `cid + ext` (`asset_ipfs.go:79`).
- Only `nodes[0]` is ever used (`internal/worker/asset.go:153`). `Bytes` comes from Content-Length (−1 when chunked),
  so sizes in `x_asset_meta` are approximate. `AssetTX` has no note field (`internal/worker/acfg.go:21`), so ARC-69
  `media_url` references are never followed.
- Arbitrary `http(s)` URLs found inside JSON are fetched (`asset.go:59`), an SSRF surface on a ufw default-allow host.
  The new contract carries CIDs only, never URLs.
- From 2026-09-04 until it was stopped on 2026-10-30, svc-asset ran a refetch loop of 75–105 M requests/day
  (~1000/s) against its own gateway (`algoup/reports/2026-10-30-ipfs1-noble-upgrade.md`). seapin ingest must be
  idempotent (a CID already cached is never fetched again) and the scanner's backfill must be rate-limited.
- The README and `config.jsonc.example` are stale (copied from a block streamer).

### Current gateway (htz-hel-ipfs-1/2, `cfg/ipfs-hel-1/docker/openresty/conf.d/07-ipfspub.conf`)
Fetch order: local kubo with `only-if-cached` (2 s) → the peer host's kubo (5 s) → Pinata dedicated gateway (30 s).
A referer whitelist and a Lua `header_filter` reject `text/*` and `application/*` except JSON. That filter lets
`image/svg+xml` through and trusts kubo's Content-Type. Both hosts run `ipfs/kubo:master-latest` (unpinned) and
ipfs-cluster. External traffic is small: ~1.1k requests per 10 min across both nodes behind the CDN (2026-10-30). The
design therefore optimizes for correctness and operability, not throughput.

### Code base: rewrite in this repo (operator OK'd starting from scratch, 2026-10-30)
Candidates compared:
- **`~/dev/ndlyipfsgw`** (225 lines, not a git repo, not in the registry): a generic read-through proxy.
  - Cache keys are `sha256(Host+URI)`: a scanner can never pre-populate them, and the same CID cached under two
    hosts is stored twice.
  - It buffers whole bodies in memory (`io.ReadAll`).
  - It "replicates" with fire-and-forget POSTs to every filer, ignoring errors.
  - Its MIME check allows every `text/*` except `text/javascript` (so `text/html` passes) and PDF, and it trusts the
    upstream type.
  - `shouldRetry` retries on any non-200, so 404s cascade through every upstream.
  - No Range, no HEAD (405), no server timeouts.
- **`~/dev/_obsolete/ndlyipfsgw`**: a 2023 chi/koanf REST template whose only route proxies to Pinata. Its
  `config.yaml` holds the Pinata token in plaintext (logged in `algoup/backlog/secrets-rotation.md`).
- **seapin**: as audited above.

None is more mature than a short rewrite. Decision: **keep this repo** (GitHub remote, registry entry, Dockerfile)
**and replace `main.go` with a fresh layout**. Two ideas carry over from ndlyipfsgw: an ordered upstream fallback
chain, and a policy check on the hit path as well as the miss path.

### Sizing (read live 2026-10-30)
| host | kubo RepoSize | blocks | StorageMax |
|---|---|---|---|
| htz-hel-ipfs-1 | 2.3 TB | 19.3 M | 4 TB |
| htz-hel-ipfs-2 | 2.4 TB | 29.2 M | 4 TB |

M0 measured the ASA-referenced set itself: **1.88 TB in 3.91 M root CIDs, growing ~0.1–0.14 GB/day** (§3 M0).
These repos also hold everything users ever fetched through the gateway, not just ASA-referenced objects. Each new
server starts with 3 TB and can grow online, cheaply, to **8 TB** (operator, 2026-10-30): about 3× today's repos.
3 TB alone would leave ~20% headroom over ~2.4 TB, so the referenced set and its daily growth (M0) decide *when* to
grow, not whether the data fits.

### Fleet SeaweedFS lessons (`algoup/projects/tmp-seaweed-zfs-tuning`)
- Pin the image.
- The `chrislusf/seaweedfs` entrypoint silently forces `-master.volumeSizeLimitMB=1024`; set it explicitly at the end
  of `command`.
- Keep the filer store on NVMe (here everything is NVMe).
- After a restart, S3 GETs fail until every volume has remounted. Check the volume count before declaring healthy.

Found while building M1 (4.45, 2026-10-30):
- **Healthchecks must probe every component.** The master's `/cluster/healthz` turns healthy before the S3 gateway
  listens. The compose check now requires the master `/cluster/healthz` plus volume, filer and S3 `/healthz`.
- **Phones home and opens extra listeners by default.** `-master.telemetry=false` stops the usage reports to
  telemetry.seaweedfs.com; `-s3.port.iceberg=0 -s3.port.lance=0` turns off the extra listeners.
- **Slot math.** With `-volume.max=0`, slots = free disk / volume size, and a new collection grows 7 volumes at
  once. 30 GB volumes therefore need >210 GB free before the first write. That is fine on 3 TB, but a small test VM
  or workstation needs a smaller `volumeSizeLimitMB`; dev uses 1 GB.

## 1. Target design (per region; both regions identical)

```
                 CDN + geo steering / health-checked failover (D6)
                 |                                          |
   +------------ EU VM (3 TB NVMe) ------------+   +-- US VM (same) --+
   | openresty: whitelist, apilog (D5)         |   |                  |
   |   -> seapin serve :8080 (127.0.0.1)       |   |                  |
   |        hit:  S3 GET -> seaweedfs (1 node) |   |                  |
   |        miss: peer cache, else 404 (D2)    |   |                  |
   | seapin ingest <- RMQ queue seapin.eu      |   |  seapin.us       |
   |        Postgres (catalog)  kubo/rainbow   |   |                  |
   +----------------------+--------------------+   +--------+---------+
                          +--- tailnet: peer fetch + catalog pull ---+
   algoipfssvc --> RMQ exchange "seapin" (fanout) --> seapin.eu, seapin.us
```

- **One binary, two roles**: `seapin serve` (public read path) and `seapin ingest` (queue consumer and fetcher). They
  ship as one image and run as two services, so ingest stalls never block serving.
- **SeaweedFS** (as deployed on the M4 hosts: `4.45_large_disk` image and **100 GB volumes**; never switch that
  data to a non-`_large_disk` image; filer store and needle indexes on the local root disk). The original sketch
  follows: `weed server -filer -s3`, single node, replication `000`, image pinned,
  `-master.volumeSizeLimitMB=30720` set explicitly. With no deletes there is no vacuum, so 100–270 × 30 GB volumes
  (3–8 TB) is fine. `-volume.max=0` derives the volume slot count from free space, so an online disk grow is picked
  up (see Capacity). `-volume.minFreeSpace=100GiB` stops writes before the filesystem fills. S3 identity for seapin
  only, bound to 127.0.0.1. One bucket `ipfs`, keys sharded on the CID tail (`<cid[-4:-2]>/<cid[-2:]>/<cid>`).
- **Only leaf objects are stored**, keyed by their own CID. A request for `<root>/<path>` resolves through the catalog
  (`aliases`) to the leaf CID. This dedups shared files and avoids file-vs-directory name clashes in the filer
  namespace.
- **Fetch sidecar** (ingest only, D1 decided 2026-10-30): **rainbow**, flags in `deploy/rainbow/README.md`. The
  original kubo sketch follows for reference: kubo, pinned, fetch-only and non-caching in practice. Gateway on 127.0.0.1 only;
  `Routing.Type=autoclient` (DHT client, no DHT server); providing disabled (the config key depends on the pinned
  version); `--enable-gc` with a small `Datastore.StorageMax` (~50 GB) and a short `GCPeriod`; resource manager and
  connection manager capped. `ipfs/rainbow`, a stateless gateway with no persistent blockstore, fits "fetch-only"
  even better; compare the two in M0.
- **Ingest fetch chain**: local S3 → peer region (internal endpoint over the tailnet, bearer token) → **legacy**
  (the old gateway's caches, `Cache-Control: only-if-cached`, migration only; M0 found ~7 in 10 historical
  objects nowhere else) → the rainbow sidecar → Pinata (until the M6 review, D7). Each CID is fetched once at a time (singleflight per CID), with
  concurrency and deadlines set per lane (see Ingest scheduling); Pinata is tried in the slow lane only. kubo verifies the blocks it fetches. The peer is our own
  infrastructure and is trusted, but its bytes are re-sniffed anyway.
- **The serve path never touches IPFS** (D2). A local S3 hit is served. On a local miss, if the peer region has the
  CID cached, seapin proxies it from there and nudges local ingest. Otherwise it answers 404 with a short
  `Cache-Control`, so the CDN asks again later.

### Catalog (Postgres sidecar, per region)
```sql
objects (cid text pk,                       -- leaf CID, CIDv1 base32
         status text,                       -- pending | cached | blocked | failed
         mime text, size bigint, reason text,   -- reason: not_allowed:<mime> | too_large | directory | takedown
         first_seen_at timestamptz,         -- block time of the first referencing txn = "upload date"
         cached_at timestamptz, source text,    -- kubo | peer | pinata
         lane text, creator text, depth int,    -- fast | slow | backfill; creator of the first ref; hops from chain
         attempts int, next_try_at timestamptz, lease_until timestamptz)
refs    (key text pk,                       -- chain-level only: '<chain>/<asset>/url', '<chain>/<asset>/note/media_url'
         root_cid text, subpath text,       -- what the chain points at (+ path inside it)
         round bigint, txid text, block_time timestamptz, updated_at timestamptz)   -- index on root_cid
links   (parent_cid text, json_path text,   -- per JSON object, from its content: immutable, computed once
         child_cid text, subpath text, pk (parent_cid, json_path))
aliases (root_cid text, subpath text, leaf_cid text, pk (root_cid, subpath))
takedowns (cid text pk, reason text, created_at timestamptz)   -- operator blocks, served as 451
```
`path` in the key (decided 2026-10-30) records where the reference sits:
- **Chain-level**, sent by the scanner as `refs`: `url` (the ASA URL itself) and `note/media_url` (ARC-69).
- **Content-level**, derived by seapin from `links` (see Reference graph): `url/image`, `url/animation_url`,
  `url/properties/file_url`, and deeper keys such as `url/<json pointer>/<json pointer>`.

One ASA therefore has as many keys as it references objects. Many keys can share one CID. When an ARC-19
reconfiguration changes the CID behind a key, the `refs` row is updated and the old object stays (append-only).

### Message contract v1 (scanner → seapin, JSON, one message per reference)
```json
{"v":1,"type":"ref","key":"mainnet-v1.0/1234567/url","chain":"mainnet-v1.0","asset":1234567,
 "path":"url","cid":"bafybei...","subpath":"","mime_hint":"application/json",
 "creator":"<ASA creator address>","src":"live",
 "round":45000000,"txid":"...","block_time":"2026-10-30T12:00:00Z"}
```
Messages are idempotent and order-insensitive (last `round` per key wins). `src` is `live` or `backfill`;
`creator` drives fair scheduling (see Ingest scheduling). A second type, `takedown`
(`{"v":1,"type":"takedown","cid":...,"reason":...}`), carries operator blocks. The current config hardcodes a 404
for one CID, which shows the need.

### Reference graph: dynamic CIDs and JSON that points to more CIDs
Example: an ARC-19 asset with URL `template-ipfs://{ipfscid:0:dag-pb:reserve:sha2-256}`. Its *current* reserve (or
manager) address gives a CID; that CID is a JSON object; the JSON lists an image, an animation and more files. Each
hop is a separate, bounded unit of work. It is never one recursive fetch inside a single 120 s request, which is
what today's `parseObj`/`fetchUri` do.

1. **The chain decides the root (scanner).** The scanner resolves only what is on chain, with no IPFS fetch:
   - `ipfs://`, `/ipfs/` paths and `*.ipfs.*` hosts, after ARC-3 `{id}` substitution
   - ARC-19 templates, reading the address field the template *names*. Today's scanner always uses the reserve and
     ignores the captured field name (`asset_ipfs.go:79`), so a manager-based template resolves to the wrong CID.
   - the ARC-69 note JSON, already on chain

   It sends one `ref` per chain-level key. When a reconfiguration changes the named address, the scanner recomputes
   the CID and sends a new `ref` with the higher round. The key now points at the new root, and the old objects
   stay (append-only).
2. **Content decides the children (seapin ingest).** After a JSON object is stored, ingest walks it once and
   records `links(parent_cid, json_path → child_cid, subpath)`.
   - A link is any string value in an explicit IPFS form (`ipfs://`, `/ipfs/`, `*.ipfs.*`), not only the well-known
     `image` / `animation_url` / `properties.file_url`. Collection-specific layouts (`properties/files/0/uri`) are
     therefore covered.
   - Children are enqueued like any object; children that are themselves JSON recurse through the same step.
3. **Links belong to the CID, not to the asset.** A JSON object's content never changes, so its links are computed
   once: 10k assets sharing one metadata JSON cost one parse. A dynamic asset that switches roots simply reaches a
   different subgraph, and nothing per asset needs invalidating.
4. **Bounds.**
   - at most 3 hops from the chain-level ref (`objects.depth` keeps the minimum seen)
   - at most 32 links per JSON
   - JSON parsed only up to 1 MiB
   - a CID that is already known is not enqueued again, so cycles (A → B → A) end by themselves

   Children inherit the parent's `creator` and lane, so a hostile JSON's fan-out counts against its creator's fair
   share (Ingest scheduling).
5. **Both regions derive the same graph** from the same bytes with the same code. Links don't need replicating;
   anti-entropy only compares objects.
6. **Every object gets a key.** The recursive view `asset_objects` (refs → aliases → links, at most 3 hops) gives
   each object its full key, e.g. `mainnet-v1.0/1234567/url/properties/file_url → bafy…`. That key is the
   catalog's "key" for children, and it is what feeds `x_asset_meta` (see Scanner). The internal endpoint
   `/internal/assets/<chain>/<asset>` serves it per asset.

### Scanner: rewrite of algoipfssvc (D9)
- **Two cursors in one process**, stored in the scanner's state table:
  - `live` starts at the tip at bootstrap (T0) and follows new blocks (`/v2/status/wait-for-block-after`); it sends
    `src=live`.
  - `backfill` starts at T0−1 and walks down to genesis; it sends `src=backfill`.

  A cursor is checkpointed only after RabbitMQ has confirmed every message below it (publisher confirms). After a
  restart, live catches up from its checkpoint and backfill resumes where it stopped; backfill ends at round 0.
- **Block source.**
  - Live reads from algod.
  - Backfill reads from the fleet block server (blksrv on htz-fi-hel-tmp, every round), with parallel fetchers per
    descending window and messages sent in descending order. At ~1000 rounds/s, 65 M rounds take about a day (M0
    measures).
  - Every block is decoded *including inner transactions*, because apps mint ASAs through inner `acfg`; the created
    asset id comes from ApplyData.
  - The scanner no longer depends on the RabbitMQ `tx` feed.
- **Newest first, once per asset.**
  - Only `acfg` matters. URL, name and unit are immutable after creation; the reserve/manager (ARC-19) and the note
    (ARC-69) change.
  - Because backfill walks down, the first configuration it meets for an asset is the newest. `asset_seen(asset,
    newest_round)` makes it resolve each asset once, from the asset's current params (algod `/v2/assets/<id>`)
    plus that transaction's note, and skip older configs. Destroyed assets are skipped.
  - Live updates `asset_seen` on every reconfiguration.
  - Recently active assets come first in the backfill, and they are the most likely to be requested.
- **No IPFS fetching in the scanner.** It sends chain-level refs only and seapin follows the JSON (Reference graph).
  This removes the 90 s worker stalls, the `ipfs.nodes[0].gw` dependency and the http(s) SSRF.
- **Backpressure.** Backfill pauses while live is more than ~10 rounds behind, and its publish rate is token-bucket
  limited. Seapin's backfill lane absorbs the rest.
- **`x_asset_meta` stays.** Nodely's GraphQL exposes it (`graphql.nft_meta` and `asset.xxx_nft_meta` in the
  indexers' `graphql.sql`). The writer targets the etl ledgerdb table only; logical replication (`ledger_extras`)
  carries it to the 5 indexer regions, so its columns must not change without the subscribers.
  - A compat writer in the scanner consumes seapin-EU's object events (`seapin.events`: `cached` / `blocked` /
    `failed`).
  - On an event it reads `/internal/assets/<chain>/<asset>` and writes the existing JSON shape (`extra.json/img/
    vid/aud/oth` with mime, bytes and cid, plus `content` for JSON ≤ 10 KiB).
  - Discord new-asset notifications are dropped (D10).

### Content policy (applied before storing and again when serving)
- Decide on the type sniffed from the first bytes (`gabriel-vasile/mimetype`, already used by the scanner). Ignore
  kubo's Content-Type, the file extension and `mime_hint`.
- **Allowlist (D3, decided 2026-10-30)**:
  - images: png, jpeg, gif, webp, avif, bmp
  - `image/svg+xml`, always served sandboxed (D4)
  - video: mp4, webm, quicktime
  - audio: mpeg, ogg, wav, flac, aac, mp4
  - 3D: `model/gltf-binary` (GLB, self-contained). A `.gltf` file passes as JSON, but its external `.bin` buffers
    are `octet-stream` and stay blocked.
  - `application/json`

  Everything else is `blocked`: HTML, JS, XML/XHTML, PDF, plain text, wasm, executables (ELF, PE, Mach-O),
  ZIP/7z/RAR/tar/gz, unknown `application/octet-stream`.

  UnixFS directories (kubo answers with an HTML listing or `index.html`) are `blocked` with reason `directory`;
  paths inside them still resolve to leaves.
- **Size cap: 256 MiB** per object (D3); anything larger is `blocked: too_large`.
- Blocked decisions are recorded in the catalog and never fetched again. Serving answers 403, or 451 for takedowns.
- **Headers on every response**:
  - `X-Content-Type-Options: nosniff`
  - `Content-Security-Policy: default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'; sandbox`
  - `Cross-Origin-Resource-Policy: cross-origin`
  - `Cache-Control: public, max-age=29030400, immutable`
  - `ETag: "<cid>"`

  The serve path also re-checks the stored MIME type against the allowlist (defense in depth).

### Ingest scheduling: bounded in-flight work, no head-of-line blocking
**Threat.** An IPFS fetch for a CID with no reachable provider burns its whole timeout (up to 120 s).
- Minting ASAs is cheap, so anyone can queue thousands of unfetchable CIDs.
- The backfill alone will contain many dead historical CIDs.

In one FIFO with fixed concurrency, these fill every slot and new, well-pinned NFTs wait behind them. Under D2 those
NFTs 404 until they are cached.

**Design: fast/slow lanes as worker pools scheduled from Postgres, not as RabbitMQ queues.**

1. **Intake never fetches.** The RabbitMQ consumer upserts the ref plus a `pending` object and acks. The object goes
   to lane `fast`, or `backfill` when `src=backfill`. The queue drains at Postgres speed, so RabbitMQ never holds
   slow work. A CID that is already known costs one upsert, not a fetch.
2. **Each lane is its own worker pool** with its own slot count, so a slow item can only hold a slot in its lane.
   Starting values, tuned from the M0 spike; the total must stay under the sidecar's measured ceiling:

   | lane | takes | TTFB deadline | slots | sources |
   |---|---|---|---|---|
   | fast | new live CIDs | 10 s (≈ p90 TTFB of successful fetches) | 48 | peer → kubo |
   | slow | fast-lane misses and retries | 120 s | 16 | peer → kubo → Pinata |
   | backfill | scanner backfill, anti-entropy, region rebuilds | 10 s, then 120 s on retries | 32 | old gateway during the M4 seed, then peer → kubo |

   Backfill runs the same retry ladder inside its own pool and never takes live slots.
3. **Deadlines measure progress, not total time.** Unfindable content wastes time during provider discovery, so each
   lane's deadline is time-to-first-byte. Once bytes flow, the fetch is aborted on:
   - a 15 s stall
   - a rate below ~32 KiB/s over 30 s (stops trickle attacks)
   - the 256 MiB cap
   - a 10 min hard ceiling

   Cancelling the HTTP request makes kubo drop the want.
4. **Retry ladder.**
   - A new CID gets one fast attempt, then a slow attempt 5 min later.
   - Further slow attempts follow at +1 h, +6 h, +24 h, +3 d and +7 d; after that the CID is `failed`.
   - A new reference re-arms a `failed` CID at most once per 30 days.
   - Dead CIDs leave the fast lane after one attempt.
5. **Fairness by creator.**
   - Each dispatch pass takes at most one item per creator per lane, oldest first.
   - A creator holds at most 4 fast and 2 slow slots at once, so a flood from one account gets one share.
   - Creators whose last 5+ fetches all failed skip the fast lane.
   - Spreading a flood over fresh accounts costs 0.1 ALGO MBR per account plus 0.1 per asset, and buys only a few
     fast attempts per account.
6. **Crash safety.** Dispatch is `SELECT … FOR UPDATE SKIP LOCKED`, woken by `LISTEN/NOTIFY` plus a 1 s poll.
   Each item carries `lease_until` (deadline plus margin), so after a crash it returns to its pool when the lease
   expires.
7. **Signals.**
   - Per lane: pending count, oldest pending age, slot use, outcomes by source.
   - Oldest pending age in the fast lane is the SLO; alert when it passes 5 min.
   - Top creators by failures.

The serve path never waits on IPFS. The peer proxy on a local miss has its own cap (64 in flight, 2 s TTFB) and a
60 s in-memory negative cache of peer misses; when the cap is reached it answers 404.

Why not RabbitMQ queues as lanes: fast/slow/backfill queues with prefetch = slots, plus TTL + dead-letter queues for
the ladder (the scanner's existing pattern), would work. But that gives no dedupe across messages, no per-creator
fairness, one TTL queue per ladder step, and state visible only as queue depth. In Postgres the state is queryable
and survives restarts, and the load is trivial with one ingest per region.

### Capacity (append-only, so this replaces eviction)
- **Disk** (as deployed: vdb at `/mnt/seaweed`, data only; the filer store, needle indexes and Postgres live on the
  local root disk): the 3 TB volume is formatted whole-device (no partition table) as XFS and mounted at `/data`. It holds
  the SeaweedFS data, the filer store and Postgres; kubo's GC'd repo stays on the root disk. Growing is then "resize
  the volume at the provider, then `xfs_growfs /data`": online, no restart, up to 8 TB.
- **SeaweedFS picks up the new space**: with `-volume.max=0` the volume server recomputes its free volume slots from
  free disk space on each heartbeat. Prove this on the pinned version with the M4 resize drill (`/dir/status` max
  grows with no restart).
- **Triggers**:
  - grow at 70% used (cheap, so grow in large steps)
  - alert at 85%
  - at 70% of 8 TB, plan the next step (a second volume as an extra `-dir`, or a second volume server)
  - track bytes/day from `objects.size` and project the date 8 TB is reached (Zabbix)
- At `minFreeSpace` ingest pauses: the queue backs up (bounded, see §2) and serving keeps working.
- Growth levers that respect append-only: the allowlist, the size cap and storing only what the scanner references
  (D2).

## 2. Replication: replicate the intent, not the bytes

**Recommendation: two independent caches fed by the same event stream, with peer-first fetching and a daily
anti-entropy pull. No SeaweedFS-level coupling between regions.** Because storage is append-only, each region's
catalog is a grow-only set. Convergence is a set union: nothing conflicts and there are no deletes to propagate.

1. **Fan-out.** The scanner publishes to a durable fanout exchange `seapin`. Each region has its own durable queue
   (`seapin.eu`, `seapin.us`) on the existing RabbitMQ cluster, with `x-max-length` (e.g. 10 M, `drop-head`) so a
   region that is down for a long time cannot exhaust RabbitMQ. A region that is down catches up from its queue when
   it returns. The US consumes over the tailnet.
2. **Peer-first fetch.** Ingest tries the other region before kubo: one HTTP GET over the tailnet instead of DHT or
   bitswap discovery, and a CID that is hard to find on IPFS only has to be found once. Optionally the US ingest
   holds messages for ~10 min (TTL + dead-letter delay queue, a pattern the scanner already uses), so most US copies
   come from the EU peer rather than a second IPFS fetch.
3. **Anti-entropy, daily.** Each region pages through the peer's catalog (`GET /internal/catalog?since=<watermark>`,
   ordered by `cached_at, cid`) and enqueues any CID or alias it lacks. This covers messages dropped by max-length,
   bugs and outages. The same endpoint rebuilds a lost region from scratch.
4. **Metadata needs no Postgres replication.** Both catalogs are built from the same messages; anti-entropy covers
   the rest. Each Postgres is a disposable sidecar that can be rebuilt from the peer, and `x_asset_meta` on etl
   stays the scanner's own record.
5. **Each region applies the content policy itself.** A policy bug in one region therefore cannot copy bad bytes
   into the other.

Failure modes:
- A region is down: its queue grows and it catches up later.
- RabbitMQ is down: both regions keep serving; new CIDs wait.
- The transatlantic link is down: each region fetches from kubo.
- A disk is full: that region's ingest pauses and serving continues.
- Messages are lost: anti-entropy fills the gap within a day.

**Why not `weed filer.sync`** (SeaweedFS's built-in asynchronous, resumable, active-active cross-cluster sync)? It
needs no code, but here it has four problems:
- It replicates bytes only, so the Postgres catalog would still need the event stream. That means two replication
  mechanisms instead of one.
- It exposes filer gRPC and volume or filer ports across the WAN (mTLS via `security.toml`).
- Bytes stored by one region would land in the other without passing that region's policy check.
- It is another long-running process with its own checkpoint state.

It remains useful for a **one-off bulk seed** of a new region (active-passive, run once, then stopped) if
peer-fetching a few TB through seapin turns out to be too slow.

## 3. Milestones

M1 and M3 can run in parallel once M0's contract is frozen; M3's `x_asset_meta` compat writer needs M2's events.

### M0: ground truth and decisions
- [x] Size the referenced set (`x_asset_meta` on etl ledgerdb, read 2026-10-30 20:03 UTC):
      - 6.06 M assets → **3.91 M distinct root CIDs** (4.08 M CID+path objects), **1.88 TB** by Content-Length
      - by class: img 1.37 M objects / 1,659 GB; vid 8.3 k / 206 GB; oth 8.6 k / 9.7 GB; aud 1 k / 3.8 GB;
        json 2.70 M / 1.0 GB
      - png + jpeg + mp4 + gif are 96% of the bytes
      - growth from new ASAs: ~350 objects and 0.10–0.14 GB per day (365 days: 36.6 GB)
      - **3 TB at go-live holds the full backfill with ~1.1 TB headroom**; ASAs alone would take decades to reach
        8 TB
      Data and queries: `/home/urtho/dev/tmp/seapin-m0/`. — 2026-10-30
- [x] Demand vs. the referenced set (`nodely.apilog`, 7 days to 2026-10-30 00:00 UTC):
      - 2.34 M requests, 30% of them 403 referer rejections; 603 k distinct CIDs
      - CIDs no ASA references: **20.6% of requests, 21.6% of today's successful ones, 16.2% of distinct CIDs**,
        but only 3.2% of bytes
      - 45% of those requests come from Pera; the top ones are dead CIDs that fail today too; the rest is a long
        tail (17.8 k CIDs requested once)
      - **Strict D2 would 404 about one in five requests that succeed today**, so D2 needs revisiting (§4). The
        98 k out-of-set CIDs are in `seapin-m0/apilog_out_cids.tsv` for M3's gap hunt. — 2026-10-30
- [x] Fetch sidecar spike (operator: "test rainbow first - I hate kubo"). Setup: rainbow v1.24.1, outbound
      only; 1,000 objects sampled from `x_asset_meta`; 32 parallel; 46 min.
      - **recent 90 days: 95.0% fetched** (img 96.8%, json 93.3%), first byte p50 0.64 s / p90 1.77 s
      - all-time: 28.0% (img 63.5%, json 10.9%)
      - of all successes, 407/414 had the first byte within 10 s (the fast-lane cutoff holds); the rest within
        120 s (the slow lane)
      - rainbow peaked at 343 MiB RSS; it has the same `X-Ipfs-Roots`, ETag, "no link named" and `DirIndex-`
        behaviour as kubo, so ingest needed no change
      - **Why old content fails:** its provider records point at Filebase, whose trustless gateway answers 404, so
        the content is gone from the network. 146 of 150 sampled all-time failures (97%) are still in the old
        gateway's caches on ipfs-1/2.
      - So roughly 7 in 10 historical objects survive only in our caches, which makes the M4 seed from ipfs-1/2
        the most valuable migration step.
      Data: scratchpad `spike/`. — 2026-10-30- [x] Freeze message contract v1: `internal/contract` (ref, takedown, object event) with validation and tests. — 2026-10-30
- [x] D1 settled: rainbow (§4). — 2026-10-30
- [ ] Settle D6 (§4).
- [x] How the indexers get `x_asset_meta`: logical replication, publication `ledger_extras` on the etl ledgerdb
      → 5 subscriptions (euc, eud, usg2, usf, sgc). The etl GraphQL reads it directly. Other readers on etl: views
      `graphql.asset`/`asset_meta`/`nft_meta()`, `ch.asset(_live)`, `xv_asset_cids`, the public Grafana IPFS
      panels. The fdw `ipfsmeta` is a dead leftover. RabbitMQ `an.hdr.ipfs.log` is bound only to `an.ipfs.db`
      (no static/discord queues). — 2026-10-30
- [x] blksrv throughput (operator: "go wild (32)"):
      - setup: 32 parallel msgpack block reads on htz-fi-hel-tmp itself (127.0.0.1:3889), cold deep-history rounds,
        host already at load 6–13 on 16 cores
      - results: 937 / 737 / 848 rounds/s at rounds 15 M / 35 M / 55 M (blocks 41 / 125 / 81 KB, 38–92 MB/s,
        p95 35–70 ms, 12,000 of 12,000 OK)
      - **a full 66.5 M-round backfill takes about a day**; network to the EU VM comes on top
      - a second mainnet blksrv runs on net-us-ash-usj — 2026-10-30
- [x] Registry: seapin `status: active` — 2026-10-30 (`_registry` 94248c4).
- [ ] Registry `deps` (RabbitMQ, algod, blksrv, the etl Postgres for `x_asset_meta`), each added when the code starts using it (M2/M3).

Exit: the decisions table is filled in and the capacity numbers are recorded here.

### M1: serve path, hardened (fresh code) — done 2026-10-30, branch `m1-serve-path`
- [x] New layout replacing `main.go` (Go 1.27): `cmd/seapin` (`serve`, `migrate`, `seed`, `health`, `version`)
      and `internal/{config,cidkey,policy,store,catalog,serve}`. `fetch` and `ingest` arrive with M2. — 2026-10-30
- [x] No `POST /upload` (A1, A2) and no MinIO defaults; endpoints and credentials required from env. — 2026-10-30
- [x] CID validation (identity multihashes refused) and normalization to CIDv1 base32; `/ipfs/{cid}/{path...}` with
      alias lookup (A3, A4). — 2026-10-30
- [x] Local miss → 404 with `Cache-Control: max-age=60` (D2); nothing on the serve path talks to IPFS. — 2026-10-30
- [x] `http.ServeContent` over ranged S3 GETs (Range, HEAD, If-None-Match), `r.Context()`, server timeouts, graceful
      shutdown (A6–A8). — 2026-10-30
- [x] Security headers, serve-time allowlist re-check, 403 for blocked, 451 for takedowns (A5). Takedowns are held in
      memory and refreshed every 60 s, so hits never wait on Postgres; `serve` won't start until the list has loaded
      once. — 2026-10-30
- [x] Sharded key layout; `/health` (503 only when S3 is down; Postgres state in the body); Prometheus `/metrics`.
      — 2026-10-30
- [x] Dev/reference compose: SeaweedFS 4.45 pinned with explicit `volumeSizeLimitMB` and `minFreeSpace`, Postgres
      18.6, host network on 127.0.0.1, `read_only`, `cap_drop: [ALL]`, `no-new-privileges`, uid 1000, `mem_limit`,
      capped logs. Separate S3 identities: serve read-only, ingest read/write/admin. One-shot `seapin-migrate`. —
      2026-10-30
- [x] Tests: unit (normalization, policy, Range/HEAD/304, miss, blocked, takedown, alias; `-race`), catalog against
      real Postgres (env-gated), `scripts/dev-smoke.sh` end to end. — 2026-10-30

Exit (met 2026-10-30): on the dev stack a seeded PNG serves 200, 206 with the right bytes, 304 and HEAD. HTML is
refused at seed time (403, catalog row `blocked not_allowed:text/html`). HTML planted in the bucket behind the
policy's back also gets 403. A takedown gives 451, an unknown CID 404 (60 s), and the serve identity's PUT 403. Unit,
race and catalog tests pass.

### M2: catalog, ingest worker and fetch sidecar — done 2026-10-30 (dev), branch `m1-serve-path`
- [x] Postgres schema and migrations (§1): `0001_init.sql` with M1. `0002_ingest.sql` adds the work queue
      `fetches` (one row per root CID + path; the leaf is only known after the fetch) and the
      `asset_objects(chain, asset)` function. — 2026-10-30
- [x] Intake: consumes `seapin.<region>` (declares the fanout exchange and the queue with `x-max-length` +
      `drop-head`), applies refs (newest round per key wins, superseded rounds queue nothing) and takedowns, then
      acks. It never fetches. — 2026-10-30
- [x] Scheduler: one worker pool per lane; fair claims in SQL (one per creator per pass) plus per-creator caps
      (fast 4, slow 2); leases outlast the deadlines; retry ladder fast → slow (+5 min … +7 d) → `failed`; live
      demand promotes waiting backfill fetches; a failed fetch is re-armed at most once per 30 days. — 2026-10-30
- [x] Fetch chain: peer (404 = not here) → kubo → Pinata (slow lanes only). Deadlines measure progress: first byte
      per lane, then a 15 s stall, 32 KiB/s after 30 s, a 10 min ceiling. One fetch per root+path via the table's
      key and lease. — 2026-10-30
- [x] Sniff the first 4096 bytes before anything reaches S3; then stream with the 256 MiB cap. S3 multipart uses
      8 MiB parts, one thread, so concurrent uploads stay bounded in memory. — 2026-10-30
- [x] Directory and path resolution: leaf CID from `X-Ipfs-Roots` (last) or a CID ETag → `aliases`. Directory
      listings (ETag `DirIndex-`) are blocked as `directory`. — 2026-10-30
- [x] JSON link extraction (`internal/arcmeta`: any explicit IPFS link, well-known ARC fields first, ≤ 32) into
      `links`. Children are queued within 3 hops, inheriting creator and lane. `/internal/assets/<chain>/<asset>`
      and `/internal/ipfs/` are served on the token-protected internal listener. Outcome events go to
      `seapin.events` (`<region>.<status>`). — 2026-10-30
- [x] Read-through lane `demand` (D2 revised; `0003_demand.sql`): serve queues the fetch and NOTIFYs ingest, which
      LISTENs and wakes the lane; 32 slots, 10 s first byte, ≤ 8 slots per site; failures go down the slow-lane
      ladder. Dev: an unreferenced PNG was served 0.26 s after a whitelisted request. — 2026-10-30
- [x] Kubo sidecar: `deploy/kubo/001-seapin.sh` (loopback gateway/API, `Routing.Type=autoclient`,
      `Provide.Enabled=false`, 50 GB GC'd blockstore, kubo's own timeouts above ours); the dev stack runs it
      offline. — 2026-10-30
- [x] Metrics: fetches by lane/outcome, duration histogram, stored bytes by source, in-flight, pending and oldest
      due per lane, queue messages by result. — 2026-10-30

Exit:
- Met on dev, 2026-10-30:
  - metadata JSON → image + file inside a directory cached; HTML 403; nothing refused is stored
  - re-published refs cost no fetch
  - unfetchable CID → slow lane, next try +5 min
  - events and `/internal/assets` correct (`scripts/dev-smoke-ingest.sh`)
  - flood test against real Postgres: 300 hanging CIDs from one creator, another creator's CID cached 56 ms after
    it was queued
- Still open: the replay of 10k *real* references needs M3's scanner and an online kubo; it runs with the M0 spike.

Found while building M2:
- kubo answers 404 both for a path missing from a resolved directory (`no link named …`, final) and for a block
  it cannot get (`block was not found …`, may appear later). Only the first is treated as permanent.
- RabbitMQ 4 refuses transient non-exclusive queues; every queue we declare is durable.
- On host networking, RabbitMQ's Erlang epmd and distribution ports listen on all interfaces unless
  `ERL_EPMD_ADDRESS`, `RABBITMQ_DIST_PORT` and `inet_dist_use_interface` pin them (dev compose does). Production
  RabbitMQ is the existing cluster; this only matters for a local broker.

### M3: scanner rewrite (§1 Scanner; replaces algoipfssvc) — built and tested offline 2026-10-30, branch `m1-serve-path`
- [x] Block reader `cmd/seapin-scan`: algod REST for live; the same API for backfill (point `BLOCKS_URL` at
      blksrv); inner transactions walked; created asset ids from ApplyData; blocks from another genesis id
      refused. — 2026-10-30
- [x] Two cursors in `scan_state`: bootstrap at the tip, live upwards, backfill down to the floor (genesis by
      default) in windows fetched in parallel. A cursor only moves after RabbitMQ has confirmed the refs. Backfill
      pauses while live is >10 rounds behind; refs/s budget. — 2026-10-30
- [x] Resolver shared with seapin (`internal/arcmeta`): `ipfs://`, `/ipfs/`, `*.ipfs.*`, ARC-3 `{id}`, ARC-19 with
      the named address field, ARC-69 notes; CIDs validated; http(s) dropped. — 2026-10-30
- [x] `scan_assets`, newest first:
      - configurations are ordered by (round, position in block); one block may create and reconfigure an asset
      - the backfill reads each block newest first, so older configurations are skipped
      - a reconfiguration seen before the creation keeps its addresses until the creation supplies the URL
      - live asks algod for the URL in that case
      - destroyed assets produce nothing

      — 2026-10-30
- [x] Publish chain-level `ref` messages (contract v1) with publisher confirms. — 2026-10-30
- [ ] Any gaps M0 finds. Under D2, anything not reachable from a chain-level ref is never served.
- [x] `x_asset_meta` compat writer: polls `/internal/assets` for assets with new refs, waits up to ~2 h for
      pending objects, then writes the old JSON shape (`id, an, un, r, url, rnd, extra.{json,img,vid,aud,oth}`).
      — 2026-10-30
- [ ] Runs beside algoipfssvc, which keeps writing `x_asset_meta` until the compat writer is verified (deploy, M4).
      At M6 algoipfssvc, its `static` worker and Redis are retired.

Exit status 2026-10-30:
- Offline:
  - full cursor run against a fake algod serving real msgpack blocks: bootstrap at tip 1000, backfill 999→975
    newest first, live 1000→1002, each round fetched once, cursors persisted
  - asset ordering and ARC-19/69 cases on real Postgres
  - publisher confirms through the dev RabbitMQ, picked up by dev ingest
- Mainnet, 2026-10-30 20:36 UTC, through Nodely's public API (`ALGOD_REFERER` in `.env` to get past the daily
  free quota):
  - bootstrapped at round 66,487,569; backfilled 2,000 rounds in 28 s (4 fetchers); live followed with lag 0
  - 737 assets configured in the window (258 destroyed) → 26 refs, in line with M0's ~350 new objects/day
  - **24 of the 26 refs carry exactly the root CID `x_asset_meta` holds for the asset, 0 differ**; the other 2
    assets are newer than the 20:03 UTC snapshot
  - for the full backfill use our own algod/blksrv
- Not yet: the 1k-asset `x_asset_meta` comparison (needs mainnet data, M4).

Found while building M3:
- `freeze` is a reserved word in Postgres; the address columns are `*_addr`.
- The bootstrap insert used one parameter as both a column value and in `$2 - 1`; Postgres refuses (42P08). The
  offline cursor test caught it before any real start.
- A failed `Run` left `main` waiting on the compat writer, so the error was never logged (seen as a silent hang
  on the quota 403). `main` now cancels the shared context, and bootstrap retries algod with backoff instead.

### M4: EU region live, shadow traffic
**Hosts.** Provisioned 2026-10-30 by algoup and handed to seapin; record `algoup deploy/seaweed-nft/README.md`,
registry service `seaweed-nft`.

| region | host | address | machine |
|---|---|---|---|
| EU | **net-de-nur-ipfs** | 62.83.39.60, NetCup Nuremberg | 8 vCPU, 15 GB RAM, 251 GB root (local) |
| US | **net-us-ash-ipfs** | 159.195.232.249, NetCup Ashburn | same |

Both reach ssh only through 4160mgmt. Each has a 3 TB netcup block device `vdb` as whole-device XFS at
`/mnt/seaweed`, growable online to 8 TB.

- [x] VM, disk and XFS, docker, and a standalone SeaweedFS stack in `/home/docker/seaweed` (`4.45_large_disk`,
      100 GB volumes, replication 000, `-volume.max=0`, `minFreeSpace=100GiB`, telemetry, iceberg and lance off,
      loopback only). Zabbix base templates and UFW (ssh from 4160mgmt, agent from the Zabbix servers). Checked
      live 2026-10-30 23:3x CET: Max 30 / Free 23 slots, no buckets yet, only :22 and :10050 public. — 2026-10-30
      (algoup)
- [ ] Take over the seaweed stack: an S3 identities file (serve read-only, ingest read/write), bucket `ipfs`, a
      healthcheck across master/volume/filer/S3, and **`mem_limit` 12g → about 6g** (15 GB host; the
      leveldb-backed needle index needs little RAM). Postgres, rainbow, seapin and openresty need the rest.
- [ ] Stacks per **nodely-deploy**: Postgres (on the local root disk, beside the filer store; vdb is network block
      storage), rainbow (`deploy/rainbow`), `seapin serve`, `seapin ingest`, and in the EU `seapin-scan`, plus
      openresty per D5 with the two `X-Seapin-*` headers. Secrets go in the vault and `.env` 0600.
- [ ] Network across providers. The NetCup hosts have no VLAN to Hetzner, so RabbitMQ, blksrv (htz-fi-hel-tmp),
      the etl Postgres (`x_asset_meta`) and the legacy caches on ipfs-1/2 are reached over the tailnet or allowlisted
      TLS endpoints (**nodely-expose**, **nodely-data**). The internal listener carries peer fetches between the two
      hosts.
- [ ] Filer store backup. It lives on the root disk and is not rebuildable: losing it orphans every stored object
      until anti-entropy re-fetches them from the peer region. Back it up nightly (`weed filer.meta.backup` or a
      leveldb snapshot), or accept that and document it.
- [ ] Seed from the old caches on ipfs-1/2 (2.3–2.4 TB).
      - On both hosts, add an internal openresty listener (VLAN/tailnet only) that passes
        `Cache-Control: only-if-cached` to kubo and returns 412 on a miss.
      - Point ingest's `LEGACY_URL` at it and run the M3 backfill.
      - This is where most historical content comes from: the M0 spike found ~7 in 10 all-time objects are gone from
        the network, and 97% of those are in these caches.
- [ ] Zabbix (health, disk 70/85 %, queue depth, ingest errors, projected date for 8 TB); apilog onboarding; deploy
      record; registry deploy row.
- [ ] Resize drill: grow the volume a little online, run `xfs_growfs`, confirm SeaweedFS reports more free slots
      with no restart.
- [ ] Shadow comparison against the current gateway on sampled requests (status, type, bytes).

Exit: EU answers at least 99% of sampled referenced-CID requests from cache, the comparison shows no new content
type leaking through, and the share of requests that will 404 (unreferenced CIDs) is measured and accepted.

### M5: US region and asynchronous replication
- [ ] Same stack in the US; `seapin.us` queue; peer endpoint over the tailnet with a token; optional US delay queue.
- [ ] Serve path: on a local miss, proxy from the peer region's cache when it has the CID, and nudge local ingest (D2).
- [ ] Daily anti-entropy both ways; `takedown` messages honoured in both regions.
- [ ] Drill: stop EU ingest for 1 h while publishing, restore, confirm convergence. Rebuild a scratch region from the
      peer's catalog.

Exit: the catalog diff between regions stays under 0.1% of CIDs 24 h after the drill.

### M6: cutover and retirement
- [ ] Geo steering with health checks (D6); referer whitelist carried over in openresty (D5); canary on a
      `*-nobunny` hostname first, then full traffic. The old gateway stays ready for a switch back for 2 weeks, but
      is not a fallback upstream (that would reopen D2).
- [ ] Retire kubo, ipfs-cluster and the `static` worker on ipfs-1/2 (**nodely-decommission**), **only after the
      legacy seed is verified**: both regions hold ≥ 99% of the objects `x_asset_meta` lists. Until then those caches
      are the only copy of most historical content.
- [ ] Pinata review (D7): keep it only if it delivered objects the IPFS network couldn't.
- [ ] Runbook for going past 8 TB written down (a second volume as an extra `-dir`, or a second volume server).

Exit: the old gateway receives no traffic for 2 weeks; fill rate tracked against the 70% grow trigger.

## 4. Decisions

Decided by the operator on 2026-10-30:

| # | decision | choice | consequence |
|---|---|---|---|
| R | Replication | event fanout + peer-first fetch + daily anti-entropy (§2) | no SeaweedFS coupling between regions |
| K | Catalog key | `<chain>/<asset>/<where the reference sits>` (§1) | one key per reference; the IPFS sub-path is a separate column |
| D2 | Request for a CID that is not cached (revised 2026-10-30 after M0) | **read-through for whitelisted referers, strict 404 for everyone else**. openresty marks whitelisted requests with `X-Seapin-Whitelisted: 1` (and `X-Seapin-Site`); seapin queues the fetch in the `demand` lane (fair per site) and waits ≤ 20 s, else 504 + `Retry-After`. A CID whose first attempt failed answers 404 at once while the ladder retries it in the background. Read-through objects are cached (their JSON links are recorded, not followed). Peer-region proxying during replication lag stays | covers the 21.6% of today's successful requests no ASA references; growth stays small (≤ ~3.4 GB/day at today's served bytes); dead CIDs cost one fetch, not one per request |
| D3 | Allowlist and size cap | §1 list plus SVG and GLB; PDF and plain text blocked; 256 MiB cap | larger objects are recorded as `too_large` |
| D4 | SVG | allowed, always served with CSP `sandbox` + `nosniff` | scripts inside an SVG cannot run |
| D5 | Front proxy | keep openresty (whitelist, apilog `$flg`) | seapin listens on 127.0.0.1 only. openresty must **always set** `X-Seapin-Whitelisted` (`$allowed_referer` → `1`/`0`, overriding any client value) and `X-Seapin-Site` (referer host, or a name for UA-whitelisted apps such as Pera). Whether non-whitelisted referers keep today's 403 for cached content or get cached-only access is openresty's choice; seapin works either way |
| D7 | Pinata | last ingest fetch source until M6, then review | its token moves to seapin's `.env`; rotation pending (§0) |
| D1 | Fetch sidecar | **rainbow** v1.24.1 (operator preference, confirmed by the M0 spike: 95% of recent CIDs, first byte p50 0.64 s, 343 MiB) | flags in `deploy/rainbow/README.md`; kubo stays only as the dev stack's offline test-content source |
| D9 | Where the new scanner lives | `cmd/seapin-scan` in this repo, sharing `cidkey`, `arcmeta` and the contract types | algoipfssvc is retired at M6 |
| D10 | Discord new-asset notifications | dropped | they stop when algoipfssvc is retired |
| L | Which JSON strings become child CIDs | any explicit IPFS link (`ipfs://`, `/ipfs/`, `*.ipfs.*`) anywhere in the JSON; ≤ 32 per JSON, ≤ 3 hops, 1 MiB parse limit; bare CIDs ignored | covers collection-specific layouts; the bounds and per-creator fairness contain hostile fan-out |

Still open (recommendation first):

| # | decision | recommendation | why |
|---|---|---|---|
| D6 | Region steering | Cloudflare Load Balancer geo steering + `/health` monitors | failover without DNS TTL waits; check how it fits behind the current CDN (Bunny, see the `/bunny` check and `-nobunny` hosts) |
| D8 | "Upload date" | block time of the first referencing txn (`first_seen_at`), plus `cached_at` | the chain time is what "uploaded" means for an ASA |