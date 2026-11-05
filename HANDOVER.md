# seapin handover (2026-11-05 07:10 CET)

For the next agent picking up seapin. Facts as of the time above; verify live state before acting.

## What seapin is now

seapin is Nodely's public IPFS gateway, **in production since 2026-11-04**. The old gateway (kubo + ipfs-cluster +
svc-asset on htz-hel-ipfs-1/2) is **retired and its data deleted**, so there is no rollback to it.

- **Regions:**
  - EU: `net-de-nur-ipfs` (62.83.39.60);
  - US: `net-us-ash-ipfs` (159.195.232.249).
  - Each runs the compose stack `/home/docker/seapin`: seapin serve, ingest, scan (EU only), rainbow, rainbow-fil
    (Filecoin), Postgres catalog, SeaweedFS cache, openresty face on :8880, `health80` (Caddy on :80), and `fbit` (log
    shipper, `/home/docker/fbit`).
- **Bunny:** pull zone **6928561** (`9211e9e3-9.b-cdn.net`), origin **linked** to the monitored, geo-routed DNS record
  set `ipfs-geo.algonode.xyz` (both faces, HTTP monitor `GET :80/`), so Bunny fails over between the regions by itself.
  - Hostnames: `ipfs.algonode.xyz` (Bunny Pull Zone record), `ipfs.algonode.dev`, `ipfs-pera.algonode.dev`,
    `ipfs-pera.algonode.xyz`.
  - Edge rules: the original 4 plus `seapin origin key` (the header `X-Nodely-Origin-Key`; the faces answer 403 without
    it).
  - Settings: copied from the deleted zone 932530.
- **Access logs:** fluentd driver → host fbit → Graylog + ClickHouse `nodely.apilog` (`api ipfs`, `lb seapin`). Bunny's
  own log forwarding (`lb bcdn`) goes to the fbit on ipfs-1:25140.
- **x_asset_meta:** the compat writer in seapin-scan (EU) writes ledgerdb `x_asset_meta`. svc-asset is gone.
- **Fetch sources** (ingest, both regions): peer region → Filecoin (rainbow-fil, one storage provider) → sidecar
  (rainbow, DHT/bitswap) → Pinata (dedicated gateway, slow lanes). No legacy caches; Filebase was tried and dropped.

## Where things are

| What | Where |
|---|---|
| Plan, decisions, milestones | `/home/urtho/dev/seapin/PLAN.md` (§5 pinning, M6 status, M7) |
| Code (Go) | `/home/urtho/dev/seapin` (`cmd/seapin`, `cmd/seapin-scan`, `internal/*`), branch `m1-serve-path`, pushed |
| Deploy, ops | algoup `/home/urtho/dev/_production/algoup/deploy/seapin/` |
| Rollout | `rollout.sh <eu|us> <step>` (steps in its header; `face`, `fbit`, `ship`, `up`, `verify`, ...) |
| Bunny | `bunny_switch.py status|diff|drain eu|us|undrain eu|us|rollback|restore` (rollback now has no target: the old gateway is gone) |
| Old-gateway retirement log | `RETIRE-LEGACY.md` ("Final state 2026-11-04", "Retired 2026-11-04"), `retire_legacy.sh` |
| Pin nodes (M7) | algoup `deploy/seapin-pin/` (`rollout.sh up 1|2`, `peering`, `status`) |
| Bunny multi-origin recipe + traps | skill nodely-expose `bunny-geo-failover.md` |
| Referer whitelist | algoup `deploy/seapin/openresty/conf.d/05-whitelist.conf` (skill nodely-whitelist; `rollout.sh <region> face`) |
| Zabbix | algoup `tools/zabbix/zbx_seapin.py` (templates "Nodely seapin region" / "scanner") |
| Secrets | KeePassXC through nodely-tools `kpxc.py` (`fleet://bunny/api`, `fleet://seapin-bunny-origin`, ...); local copies in `/home/urtho/dev/tmp/seapin-m4/` (0600). Never print them. |
| Registry / deploy log | `/home/urtho/dev/_registry` (`tools/reg.py deploy seapin ...`) |

Hosts: the faces are reached with `ssh -J urtho@4160mgmt.hosts.algonode.net root@<ip>`; ipfs-1/2 with
`ssh -F algoup/inventory/ssh_metal.config htz-hel-ipfs-1|2`. Catalog: `docker exec seapin-postgres psql -h 127.0.0.1
-p 45435 -U $POSTGRES_USER -d $POSTGRES_DB` (read user and db from the container's env).

## Open items

1. **`ipfs-pera.algonode.xyz`:** Bunny answers "508 Loop Detected" without contacting the origin. It started with the
   loop of 2026-11-04 ~18:00 CET and survived zone deletion, re-adding the hostname and a purge. Its DNS record
   (algonode.xyz, CNAME, id 32977139) is **disabled** by the operator.
   - Needs a Bunny support ticket: request id `730dcbe0aac3a800c0ba3a1c55bd1d99`, 2026-11-04 18:03:34 UTC.
   - Traffic is negligible: 1–5 requests/h, root redirects only.
2. **EU slow lane growth:** 2,638 pending at 07:05 CET, from ~45 the day before.
   - Customer requests for content no source has, now all landing on seapin, at ~100–360 new per hour.
   - Not an error; watch that it levels off.
   - The US has 81.
3. **Backfill:** ~5.9 k (EU) / ~8 k (US) objects no source has. Their make-up:
   - one issuer's signed documents (~43%);
   - NFD metadata (~32%), for which only TxnLab (NFD) holds the content;
   - destroyed assets (~12%).

   The operator has not decided whether to stop retrying them.
4. **Pinata gateway token rotation:** listed in algoup `backlog/secrets-rotation.md`. Don't raise it (operator rule:
   leaked or retired secrets are tracked there, not asked about).
