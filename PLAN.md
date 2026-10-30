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
- **SeaweedFS**: `weed server -filer -s3`, single node, replication `000`, image pinned,
  `-master.volumeSizeLimitMB=30720` set explicitly. With no deletes there is no vacuum, so 100–270 × 30 GB volumes
  (3–8 TB) is fine. `-volume.max=0` derives the volume slot count from free space, so an online disk grow is picked
  up (see Capacity). `-volume.minFreeSpace=100GiB` stops writes before the filesystem fills. S3 identity for seapin
  only, bound to 127.0.0.1. One bucket `ipfs`, keys sharded on the CID tail (`<cid[-4:-2]>/<cid[-2:]>/<cid>`).
- **Only leaf objects are stored**, keyed by their own CID. A request for `<root>/<path>` resolves through the catalog
  (`aliases`) to the leaf CID. This dedups shared files and avoids file-vs-directory name clashes in the filer
  namespace.
- **Fetch sidecar** (ingest only): kubo, pinned, fetch-only and non-caching in practice. Gateway on 127.0.0.1 only;
  `Routing.Type=autoclient` (DHT client, no DHT server); providing disabled (the config key depends on the pinned
  version); `--enable-gc` with a small `Datastore.StorageMax` (~50 GB) and a short `GCPeriod`; resource manager and
  connection manager capped. `ipfs/rainbow`, a stateless gateway with no persistent blockstore, fits "fetch-only"
  even better; compare the two in M0.
- **Ingest fetch chain**: local S3 → peer region (internal endpoint over the tailnet, bearer token) → local kubo →
  Pinata (until the M6 review, D7). Each CID is fetched once at a time (singleflight per CID), with
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
  indexers' `graphql.sql`).
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
- **Disk**: the 3 TB volume is formatted whole-device (no partition table) as XFS and mounted at `/data`. It holds
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
- [ ] Size the referenced set: distinct CIDs and summed bytes by MIME class from `x_asset_meta` (sizes approximate,
      see §0), plus new-ASA bytes/day. Set the go-live size (3 TB, or more right away) and project when 8 TB is
      reached.
- [ ] From `nodely.apilog` (flag `api:ipfs`): what share of today's requests is for CIDs no ASA references? Under D2
      those will 404. If the share is large, find which references the scanner misses and add them in M3 before
      cutover.
- [ ] Spike: kubo vs rainbow on ~1k real CIDs (success rate, p50/p95, RAM); pick the fetch sidecar (D1). Also
      measure: the time-to-first-byte distribution of successful fetches (sets the fast-lane cutoff), the share of
      dead CIDs in a historical sample (sizes the backfill), and the sidecar's concurrency ceiling (caps total slots).
- [ ] Freeze message contract v1; settle D1 and D6 (§4).
- [ ] Confirm how the indexers' GraphQL gets `x_asset_meta` (an fdw to the etl Postgres?) and who else reads it or
      the `log` exchange.
- [ ] Measure blksrv backfill throughput (rounds/s at 8, 16 and 32 parallel fetchers).
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

### M2: catalog, ingest worker and fetch sidecar
- [x] Postgres schema and migrations (§1): shipped with M1 (`0001_init.sql`); the `asset_objects` view comes with link extraction below. — 2026-10-30
- [ ] Intake: consume `seapin.<region>`, upsert ref + object, ack. It never fetches (§1 Ingest scheduling).
- [ ] Scheduler: lanes with their own worker pools, leases, per-creator round-robin and caps, retry ladder,
      TTFB/stall/rate deadlines.
- [ ] Fetch chain (§1) with singleflight and the size cap.
- [ ] Sniff the first bytes, then stream into S3 (multipart for large objects).
- [ ] Directory and path resolution via kubo (`X-Ipfs-Roots` / ETag → leaf CID → `aliases`).
- [ ] JSON link extraction into `links`, child enqueue within the depth and fan-out bounds, `asset_objects` view,
      `/internal/assets/<chain>/<asset>`; object outcome events to `seapin.events`.
- [ ] Kubo or rainbow sidecar compose, fetch-only config.
- [ ] Metrics: queue lag, fetch outcome by source, blocked by reason, bytes stored per day.

Exit: replaying 10k real references fills S3 and the catalog; a second replay fetches 0 bytes; no blocked type ever
lands in S3. With 10k unfetchable CIDs from one creator queued, a well-pinned live CID still caches within about one
fast-lane TTFB.

### M3: scanner rewrite (§1 Scanner; replaces algoipfssvc)
- [ ] Block reader: algod for live, blksrv for backfill (parallel, descending windows); inner transactions; created
      asset ids from ApplyData.
- [ ] Two cursors with confirmed checkpoints: bootstrap at the tip, backfill down to genesis, backpressure.
- [ ] Resolver shared with seapin (`internal/arcmeta`): `ipfs://`, `/ipfs/`, `*.ipfs.*`, ARC-3 `{id}`, ARC-19
      with the named address field, ARC-69 note; CID validation; http(s) dropped.
- [ ] `asset_seen` newest-first dedupe; a reconfiguration sends a new root ref when the named address changes.
- [ ] Publish chain-level `ref` messages (contract v1) with publisher confirms.
- [ ] Any gaps M0 finds. Under D2, anything not reachable from a chain-level ref is never served.
- [ ] `x_asset_meta` compat writer driven by `seapin.events` (needs M2).
- [ ] Runs beside algoipfssvc, which keeps writing `x_asset_meta` until the compat writer is verified. At M6
      algoipfssvc, its `static` worker and Redis are retired.

Exit:
- references for a new ASA reach both region queues within seconds of its block
- backfill progress (current round, rounds/s) is visible
- for a 1k-asset sample, `x_asset_meta` from the new writer matches the old on the fields GraphQL serves

### M4: EU region live, shadow traffic
- [ ] VM with 3 TB NVMe (XFS); stacks per **nodely-deploy** (seaweed, postgres, kubo, seapin serve and ingest,
      openresty per D5); secrets in the vault and `.env` 0600; UFW; tailnet.
- [ ] Seed from the existing ipfs-1/2 kubo repos (2.3–2.4 TB already local) through their peer listener, driven by
      the M3 backfill. No IPFS network round trips for most objects.
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
- [ ] Retire kubo, ipfs-cluster and the `static` worker on ipfs-1/2 (**nodely-decommission**).
- [ ] Pinata review (D7): keep it only if it delivered objects the IPFS network couldn't.
- [ ] Runbook for going past 8 TB written down (a second volume as an extra `-dir`, or a second volume server).

Exit: the old gateway receives no traffic for 2 weeks; fill rate tracked against the 70% grow trigger.

## 4. Decisions

Decided by the operator on 2026-10-30:

| # | decision | choice | consequence |
|---|---|---|---|
| R | Replication | event fanout + peer-first fetch + daily anti-entropy (§2) | no SeaweedFS coupling between regions |
| K | Catalog key | `<chain>/<asset>/<where the reference sits>` (§1) | one key per reference; the IPFS sub-path is a separate column |
| D2 | Request for a CID that is not cached | 404. The only exception is proxying from the peer region's cache during replication lag; unreferenced CIDs are never fetched or stored | the serve path never touches IPFS; scanner coverage decides what users can get (M0 measures the 404 share, M3 closes gaps) |
| D3 | Allowlist and size cap | §1 list plus SVG and GLB; PDF and plain text blocked; 256 MiB cap | larger objects are recorded as `too_large` |
| D4 | SVG | allowed, always served with CSP `sandbox` + `nosniff` | scripts inside an SVG cannot run |
| D5 | Front proxy | keep openresty (whitelist, apilog `$flg`) | seapin listens on 127.0.0.1 only |
| D7 | Pinata | last ingest fetch source until M6, then review | its token moves to seapin's `.env`; rotation pending (§0) |
| D9 | Where the new scanner lives | `cmd/seapin-scan` in this repo, sharing `cidkey`, `arcmeta` and the contract types | algoipfssvc is retired at M6 |
| D10 | Discord new-asset notifications | dropped | they stop when algoipfssvc is retired |
| L | Which JSON strings become child CIDs | any explicit IPFS link (`ipfs://`, `/ipfs/`, `*.ipfs.*`) anywhere in the JSON; ≤ 32 per JSON, ≤ 3 hops, 1 MiB parse limit; bare CIDs ignored | covers collection-specific layouts; the bounds and per-creator fairness contain hostile fan-out |

Still open (recommendation first):

| # | decision | recommendation | why |
|---|---|---|---|
| D1 | Fetch sidecar | kubo pinned and GC'd, unless rainbow wins the M0 spike | kubo is proven here; rainbow is stateless by design |
| D6 | Region steering | Cloudflare Load Balancer geo steering + `/health` monitors | failover without DNS TTL waits; check how it fits behind the current CDN (Bunny, see the `/bunny` check and `-nobunny` hosts) |
| D8 | "Upload date" | block time of the first referencing txn (`first_seen_at`), plus `cached_at` | the chain time is what "uploaded" means for an ASA |