5. **Docs naming ipfs-1/2 as the gateway:** skills nodely, nodely-find, nodely-deploy, nodely-expose, nodely-data,
   nodely-clickhouse, nodely-decommission, and algoup `CLAUDE.md`. Grep `htz-hel-ipfs|ipfs2-nobunny`.
6. **Leftovers:**
   - empty RabbitMQ exchanges `an.hdr.ipfs.log` and `an.dir.dlx` on vhost mainnet (rabbitmqctl cannot delete them);
   - Bunny canary zone `seapin-canary` (6921042, `ipfs-canary.algonode.dev`): delete when no longer useful.

## M7: pinning service (started 2026-11-04, operator go)

Design: PLAN.md §5, including "Interface with the portal (agreed with the nodely-portal session, 2026-11-03)". The portal
side is built: nodely-portal `docs/seapin-pinning-api.md` and `docs/pin-quotas-api.md` (v4); `PINNING_LIVE` stays
false until seapin says `/portal/v1` answers.

- **Decided:**
  - P1–P7 (P6 `pinned+entities` with the sweep provider; P7 keep the cache copy);
  - pin nodes on ipfs-1/2;
  - quotas per tier from the portal; `quota_files` counts leaf files;
  - `indexable` per org, off by default; **no-index always wins, including over the cache** (deny feed by CID and
    content sha256);
  - customer API = IPFS Pinning Service API `/psa` + Pinata-style uploads + `/portal/v1` on `pin.nodely.io` (Cloudflare,
    WAF letting `/portal/*` in only from the portal's IPs);
  - access logs to Graylog + apilog before any traffic (rule DEP003).
- **Step 1, done 2026-11-04:** kubo v0.43.1 pin nodes on ipfs-1/2, `/home/docker/seapin-pin`, repo
  `/mnt/lv0/seapin-pin/kubo` (StorageMax 3 TB).
  - Server profile, minus the VLAN 192.168.4.0/24; API 127.0.0.1:5001; gateway 127.0.0.1:8085 with NoFetch; UFW 4001
    tcp+udp open.
  - Peer ids: ipfs-1 `12D3KooWKgLsjeuyUKHNk72USq43g3emZnj8qaciuU9yeJXsC43k`, ipfs-2
    `12D3KooWMesWsUBibecEQkoJcN6Y2dS43vWfhMabM8pNWw88djon`.
  - Peering is configured over the VLAN, but they connect over public IPv6. To do: prefer the VLAN path. Zabbix for the
    pin nodes is also still to do.
- **Next:**
  - **Step 2:** the pin catalog. Postgres on ipfs-1 with a streaming replica on ipfs-2 over the VLAN; one catalog, so
    quota checks are exact.
  - **Step 3:** `seapin pin` in this repo:
    - the `/psa`, Pinata-style and `/portal/v1` APIs;
    - the pin-quotas v4 puller (portal int6 listener, cert pin; or the tailnet, see below);
    - quota reservations, and usage counters per (org, ld, type);
    - pin/unpin fanout on RabbitMQ (rabbit02/03 are local to ipfs-1/2) to both kubo nodes;
    - anti-entropy and GC;
    - `pins/images` + `pins/deny` feeds for nft-vectors (`/home/urtho/dev/nft-vectors`, which must check the deny set
      before every embed);
    - takedowns on pins;
    - the fluentd access log.
  - **Then:** expose `pin.nodely.io`, serve pins through seapin serve (pin-node fallback), the drill, the outside
    `findprovs` check, and tell the Nodely Portal session.
- **New since the design:** the operator's rule of 2026-11-04 opens the **tailnet for service-to-service traffic**
  (skill nodely-m2m, `deploy/tailnet/m2m.yml`). The portal links were agreed as public IP + allowlist; consider moving
  them to the nodely-m2m standard and agree it with the Nodely Portal session (`SendMessage` to "Nodely Portal").

## Rules and lessons from this run (read before touching production)

- **Production, DNS and firewall changes need explicit operator approval.** The auto-mode classifier blocks them;
  when blocked, hand the operator a ready command (`! <command>`) instead of working around it.
- **When the operator asks "tell me what to change", answer and do not act.** Answer short and exactly.
- **Bunny:**
  - never repoint a linked zone through `POST /pullzone` (ignored);
  - never use a zone's own hostname as its URL origin (508 loops, ~1 h 10 min outage on 2026-11-04);
  - snapshot a zone before any change;
  - multi-origin uses monitored `*-geo` records (see the recipe).
- **Gates:**
  - a public API ships its access log to both sinks before it takes traffic (nodely-deploy, RULES DEP003);
  - check a milestone's open items before a cutover.
- **Times:** Europe/Warsaw in replies, UTC in logs.
- **Commits:** by pathspec (other sessions work in algoup), with the Co-Authored-By trailer.
- **Deploys:** record every one with `reg.py deploy`.
- **Questions:** decisions go to the operator through the interactive question tool.
